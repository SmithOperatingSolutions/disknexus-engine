// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func hashN(n uint64) [32]byte {
	var h [32]byte
	binary.BigEndian.PutUint64(h[:], n)
	return h
}

// The streamed fold is the compaction every repo's index goes through: the
// result must resolve every chunk of every delta, and a hash present in
// two deltas resolves to the LATER delta's location (a re-stored chunk
// after a prune moved it). FilterSortedIndex is the GC rebuild's
// drop-these-packs pass: the dropped pack's chunks vanish, nothing else.
func TestFoldDeltasStreamedLaterWinsAndFilterDropsOnlyThePack(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DeltaSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Delta a: chunks 1..100 in pack 0. Delta b: chunks 101..150 in pack 1,
	// plus chunk 50 AGAIN in pack 1 at a new offset (later wins).
	a := &Delta{}
	for n := uint64(1); n <= 100; n++ {
		a.Entries = append(a.Entries, DeltaEntry{StrongHash: hashN(n), WeakHash: n * 7919, PackNumber: 0, StoreOffset: n * 4096, ChunkLength: 4096})
	}
	b := &Delta{}
	for n := uint64(101); n <= 150; n++ {
		b.Entries = append(b.Entries, DeltaEntry{StrongHash: hashN(n), WeakHash: n * 7919, PackNumber: 1, StoreOffset: n * 4096, ChunkLength: 4096})
	}
	b.Entries = append(b.Entries, DeltaEntry{StrongHash: hashN(50), WeakHash: 50 * 7919, PackNumber: 1, StoreOffset: 999_999, ChunkLength: 4096})
	for name, d := range map[string]*Delta{"0000000a.delta": a, "0000000b.delta": b} {
		if err := os.WriteFile(filepath.Join(dir, DeltaSubdir, name), d.Marshal(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	folded, err := FoldDeltasStreamed(dir, 10_000, 0.001)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 151 {
		t.Fatalf("folded %d entries, want 151 (duplicates counted, as the staging report does)", folded)
	}

	open := func() *DedupIndex {
		t.Helper()
		idx, err := NewDedupIndexReadOnly(dir, 10_000, 0.001, 0)
		if err != nil {
			t.Fatal(err)
		}
		return idx
	}
	idx := open()
	for n := uint64(1); n <= 150; n++ {
		e, found, err := idx.LookupDirect(hashN(n))
		if err != nil || !found {
			t.Fatalf("chunk %d not resolvable after the fold (found=%v err=%v)", n, found, err)
		}
		wantPack, wantOff := uint32(0), n*4096
		if n > 100 || n == 50 {
			wantPack = 1
		}
		if n == 50 {
			wantOff = 999_999
		}
		if e.PackNumber != wantPack || e.StoreOffset != wantOff {
			t.Fatalf("chunk %d resolves to pack %d offset %d, want pack %d offset %d (later delta wins)", n, e.PackNumber, e.StoreOffset, wantPack, wantOff)
		}
	}
	if _, found, _ := idx.LookupDirect(hashN(9999)); found {
		t.Fatal("a chunk no delta carried resolves")
	}
	idx.CloseDiscard()

	// The fold leaves the staged deltas for the caller to delete once the
	// compacted index is published — and every open merges whatever is
	// still under deltas/. Do what compaction does before filtering, or the
	// re-open would resurrect the dropped pack from the deltas.
	if err := os.RemoveAll(filepath.Join(dir, DeltaSubdir)); err != nil {
		t.Fatal(err)
	}
	// Drop pack 0: chunks 1..100 except 50 (which moved to pack 1) vanish.
	if err := FilterSortedIndex(filepath.Join(dir, "hash-index.db"), func(pack uint32) bool { return pack != 0 }); err != nil {
		t.Fatal(err)
	}
	idx = open()
	defer idx.CloseDiscard()
	kept, dropped := 0, 0
	for n := uint64(1); n <= 150; n++ {
		_, found, err := idx.LookupDirect(hashN(n))
		if err != nil {
			t.Fatal(err)
		}
		inPack1 := n > 100 || n == 50
		switch {
		case inPack1 && !found:
			t.Fatalf("chunk %d (pack 1) was dropped by a filter that keeps pack 1", n)
		case !inPack1 && found:
			t.Fatalf("chunk %d (pack 0) survived a filter that drops pack 0", n)
		case found:
			kept++
		default:
			dropped++
		}
	}
	if kept != 51 || dropped != 99 {
		t.Fatalf("kept %d dropped %d, want 51/99", kept, dropped)
	}
}
