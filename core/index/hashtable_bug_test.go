// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"path/filepath"
	"testing"
)

// TestDiskHashTable_InsertUpdatesExisting is a regression test: Insert must
// update the metadata (PackNumber/StoreOffset/ChunkLength) when the same
// StrongHash is inserted again with a different location.
//
// This matters during FlushDelta: if a chunk is rewritten to a new pack
// (e.g. during compaction), the updated location must not be lost, or the
// index would permanently point to the old pack/offset.
func TestDiskHashTable_InsertUpdatesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.htab")

	ht, err := createDiskHashTable(path, 64)
	if err != nil {
		t.Fatalf("createDiskHashTable: %v", err)
	}
	defer ht.Close()

	original := makeEntry(42, 0, 1) // pack=1, offset=100
	if err := ht.Insert(original); err != nil {
		t.Fatalf("Insert original: %v", err)
	}

	// Insert the same hash with a different pack location.
	updated := original
	updated.PackNumber = 5
	updated.StoreOffset = 999
	updated.ChunkLength = 8192

	if err := ht.Insert(updated); err != nil {
		t.Fatalf("Insert updated: %v", err)
	}

	// Look up the entry — it should reflect the updated location.
	loc, found, err := ht.Lookup(original.StrongHash)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("hash not found after insert")
	}

	if loc.PackNumber != 5 {
		t.Errorf("PackNumber = %d, want 5 (Insert did not update existing entry)", loc.PackNumber)
	}
	if loc.StoreOffset != 999 {
		t.Errorf("StoreOffset = %d, want 999 (Insert did not update existing entry)", loc.StoreOffset)
	}
	if loc.ChunkLength != 8192 {
		t.Errorf("ChunkLength = %d, want 8192 (Insert did not update existing entry)", loc.ChunkLength)
	}
}
