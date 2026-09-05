// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package checkpoint_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// TestAVersion1ResumesLostWeakHashesCostStorageAndNothingElse is the companion
// #378 asked for, and the reason segment_test.go's `WeakHash == 0` assertion is
// allowed to stand.
//
// THE SITUATION. A backup suspended by a build older than #365 has version-1
// checkpoint segments: strong hash, pack, offset, length, and no weak hash.
// Resuming it decodes those tuples with WeakHash == 0 and pipeline.go:419
// pushes every one of them into this run's dedup index —
//
//	dedupIdx.Insert(hasher.ChunkID{WeakHash: t.WeakHash, StrongHash: t.StrongHash}, ...)
//
// — and on success this run FLUSHES that index, bloom included, as the
// repository's own. So the zero is not confined to "that one resumed run": it
// is written into a shared artifact every later backup reads. Nothing anywhere
// in internal/ filters WeakHash == 0, and nothing can: the weak hash is
// computed from chunk bytes the resumed run never reads again.
//
// THE DECISION, with the evidence below as the argument. Accepting v1 segments
// is right and no filter is added, because:
//
//   - A filter cannot restore the information. Skipping the insert entirely
//     would be strictly worse — it would take the entry out of the HASH index
//     too, and restore resolves chunks through that alone (LookupDirect, hard
//     fail on a miss). A bloom-only filter would change nothing an operator can
//     observe: every zero-weak tuple hashes to the same handful of bloom bits,
//     so the whole preload sets those bits once whether there are two tuples or
//     two million.
//   - The damage is a STORAGE cost, not a correctness one, and it is bounded by
//     the four assertions below. That is what a companion test is for: the
//     defect is real, it is priced, and the price is written down where the
//     next person meets the zero.
func TestAVersion1ResumesLostWeakHashesCostStorageAndNothingElse(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")

	// The repository already holds a chunk from an earlier, ordinary backup.
	// It is the control for bound (2): the resumed run must not cost the repo
	// what it already knew.
	preexisting := hasher.Sum([]byte("a chunk the repo already had before any of this"))
	base, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Insert(preexisting, 0, 8, 47)
	if err := base.Close(); err != nil {
		t.Fatal(err)
	}

	// The suspended prefix: eight real chunks, whose bytes we still have here
	// so we can ask what a LATER backup re-reading them would see. A v1
	// segment carries their strong hashes and nothing else.
	const prefixChunks = 8
	prefix := make([]hasher.ChunkID, prefixChunks)
	strongs := make([][32]byte, prefixChunks)
	for i := range prefix {
		prefix[i] = hasher.Sum([]byte(fmt.Sprintf("prefix chunk %d, stored before the pause", i)))
		strongs[i] = prefix[i].StrongHash
	}
	seg, _, err := checkpoint.UnmarshalSegment(v1SegmentBytes([]byte("v1-sidecar"), strongs))
	if err != nil {
		t.Fatalf("a version-1 segment must still parse: %v", err)
	}
	if len(seg.Inserts) != prefixChunks {
		t.Fatalf("the v1 segment decoded %d tuples, want %d", len(seg.Inserts), prefixChunks)
	}
	for i, tup := range seg.Inserts {
		if tup.WeakHash != 0 {
			t.Fatalf("tuple %d decoded a weak hash (%d) — this test is about the case where it cannot, "+
				"and if v1 ever carries one the pin next door is what should change", i, tup.WeakHash)
		}
	}

	// The resume, exactly as pipeline.go does it, followed by the success-path
	// Flush that makes this the REPOSITORY's index.
	resumed, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tup := range seg.Inserts {
		resumed.Insert(hasher.ChunkID{WeakHash: tup.WeakHash, StrongHash: tup.StrongHash},
			tup.PackNumber, tup.StoreOffset, tup.ChunkLength)
	}
	// ...plus the work the resumed run itself does after the checkpoint, which
	// DOES carry a real weak hash.
	afterResume := hasher.Sum([]byte("a chunk this run stored after resuming"))
	resumed.Insert(afterResume, 99, 8, 51)
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}

	repo, err := index.NewDedupIndexReadOnly(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.CloseDiscard()

	// BOUND 1 — RESTORE IS UNAFFECTED. Every prefix chunk resolves by strong
	// hash at the coordinates the v1 tuple named. This is the whole reason
	// accepting v1 segments is right: refusing them would strand the backup,
	// and taking them costs nothing a restore can see.
	for i, tup := range seg.Inserts {
		e, found, err := repo.LookupDirect(tup.StrongHash)
		if err != nil {
			t.Fatalf("prefix chunk %d: %v", i, err)
		}
		if !found {
			t.Fatalf("prefix chunk %d is not in the repository's index — restore resolves chunks through "+
				"LookupDirect alone and hard-fails on a miss, so the resumed backup would be unrestorable. "+
				"THIS is the bound that must never move: the lost weak hash may cost storage, never data", i)
		}
		if e.PackNumber != tup.PackNumber || e.StoreOffset != tup.StoreOffset || e.ChunkLength != tup.ChunkLength {
			t.Fatalf("prefix chunk %d resolves to pack %d offset %d length %d, want pack %d offset %d length %d — "+
				"a zero weak hash must not disturb the coordinates a restore reads bytes from",
				i, e.PackNumber, e.StoreOffset, e.ChunkLength, tup.PackNumber, tup.StoreOffset, tup.ChunkLength)
		}
	}

	// BOUND 2 — THE REPOSITORY DOES NOT LOSE WHAT IT ALREADY KNEW. The resumed
	// run flushes ITS bloom as the repo's, so the question worth asking is
	// whether that flush is a superset or a replacement. A replacement would
	// turn one v1 resume into a repo-wide dedup reset.
	res, err := repo.Check(preexisting)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsNew {
		t.Error("a chunk the repository held BEFORE the resume now reads as new — the resumed run's flush " +
			"replaced the repo's bloom instead of adding to it, which would make one v1 resume a repo-wide " +
			"dedup reset rather than a loss confined to the prefix")
	}
	// And the resumed run's own post-checkpoint work is in tier 1 normally:
	// the loss is the v1 preload's, not the resume path's.
	if res2, err := repo.Check(afterResume); err != nil {
		t.Fatal(err)
	} else if res2.IsNew {
		t.Error("a chunk the RESUMED RUN stored after the checkpoint reads as new — that one carries a real " +
			"weak hash, so a miss here is the resume path broken rather than the v1 tuples' known loss")
	}

	// BOUND 3 — THE PRICE, NAMED. A later backup re-reading the prefix computes
	// the real weak hash, which is not in the bloom, so tier 1 says "new" and
	// the chunk is stored again. Bytes the repo already holds, paid for twice.
	// This is the #365 defect, reproduced, and it is the entire cost.
	tierOneMisses := 0
	for i, id := range prefix {
		res, err := repo.Check(id)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsNew {
			tierOneMisses++
			continue
		}
		// If tier 1 happens to hit (a bloom false positive), tier 2 must give
		// the right answer — which is the point of BOUND 4.
		if _, found, err := repo.LookupDirect(id.StrongHash); err != nil || !found {
			t.Fatalf("prefix chunk %d deduped without resolving: found=%v err=%v", i, found, err)
		}
	}
	if tierOneMisses == 0 {
		t.Error("every prefix chunk was found by tier-1 dedup — that would mean the weak hashes survived " +
			"the v1 round trip after all, and the pin in segment_test.go (and this whole test) is describing " +
			"a loss that no longer happens; delete both rather than leaving them to describe nothing")
	}
	t.Logf("KNOWN COST of a v1 resume: %d of %d prefix chunks miss tier-1 dedup and will be re-stored by "+
		"the next backup that reads them. Storage, not data. (#365 defect, bounded here.)",
		tierOneMisses, prefixChunks)

	// BOUND 4 — THE ZERO CANNOT MAKE A RESTORE READ THE WRONG BYTES. The bloom
	// now has bits set for weak hash 0, so a genuinely new chunk that also
	// hashes weak-zero gets a tier-1 HIT. It must still come back NEW, because
	// tier 2 is keyed on the strong hash alone. If it did not, the zero would
	// be handing a backup somebody else's pack coordinates, and that is data
	// loss rather than a storage bill.
	var impostor hasher.ChunkID // WeakHash 0, a strong hash the repo has never seen
	impostor.StrongHash[0] = 0xAB
	impostor.StrongHash[31] = 0xCD
	res, err = repo.Check(impostor)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BloomHit {
		t.Fatal("a weak-hash-zero chunk did not even reach tier 2 — the preload's zeros are supposed to be " +
			"IN this bloom, so this test is no longer exercising the collision it exists for")
	}
	if !res.IsNew {
		t.Fatal("a chunk the repository has never held deduped against a v1 preload entry on a weak-hash " +
			"collision — the zero would then be handing a backup another chunk's pack coordinates, which is " +
			"data loss and not a storage bill. Tier 2 must resolve on the strong hash alone")
	}
}
