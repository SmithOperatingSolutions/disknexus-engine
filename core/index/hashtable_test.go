// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// makeEntry creates an IndexEntry whose first 8 hash bytes encode prefix and
// whose last byte is suffix, making the full hash unique per (prefix, suffix)
// pair.  The slot index for a table with n slots is prefix % n.
func makeEntry(prefix uint64, suffix byte, packNum uint32) IndexEntry {
	var h [32]byte
	binary.BigEndian.PutUint64(h[0:8], prefix)
	h[31] = suffix
	return IndexEntry{StrongHash: h, PackNumber: packNum, StoreOffset: uint64(packNum) * 100, ChunkLength: 4096}
}

// TestDiskHashTableCreateAndOpen verifies that a newly created table has the
// correct header fields, and that Close persists the count so a subsequent
// open reads it back.
func TestDiskHashTableCreateAndOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")

	ht, err := createDiskHashTable(path, 64)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	if ht.numSlots != 64 {
		t.Errorf("numSlots = %d, want 64", ht.numSlots)
	}
	if ht.count != 0 {
		t.Errorf("initial count = %d, want 0", ht.count)
	}

	// Insert a few entries then close.
	for i := range 5 {
		if err := ht.Insert(makeEntry(uint64(i+1), 0, uint32(i))); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	if ht.count != 5 {
		t.Errorf("count after inserts = %d, want 5", ht.count)
	}
	if err := ht.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen and verify header is persisted.
	ht2, err := openDiskHashTable(path)
	if err != nil {
		t.Fatalf("openDiskHashTable: %v", err)
	}
	defer ht2.Close()

	if ht2.numSlots != 64 {
		t.Errorf("reopened numSlots = %d, want 64", ht2.numSlots)
	}
	if ht2.count != 5 {
		t.Errorf("reopened count = %d, want 5", ht2.count)
	}
}

// TestDiskHashTableFileSize verifies that the on-disk file size equals
// header + numSlots × EntrySize.
func TestDiskHashTableFileSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 32)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	ht.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := int64(htabHeaderSize) + 32*EntrySize
	if info.Size() != want {
		t.Errorf("file size = %d, want %d", info.Size(), want)
	}
}

// TestDiskHashTableLookupEmpty verifies that Lookup on a fresh table always
// returns not-found without error.
func TestDiskHashTableLookupEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 16)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	for i := range 10 {
		e := makeEntry(uint64(i+1), 0, 0)
		entry, found, err := ht.Lookup(e.StrongHash)
		if err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
		if found || entry != nil {
			t.Errorf("Lookup %d on empty table: found=%v entry=%v", i, found, entry)
		}
	}
}

// TestDiskHashTableInsertLookup verifies basic insert-then-lookup round-trip
// with field value verification.
func TestDiskHashTableInsertLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 64)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	const n = 20
	for i := range n {
		e := makeEntry(uint64(i+1), 0, uint32(i))
		if err := ht.Insert(e); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	for i := range n {
		e := makeEntry(uint64(i+1), 0, uint32(i))
		got, found, err := ht.Lookup(e.StrongHash)
		if err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
		if !found {
			t.Fatalf("entry %d not found", i)
		}
		if got.PackNumber != uint32(i) {
			t.Errorf("entry %d: PackNumber = %d, want %d", i, got.PackNumber, i)
		}
		if got.StoreOffset != uint64(i)*100 {
			t.Errorf("entry %d: StoreOffset = %d, want %d", i, got.StoreOffset, uint64(i)*100)
		}
		if got.ChunkLength != 4096 {
			t.Errorf("entry %d: ChunkLength = %d, want 4096", i, got.ChunkLength)
		}
	}

	// A hash that was never inserted must not be found.
	absent := makeEntry(999, 0, 0)
	_, found, err := ht.Lookup(absent.StrongHash)
	if err != nil {
		t.Fatalf("Lookup absent: %v", err)
	}
	if found {
		t.Error("absent entry incorrectly found")
	}
}

// TestDiskHashTableIdempotentInsert verifies that inserting the same hash
// twice does not increment the count and does not return an error.
func TestDiskHashTableIdempotentInsert(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 16)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	e := makeEntry(7, 0, 42)
	if err := ht.Insert(e); err != nil {
		t.Fatalf("first Insert: %v", err)
	}
	if ht.count != 1 {
		t.Errorf("count after first insert = %d, want 1", ht.count)
	}
	if err := ht.Insert(e); err != nil {
		t.Fatalf("second Insert (same entry): %v", err)
	}
	if ht.count != 1 {
		t.Errorf("count after duplicate insert = %d, want 1", ht.count)
	}
}

// TestDiskHashTableTableFull verifies that Insert returns errTableFull once
// the load exceeds 85% of numSlots.
func TestDiskHashTableTableFull(t *testing.T) {
	const numSlots = 8
	// threshold = numSlots * 85 / 100 = 6; errTableFull when count > 6
	const threshold = numSlots * 85 / 100 // = 6

	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, numSlots)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	// Insert threshold+1 entries — all should succeed.
	for i := range threshold + 1 {
		e := makeEntry(uint64(i+1), 0, uint32(i))
		if err := ht.Insert(e); err != nil {
			t.Fatalf("Insert %d (should succeed): %v", i, err)
		}
	}
	if ht.count != threshold+1 {
		t.Errorf("count = %d, want %d", ht.count, threshold+1)
	}

	// The next insert must return errTableFull.
	extra := makeEntry(999, 0, 0)
	if err := ht.Insert(extra); err != errTableFull {
		t.Errorf("expected errTableFull, got %v", err)
	}
	// Count must not have changed.
	if ht.count != threshold+1 {
		t.Errorf("count changed on errTableFull: got %d, want %d", ht.count, threshold+1)
	}
}

// TestDiskHashTableLinearProbingCollisions exercises the linear probing path
// by inserting multiple entries that all map to the same initial slot.
// With numSlots=8 and prefix=8 (slot 0), prefix=16 (slot 0), prefix=24 (slot 0):
// entry A → slot 0, entry B → probe 0→1, entry C → probe 0→1→2.
// All three must be findable.
func TestDiskHashTableLinearProbingCollisions(t *testing.T) {
	const numSlots = 8
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, numSlots)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	// All three map to slot 0 (prefix % 8 == 0, prefix > 0 to avoid zero hash).
	entries := []IndexEntry{
		makeEntry(8, 1, 1),  // slot 0
		makeEntry(16, 2, 2), // collision → slot 1
		makeEntry(24, 3, 3), // collision → slot 2
	}
	for i, e := range entries {
		if e.StrongHash == ([32]byte{}) {
			t.Fatalf("entry %d has zero hash (sentinel)", i)
		}
		if slot := hashPrefix8(e.StrongHash) % numSlots; slot != 0 {
			t.Fatalf("entry %d: expected slot 0, got %d", i, slot)
		}
		if err := ht.Insert(e); err != nil {
			t.Fatalf("Insert entry %d: %v", i, err)
		}
	}

	for i, e := range entries {
		got, found, err := ht.Lookup(e.StrongHash)
		if err != nil {
			t.Fatalf("Lookup entry %d: %v", i, err)
		}
		if !found {
			t.Fatalf("entry %d not found after collision insert", i)
		}
		if got.PackNumber != e.PackNumber {
			t.Errorf("entry %d: PackNumber = %d, want %d", i, got.PackNumber, e.PackNumber)
		}
	}
}

// TestDiskHashTableWrapAround verifies that linear probing wraps from the last
// slot back to slot 0.  We create a table with 4 slots, insert entries whose
// home slot is 3, and force two of them to wrap around.
func TestDiskHashTableWrapAround(t *testing.T) {
	const numSlots = 4
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, numSlots)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	// prefix=3 → slot 3 for all three; probing wraps: 3 → 0 → 1
	entries := []IndexEntry{
		makeEntry(3, 1, 10), // lands at slot 3
		makeEntry(3, 2, 20), // probes 3→0 (wrap-around)
		makeEntry(3, 3, 30), // probes 3→0→1
	}
	for i, e := range entries {
		if slot := hashPrefix8(e.StrongHash) % numSlots; slot != 3 {
			t.Fatalf("entry %d home slot = %d, want 3", i, slot)
		}
		if err := ht.Insert(e); err != nil {
			t.Fatalf("Insert entry %d: %v", i, err)
		}
	}

	for i, e := range entries {
		got, found, err := ht.Lookup(e.StrongHash)
		if err != nil {
			t.Fatalf("Lookup entry %d: %v", i, err)
		}
		if !found {
			t.Fatalf("entry %d not found (wrap-around probing broken)", i)
		}
		if got.PackNumber != e.PackNumber {
			t.Errorf("entry %d: PackNumber = %d, want %d", i, got.PackNumber, e.PackNumber)
		}
	}
}

// TestDiskHashTableReadAll verifies that ReadAll returns exactly the inserted
// entries (order may differ) and matches the count field.
func TestDiskHashTableReadAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 32)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	const n = 15
	want := make(map[[32]byte]IndexEntry, n)
	for i := range n {
		e := makeEntry(uint64(i+1), 0, uint32(i))
		want[e.StrongHash] = e
		if err := ht.Insert(e); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}

	got, err := ht.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != n {
		t.Fatalf("ReadAll returned %d entries, want %d", len(got), n)
	}
	for _, e := range got {
		w, ok := want[e.StrongHash]
		if !ok {
			t.Errorf("ReadAll returned unexpected entry: %+v", e)
			continue
		}
		if e.PackNumber != w.PackNumber || e.StoreOffset != w.StoreOffset {
			t.Errorf("entry mismatch: got %+v, want %+v", e, w)
		}
	}
}

// TestDiskHashTableCountPersisted verifies that Close writes the in-memory
// count to the file header so that the count is available after reopening.
func TestDiskHashTableCountPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 64)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}

	for i := range 12 {
		if err := ht.Insert(makeEntry(uint64(i+1), 0, uint32(i))); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	if err := ht.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ht2, err := openDiskHashTable(path)
	if err != nil {
		t.Fatalf("openDiskHashTable: %v", err)
	}
	defer ht2.Close()

	if ht2.count != 12 {
		t.Errorf("count after reopen = %d, want 12", ht2.count)
	}
	// All entries must still be findable.
	for i := range 12 {
		e := makeEntry(uint64(i+1), 0, uint32(i))
		_, found, err := ht2.Lookup(e.StrongHash)
		if err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
		if !found {
			t.Errorf("entry %d not found after reopen", i)
		}
	}
}

// TestDiskHashTableInvalidMagic verifies that openDiskHashTable rejects a
// file whose magic bytes do not match.
func TestDiskHashTableInvalidMagic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.htab")
	if err := os.WriteFile(path, make([]byte, htabHeaderSize+EntrySize), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := openDiskHashTable(path)
	if err == nil {
		t.Fatal("expected error for invalid magic, got nil")
	}
}

// TestDiskHashTableReadAllEmpty verifies ReadAll on a fresh table returns a
// non-nil empty slice.
func TestDiskHashTableReadAllEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")
	ht, err := createDiskHashTable(path, 16)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	entries, err := ht.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ReadAll on empty table returned %d entries, want 0", len(entries))
	}
}
