// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// The delta object is the unit an append-only index is built out of (#357
// phase 2), so its three properties are load-bearing and each gets a test:
//
//  1. It round-trips. Every field a restore needs — hash, pack, offset,
//     length — survives, plus the weak hash the bloom is keyed on (which the
//     hash index does NOT store and cannot recompute; see hasher.Sum).
//  2. Applying it is IDEMPOTENT and ORDER-INDEPENDENT. Crash recovery
//     re-applies deltas and compaction may see the same delta twice; both
//     must be no-ops.
//  3. It refuses to be misread. A future format change must be DETECTABLE,
//     and a corrupted delta must fail loudly rather than contribute
//     plausible-looking garbage to an index restore resolves through.

func TestDeltaRoundTripsEveryFieldARestoreNeeds(t *testing.T) {
	d := &index.Delta{Entries: []index.DeltaEntry{
		{StrongHash: h(1), WeakHash: 0xdeadbeefcafe, PackNumber: 7, StoreOffset: 1 << 40, ChunkLength: 65536},
		{StrongHash: h(2), WeakHash: 1, PackNumber: 0, StoreOffset: 0, ChunkLength: 1},
	}}
	blob := d.Marshal()
	got, err := index.ParseDelta(blob)
	if err != nil {
		t.Fatalf("ParseDelta: %v", err)
	}
	if len(got.Entries) != len(d.Entries) {
		t.Fatalf("got %d entries, want %d", len(got.Entries), len(d.Entries))
	}
	for i := range d.Entries {
		if got.Entries[i] != d.Entries[i] {
			t.Errorf("entry %d round-tripped as %+v, want %+v", i, got.Entries[i], d.Entries[i])
		}
	}
}

func TestDeltaSizeScalesWithEntryCountNotIndexSize(t *testing.T) {
	small := (&index.Delta{Entries: make([]index.DeltaEntry, 10)}).Marshal()
	large := (&index.Delta{Entries: make([]index.DeltaEntry, 1010)}).Marshal()
	if grew := len(large) - len(small); grew != 1000*index.DeltaEntrySize {
		t.Fatalf("1000 more entries grew the delta by %d bytes, want %d", grew, 1000*index.DeltaEntrySize)
	}
}

func TestApplyingADeltaTwiceIsANoOp(t *testing.T) {
	dir := t.TempDir()
	d := &index.Delta{Entries: []index.DeltaEntry{
		{StrongHash: h(1), WeakHash: 11, PackNumber: 3, StoreOffset: 100, ChunkLength: 10},
		{StrongHash: h(2), WeakHash: 22, PackNumber: 3, StoreOffset: 200, ChunkLength: 20},
	}}

	once := applyToFreshIndex(t, filepath.Join(dir, "once"), d)
	twice := applyToFreshIndex(t, filepath.Join(dir, "twice"), d, d)

	// NEGATIVE CONTROL, and the reason this test exists at all (#378 item 8).
	// "once == twice" and "the bloom bits match" are both properties of an
	// index NOTHING was applied to, so this whole test passed against
	//
	//	func (d *Delta) ApplyTo(idx *DedupIndex) {}
	//
	// It was testing the Go map and the bloom's bit-setting, not delta code.
	// An index the delta was applied to must differ from one it was not.
	none := applyToFreshIndex(t, filepath.Join(dir, "none"))
	if bytes.Equal(once, none) {
		t.Fatal("applying a delta produced byte-identical index files to applying nothing at all — " +
			"ApplyTo did not apply anything, and every equality below holds trivially")
	}

	if !bytes.Equal(once, twice) {
		t.Fatalf("applying the same delta twice produced a different index (%d vs %d bytes) — "+
			"re-application must be a no-op or a crash-recovery re-apply corrupts the repo", len(once), len(twice))
	}
	// The bloom's BITS must match too — they are what dedup reads. (Its
	// 8-byte "count" header field is a stats estimate that over-counts a
	// re-applied delta; nothing consults it for a decision.)
	if !bytes.Equal(bloomBits(t, filepath.Join(dir, "once")), bloomBits(t, filepath.Join(dir, "twice"))) {
		t.Fatal("applying the same delta twice produced different bloom bits")
	}

	// And what SURVIVED the double application is the delta's own content, at
	// its own coordinates: "the two files are equal" says nothing about them
	// being right.
	assertDeltaEntriesResolve(t, filepath.Join(dir, "twice"), d)
}

// assertDeltaEntriesResolve is the authority every applied-delta test is
// measured against: each entry the delta carried must be findable in the
// resulting index, at the pack, offset and length it named.
//
// Restore resolves chunks through LookupDirect alone and hard-fails on a miss,
// so an entry that is absent — or present pointing somewhere else — is an
// unrestorable backup, and no amount of comparing two indexes to each other
// notices it.
func assertDeltaEntriesResolve(t *testing.T, dir string, deltas ...*index.Delta) {
	t.Helper()
	idx, err := index.NewDedupIndexReadOnly(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	n := 0
	for _, d := range deltas {
		for _, want := range d.Entries {
			got, found, err := idx.LookupDirect(want.StrongHash)
			if err != nil {
				t.Fatalf("looking up %x: %v", want.StrongHash[:4], err)
			}
			if !found {
				t.Fatalf("chunk %x is not in the index the delta was applied to — restore resolves chunks "+
					"through LookupDirect alone and hard-fails on a miss", want.StrongHash[:4])
			}
			if got.PackNumber != want.PackNumber || got.StoreOffset != want.StoreOffset || got.ChunkLength != want.ChunkLength {
				t.Fatalf("chunk %x resolves to pack %d offset %d length %d, want pack %d offset %d length %d — "+
					"a delta entry that lands at the wrong coordinates is a restore reading the wrong bytes",
					want.StrongHash[:4], got.PackNumber, got.StoreOffset, got.ChunkLength,
					want.PackNumber, want.StoreOffset, want.ChunkLength)
			}
			n++
		}
	}
	if n == 0 {
		t.Fatal("assertDeltaEntriesResolve was given no entries to check — it is proving nothing")
	}
}

// bloomBits reads bloom.bin's bit array, skipping the 16-byte header.
func bloomBits(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "bloom.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return data[16:]
}

func TestDeltaApplicationIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	a := &index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(1), WeakHash: 11, PackNumber: 1, StoreOffset: 8, ChunkLength: 10}}}
	b := &index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(2), WeakHash: 22, PackNumber: 2, StoreOffset: 8, ChunkLength: 20}}}

	ab := applyToFreshIndex(t, filepath.Join(dir, "ab"), a, b)
	ba := applyToFreshIndex(t, filepath.Join(dir, "ba"), b, a)

	// NEGATIVE CONTROL — see the note in TestApplyingADeltaTwiceIsANoOp. Two
	// empty indexes are also order-independent, and this test passed against a
	// no-op ApplyTo for exactly that reason.
	none := applyToFreshIndex(t, filepath.Join(dir, "none"))
	if bytes.Equal(ab, none) {
		t.Fatal("applying two deltas produced byte-identical index files to applying nothing at all — " +
			"ApplyTo did not apply anything, and commutativity over an empty set is free")
	}

	if !bytes.Equal(ab, ba) {
		t.Fatal("delta application is order-dependent — writers land in an arbitrary order, so a repo's " +
			"index would depend on which listing a reader happened to get")
	}

	// Both writers' work is present in BOTH orders. Identical files that are
	// identically missing an entry would satisfy the comparison above.
	assertDeltaEntriesResolve(t, filepath.Join(dir, "ab"), a, b)
	assertDeltaEntriesResolve(t, filepath.Join(dir, "ba"), a, b)
}

func TestAppliedDeltaEntriesAreVisibleToBothDedupTiers(t *testing.T) {
	dir := t.TempDir()
	id := hasher.Sum([]byte("a chunk that only a delta knows about"))
	d := &index.Delta{Entries: []index.DeltaEntry{
		{StrongHash: id.StrongHash, WeakHash: id.WeakHash, PackNumber: 4, StoreOffset: 64, ChunkLength: 99},
	}}
	applyToFreshIndex(t, filepath.Join(dir, "idx"), d)

	idx, err := index.NewDedupIndex(filepath.Join(dir, "idx"), index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()

	// Tier 2 — what RESTORE uses. A miss here is an unrestorable backup.
	if _, found, err := idx.LookupDirect(id.StrongHash); err != nil || !found {
		t.Fatalf("LookupDirect on a delta-supplied chunk: found=%v err=%v — restore resolves chunks "+
			"through this call alone and hard-fails on a miss", found, err)
	}
	// Tier 1 — what DEDUP uses. A miss here re-uploads a chunk the repo has;
	// correctness survives, cross-device dedup (the point of #357) does not.
	res, err := idx.Check(id)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsNew {
		t.Fatal("a delta-supplied chunk reads as NEW to dedup — the bloom filter is keyed on the weak hash, " +
			"which the hash index does not store, so the delta must carry it")
	}
}

func TestADeltaFromAFutureFormatIsRefusedNotMisread(t *testing.T) {
	blob := (&index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(1)}}}).Marshal()
	blob[6]++ // bump the version word
	if _, err := index.ParseDelta(blob); err == nil {
		t.Fatal("a delta written by a newer format parsed clean — a format change must be detectable")
	}
	bad := (&index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(1)}}}).Marshal()
	bad[0] = 'X'
	if _, err := index.ParseDelta(bad); err == nil {
		t.Fatal("a non-delta object parsed as a delta")
	}
	torn := (&index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(1)}}}).Marshal()
	if _, err := index.ParseDelta(torn[:len(torn)-4]); err == nil {
		t.Fatal("a truncated delta parsed clean")
	}
	flipped := (&index.Delta{Entries: []index.DeltaEntry{{StrongHash: h(1), PackNumber: 5}}}).Marshal()
	flipped[index.DeltaHeaderSize+40]++ // corrupt a pack number in the payload
	if _, err := index.ParseDelta(flipped); err == nil {
		t.Fatal("a delta whose payload was corrupted in flight parsed clean — every entry it carries " +
			"becomes a chunk location restore trusts, so the object must carry its own checksum")
	}
}

// applyToFreshIndex applies the deltas in order to an empty index at dir and
// returns the resulting hash-index.db bytes.
func applyToFreshIndex(t *testing.T, dir string, deltas ...*index.Delta) []byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx, err := index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range deltas {
		d.ApplyTo(idx)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "hash-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func h(b byte) [32]byte {
	var out [32]byte
	for i := range out {
		out[i] = b
	}
	return out
}
