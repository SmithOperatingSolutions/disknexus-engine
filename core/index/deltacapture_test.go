// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// Capturing a run's delta (#357 phase 2, write side).
//
// The delta a backup publishes must contain THIS RUN'S work and nothing else:
// not the entries it inherited from the authoritative index, not the entries
// it merged from another writer's pending delta. Get that wrong in the
// inclusive direction and every writer's delta grows with the repository
// instead of with its own change — the whole-index upload back again, wearing
// a different name. Get it wrong in the exclusive direction and a completed
// backup references chunks nothing can resolve.

func TestACapturedDeltaHoldsThisRunsInsertsAndNothingElse(t *testing.T) {
	dir := t.TempDir()

	// A repo that already holds one chunk (the "authoritative index").
	base := hasher.Sum([]byte("already in the repo"))
	seed, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	seed.Insert(base, 0, 8, 19)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// A run that merges somebody else's pending delta and then does its own work.
	inherited := hasher.Sum([]byte("learned from another writer's delta"))
	mine := hasher.Sum([]byte("this run stored this one"))

	idx, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}

	// ARM CAPTURE FIRST. The fixture used to merge the foreign delta BEFORE
	// CaptureDelta, so nothing was armed while ApplyTo ran and the headline
	// claim — "not the entries it merged from another writer's delta" — was
	// never exercised: changing ApplyTo's insertNoCapture to a plain Insert
	// left engine/core/index and internal/cloudsync both green (#378 item
	// 5). Merging today happens at index OPEN, i.e. always before arming, so
	// this ordering is not a production sequence; it is the CONTRACT, stated
	// so that the day delta amplification does merge mid-run the guarantee is
	// already pinned rather than acquired by luck.
	deltaPath := filepath.Join(dir, "session.delta")
	if err := idx.CaptureDelta(deltaPath); err != nil {
		t.Fatalf("CaptureDelta: %v", err)
	}

	(&index.Delta{Entries: []index.DeltaEntry{
		{StrongHash: inherited.StrongHash, WeakHash: inherited.WeakHash, PackNumber: 1, StoreOffset: 8, ChunkLength: 35},
	}}).ApplyTo(idx)

	idx.Insert(mine, 2, 8, 24)

	// The counter, read while the index is still open: the delta object on
	// disk is patched at close, but capture's own tally is the thing ApplyTo
	// must not have touched, and reading it here says WHICH of the two writes
	// went astray.
	if n := idx.DeltaEntryCount(); n != 1 {
		t.Errorf("capture counted %d entries after one merge and one insert, want 1 — a merged entry that "+
			"lands in this run's delta is an entry this run never stored, and every writer's delta then grows "+
			"with the repository instead of with its own change", n)
	}

	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatalf("the run published no delta: %v", err)
	}
	d, err := index.ParseDelta(blob)
	if err != nil {
		t.Fatalf("parsing the captured delta: %v", err)
	}
	if len(d.Entries) != 1 {
		var gotHashes []string
		for _, e := range d.Entries {
			gotHashes = append(gotHashes, fmt.Sprintf("%x@pack%d", e.StrongHash[:4], e.PackNumber))
		}
		t.Fatalf("the captured delta holds %d entries (%v), want exactly this run's 1 (%x@pack2) — a delta "+
			"that carries inherited entries grows with the repository instead of with the change",
			len(d.Entries), gotHashes, mine.StrongHash[:4])
	}
	got := d.Entries[0]
	if got.StrongHash != mine.StrongHash || got.WeakHash != mine.WeakHash ||
		got.PackNumber != 2 || got.StoreOffset != 8 || got.ChunkLength != 24 {
		t.Fatalf("captured entry %+v does not describe this run's insert", got)
	}

	// The merge still had to WORK — the run must be able to dedup against what
	// it learned. Excluding an entry from the published delta is not the same
	// as dropping it, and a fixture that cannot tell those apart would pass
	// against an ApplyTo that did nothing at all.
	reader, err := index.NewDedupIndexReadOnly(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.CloseDiscard()
	if _, found, err := reader.LookupDirect(inherited.StrongHash); err != nil || !found {
		t.Fatalf("the merged entry is not in the index (found=%v err=%v) — ApplyTo must keep the entry OUT "+
			"of this run's published delta while still putting it IN this run's index; a no-op ApplyTo "+
			"satisfies the delta assertion above and nothing else", found, err)
	}
}

func TestACapturedDeltaIsReadableBeforeTheIndexIsClosed(t *testing.T) {
	// UploadResults runs AFTER the pipeline has closed its index, but ship.go
	// uploads once per backup in a chain with the index still open. Both must
	// see a complete, parseable object.
	dir := t.TempDir()
	idx, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	deltaPath := filepath.Join(dir, "session.delta")
	if err := idx.CaptureDelta(deltaPath); err != nil {
		t.Fatal(err)
	}

	for n := 1; n <= 3; n++ {
		idx.Insert(hasher.Sum([]byte{byte(n)}), uint32(n), 8, 1)
		if err := idx.Flush(); err != nil {
			t.Fatal(err)
		}
		blob, err := os.ReadFile(deltaPath)
		if err != nil {
			t.Fatal(err)
		}
		d, err := index.ParseDelta(blob)
		if err != nil {
			t.Fatalf("delta after %d inserts: %v", n, err)
		}
		if len(d.Entries) != n {
			t.Fatalf("after %d inserts the delta holds %d entries", n, len(d.Entries))
		}
	}
}

// TestAnEncryptedReposDeltaIsEncryptedAtRestAndStillMerges.
//
// A delta IS index: the same chunk hashes and pack coordinates hash-index.db
// holds, in a different shape. So it gets the same treatment bloom.bin and
// hash-index.db get on a repo whose index is encrypted — .enc beside the
// plaintext, plaintext gone once the run is done writing it — and the read
// side has to be able to merge that back.
func TestAnEncryptedReposDeltaIsEncryptedAtRestAndStillMerges(t *testing.T) {
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()

	idx, err := index.NewDedupIndex(repo, index.ReadOpenExpectedChunks, 0.01, 0, key)
	if err != nil {
		t.Fatal(err)
	}
	deltaPath := filepath.Join(repo, "session.delta")
	if err := idx.CaptureDelta(deltaPath); err != nil {
		t.Fatal(err)
	}
	id := hasher.Sum([]byte("a chunk in an encrypted cloud repo"))
	idx.Insert(id, 3, 8, 33)
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(deltaPath); err == nil {
		t.Fatal("an encrypted repo left its index delta in the clear")
	}
	blob, err := os.ReadFile(deltaPath + ".enc")
	if err != nil {
		t.Fatalf("no encrypted delta: %v", err)
	}
	if _, err := index.ParseDelta(blob); err == nil {
		t.Fatal("the .enc delta parsed as plaintext — it was not encrypted")
	}

	// The read side: staged where an index open looks, merged with the key.
	reader := t.TempDir()
	if err := os.MkdirAll(filepath.Join(reader, index.DeltaSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reader, index.DeltaSubdir, "b.delta.enc"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	merged, err := index.NewDedupIndexReadOnly(reader, index.ReadOpenExpectedChunks, 0.01, 0, key)
	if err != nil {
		t.Fatalf("opening an encrypted repo's index: %v", err)
	}
	defer merged.CloseDiscard()
	if _, found, err := merged.LookupDirect(id.StrongHash); err != nil || !found {
		t.Fatalf("the encrypted delta did not merge (found=%v err=%v)", found, err)
	}

	// And without the key it is a REFUSAL, not a short index: a reader that
	// silently skipped it would hand restore a repository missing whatever
	// that delta described, indistinguishable from lost data.
	if _, err := index.NewDedupIndexReadOnly(reader, index.ReadOpenExpectedChunks, 0.01, 0, nil); err == nil {
		t.Fatal("an encrypted delta was silently skipped by a keyless open")
	}
}

func TestADiscardedRunPublishesNoDelta(t *testing.T) {
	// CloseDiscard is what a FAILED backup does: its inserts reference chunks
	// in packs that were never sealed. A delta left behind would be merged by
	// the next reader and hand restore coordinates into nothing.
	dir := t.TempDir()
	idx, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	deltaPath := filepath.Join(dir, "session.delta")
	if err := idx.CaptureDelta(deltaPath); err != nil {
		t.Fatal(err)
	}
	idx.Insert(hasher.Sum([]byte("stored into a pack that never sealed")), 9, 8, 36)
	if err := idx.CloseDiscard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(deltaPath); err == nil {
		t.Fatal("a discarded run left a delta behind — merging it would point restore at a pack " +
			"the crashed writer never uploaded")
	}
}
