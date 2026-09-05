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

// TestOpenDiskHashTableRejectsZeroSlots guards issue #16: a corrupt .htab header
// with numSlots=0 previously opened fine, then Insert's (hash % numSlots) panics
// with an integer divide-by-zero. Opening must reject it.
func TestOpenDiskHashTableRejectsZeroSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.htab")
	hdr := make([]byte, htabHeaderSize)
	copy(hdr[0:8], htabMagic[:])
	// numSlots (hdr[8:16]) and count (hdr[16:24]) left zero.
	if err := os.WriteFile(path, hdr, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := openDiskHashTable(path); err == nil {
		t.Fatal("openDiskHashTable accepted a numSlots=0 header; Insert would divide-by-zero panic")
	}
}

// TestLoadBloomFilterRejectsNonMultipleOf64 guards issue #16: a numBits value
// that is not a positive multiple of 64 passed the floor-division size check but
// made Add/MayContain panic. LoadBloomFilter must reject it.
func TestLoadBloomFilterRejectsNonMultipleOf64(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.bloom")
	// numBits=100 (not a multiple of 64); expectedSize = 16 + (100/64)*8 = 24,
	// so a 24-byte file passes the old size check.
	buf := make([]byte, 24)
	binary.LittleEndian.PutUint64(buf[0:8], 100)
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadBloomFilter(path); err == nil {
		t.Fatal("LoadBloomFilter accepted numBits=100 (not a multiple of 64); Add/MayContain would panic")
	}
}

// TestHashIndexCountDoesNotDoubleCount guards issue #16: Count() added
// htab.count + len(buffer) blindly, so a hash already flushed to the hash table
// and then re-Inserted into the buffer was counted twice.
func TestHashIndexCountDoesNotDoubleCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hash-index.db")
	idx, err := NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	defer idx.Close()

	var sh [32]byte
	sh[0] = 0x7a
	id := hasher.ChunkID{WeakHash: 1, StrongHash: sh}

	// Flush the hash into the table.
	idx.Insert(id, 0, 0, 8192)
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}
	// Re-Insert the same hash into the buffer (not yet flushed).
	idx.Insert(id, 0, 0, 8192)

	if got := idx.Count(); got != 1 {
		t.Fatalf("Count() = %d, want 1 (a re-Inserted, already-flushed hash was double-counted)", got)
	}
}
