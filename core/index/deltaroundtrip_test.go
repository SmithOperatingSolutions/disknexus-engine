// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// A captured delta is what a backup publishes so other writers (and the
// fold) learn its chunks: every Insert after arming lands in it, in
// order, with the pack/offset/length that Lookup will return; the count
// read from the header agrees with a full parse; the streaming iterator
// yields the same records as the materializing parser; and a corrupt
// blob is refused by both, never partially applied.
func TestCapturedDeltaRoundTripsThroughEveryReader(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewDedupIndex(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	if idx.DeltaPath() != "" {
		t.Fatal("DeltaPath reports a path before capture is armed")
	}
	deltaPath := filepath.Join(t.TempDir(), "run.delta")
	if err := idx.CaptureDelta(deltaPath); err != nil {
		t.Fatal(err)
	}
	if err := idx.CaptureDelta(deltaPath); err == nil {
		t.Fatal("arming capture twice returned no error")
	}
	if idx.DeltaPath() != deltaPath {
		t.Fatalf("DeltaPath = %q, want %q", idx.DeltaPath(), deltaPath)
	}

	const n = 200
	var ids []hasher.ChunkID
	for i := 0; i < n; i++ {
		var data [64]byte
		binary.BigEndian.PutUint64(data[:], uint64(i))
		id := hasher.Sum(data[:])
		ids = append(ids, id)
		idx.Insert(id, uint32(i%5), uint64(i)*4096, 4096+uint32(i%3))
	}
	if got := idx.DeltaEntryCount(); got != n {
		t.Fatalf("DeltaEntryCount() = %d, want %d", got, n)
	}
	if st := idx.Stats(); st.IndexEntries != n || st.BloomItems != n {
		t.Fatalf("Stats = %+v, want %d entries and %d bloom items", st, n, n)
	}
	if err := idx.WriteDeltaObject(); err != nil {
		t.Fatal(err)
	}

	// Header count, without a parse.
	if c, err := DeltaEntryCount(deltaPath); err != nil || c != n {
		t.Fatalf("DeltaEntryCount(path) = %d, %v; want %d", c, err, n)
	}
	if err := ValidateDeltaFile(deltaPath); err != nil {
		t.Fatalf("ValidateDeltaFile: %v", err)
	}
	blob, err := os.ReadFile(deltaPath)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDelta(blob)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Entries) != n {
		t.Fatalf("ParseDelta: %d entries, want %d", len(parsed.Entries), n)
	}
	var streamed []DeltaEntry
	if err := ForEachDeltaEntry(blob, func(e *DeltaEntry) { streamed = append(streamed, *e) }); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != n {
		t.Fatalf("ForEachDeltaEntry: %d entries, want %d", len(streamed), n)
	}
	for i := 0; i < n; i++ {
		want := DeltaEntry{StrongHash: ids[i].StrongHash, WeakHash: ids[i].WeakHash, PackNumber: uint32(i % 5),
			StoreOffset: uint64(i) * 4096, ChunkLength: 4096 + uint32(i%3)}
		if parsed.Entries[i] != want {
			t.Fatalf("ParseDelta entry %d = %+v, want %+v", i, parsed.Entries[i], want)
		}
		if streamed[i] != want {
			t.Fatalf("streamed entry %d = %+v, want %+v", i, streamed[i], want)
		}
	}
	if packs := parsed.PackNumbers(); len(packs) != 5 || packs[0] != 0 || packs[4] != 4 {
		t.Fatalf("PackNumbers = %v, want the 5 distinct packs ascending", packs)
	}
	// The same bytes, marshalled again, are the same delta.
	if again := parsed.Marshal(); string(again) != string(blob) {
		t.Fatal("Marshal(ParseDelta(blob)) != blob")
	}

	// Corruption anywhere in the payload fails the checksum in both readers.
	bad := append([]byte(nil), blob...)
	bad[len(bad)-3] ^= 0x01
	if _, err := ParseDelta(bad); err == nil {
		t.Fatal("ParseDelta accepted a corrupt payload")
	}
	calls := 0
	if err := ForEachDeltaEntry(bad, func(*DeltaEntry) { calls++ }); err == nil {
		t.Fatal("ForEachDeltaEntry accepted a corrupt payload")
	}
	if calls != 0 {
		t.Fatalf("ForEachDeltaEntry yielded %d entries from a corrupt blob before failing — partial application", calls)
	}
	if err := ForEachDeltaEntry(blob[:DeltaHeaderSize-1], func(*DeltaEntry) {}); err == nil {
		t.Fatal("a truncated header was accepted")
	}
	if _, err := DeltaEntryCount(filepath.Join(dir, "missing.delta")); err == nil {
		t.Fatal("DeltaEntryCount on a missing file returned no error")
	}
}

// Stats, FlushHashIndex and SaveBloom are what a long backup uses to bound
// memory and what a rebuild hands its filter over with; the lookups that
// follow must still find everything.
func TestFlushHashIndexAndSaveBloomKeepEveryChunkFindable(t *testing.T) {
	dir := t.TempDir()
	idx, err := NewDedupIndex(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	var ids []hasher.ChunkID
	for i := 0; i < 300; i++ {
		var data [32]byte
		binary.LittleEndian.PutUint64(data[:], uint64(i))
		id := hasher.Sum(data[:])
		ids = append(ids, id)
		idx.Insert(id, 1, uint64(i)*100, 100)
		if i == 149 {
			if err := idx.FlushHashIndex(); err != nil {
				t.Fatalf("FlushHashIndex mid-run: %v", err)
			}
		}
	}
	for i, id := range ids {
		e, found, err := idx.LookupDirect(id.StrongHash)
		if err != nil || !found || e.StoreOffset != uint64(i)*100 {
			t.Fatalf("chunk %d after a mid-run flush: found=%v err=%v entry=%+v", i, found, err, e)
		}
	}
	saved := filepath.Join(t.TempDir(), "bloom.copy")
	if err := idx.SaveBloom(saved); err != nil {
		t.Fatal(err)
	}
	bf, err := LoadBloomFilter(saved)
	if err != nil {
		t.Fatal(err)
	}
	if bf.Count() != 300 {
		t.Fatalf("saved bloom holds %d items, want 300", bf.Count())
	}
	for _, id := range ids {
		if !bf.MayContain(id.WeakHash) {
			t.Fatalf("saved bloom lost a chunk — a rebuild handed this filter would re-store it")
		}
	}
}

// BloomSizeBytes is the heap gate's projection (#507): it must equal what
// NewBloomFilter actually allocates, or the gate admits a fold that then
// exceeds the cap.
func TestBloomSizeBytesMatchesTheAllocation(t *testing.T) {
	for _, c := range []struct {
		items uint64
		fp    float64
	}{{0, 0.01}, {1, 0.01}, {10_000, 0.01}, {1_000_000, 0.001}, {5_000_000, 0.0001}} {
		want := int64(NewBloomFilter(c.items, c.fp).SizeBytes())
		if got := BloomSizeBytes(c.items, c.fp); got != want {
			t.Errorf("BloomSizeBytes(%d, %g) = %d, NewBloomFilter allocates %d", c.items, c.fp, got, want)
		}
	}
}
