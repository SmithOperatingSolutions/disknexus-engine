// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

func chunkIDFromByte(b byte) hasher.ChunkID {
	var id hasher.ChunkID
	for i := range id.StrongHash {
		id.StrongHash[i] = b
	}
	return id
}

// TestSkipHtabFlushPreservesExistingEntries proves that Flush in skip-htab
// mode destroys the existing on-disk index: with idx.htab == nil, Flush
// collects only the in-memory buffer (the sorted index file is never read)
// and atomically renames the rewritten file over idx.path, permanently
// deleting every previously flushed entry. NewDedupIndexReadOnly explicitly
// documents "writes (Insert + Flush)" as supported usage of skip-htab mode.
func TestSkipHtabFlushPreservesExistingEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Build an index with some entries the normal way.
	idx, err := NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	existing := []hasher.ChunkID{chunkIDFromByte(1), chunkIDFromByte(2), chunkIDFromByte(3)}
	for i, id := range existing {
		idx.Insert(id, uint32(i), uint64(i)*100, 100)
	}
	if err := idx.Flush(); err != nil {
		t.Fatalf("initial Flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen in skip-htab (read-only) mode and add one more entry.
	roIdx, err := NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex skipHtab: %v", err)
	}
	added := chunkIDFromByte(9)
	roIdx.Insert(added, 7, 700, 100)
	if err := roIdx.Flush(); err != nil {
		t.Fatalf("skip-htab Flush: %v", err)
	}
	if err := roIdx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen normally: all four entries must be present.
	verify, err := NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex verify: %v", err)
	}
	defer verify.Close()

	for _, id := range append(existing, added) {
		_, ok, err := verify.Lookup(id)
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if !ok {
			t.Errorf("entry %x lost after skip-htab Flush", id.StrongHash[0])
		}
	}
}

// TestParsePackNumberFiveDigits proves that parsePackNumber cannot parse
// pack numbers >= 10000: Sscanf treats the 4 in %04d as a maximum scan
// width, so "10000.pack" (produced by the store's %04d Sprintf once pack
// numbers exceed 9999) parses 4 digits then fails to match ".pack" —
// aborting index Rebuild on repos with >= 10000 packs (> ~5 TB at the
// default 512 MB pack size), exactly the disaster-recovery path.
func TestParsePackNumberFiveDigits(t *testing.T) {
	// Sanity: this is how the store names packs (store.go packPath).
	name := "10000.pack"

	n, err := parsePackNumber(name)
	if err != nil {
		t.Fatalf("parsePackNumber(%q): %v", name, err)
	}
	if n != 10000 {
		t.Fatalf("parsePackNumber(%q) = %d, want 10000", name, n)
	}
}
