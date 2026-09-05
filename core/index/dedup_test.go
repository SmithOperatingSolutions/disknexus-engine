// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

func TestDedupIndexEncryptedRoundTrip(t *testing.T) {
	dir := t.TempDir()

	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	// Create index with encryption
	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 1, mk)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}

	// Insert some entries
	ids := make([]hasher.ChunkID, 20)
	for i := range ids {
		ids[i] = hasher.ChunkID{
			WeakHash:   uint64(i * 12345),
			StrongHash: [32]byte{byte(i), byte(i + 1), byte(i + 2)},
		}
		idx.Insert(ids[i], uint32(i%3), uint64(i*100), uint32(8192))
	}

	// Flush and close
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify: .enc files should exist, plaintext should not
	if _, err := os.Stat(filepath.Join(dir, "bloom.bin.enc")); err != nil {
		t.Error("bloom.bin.enc should exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "hash-index.db.enc")); err != nil {
		t.Error("hash-index.db.enc should exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "bloom.bin")); err == nil {
		t.Error("bloom.bin plaintext should not exist at rest")
	}
	if _, err := os.Stat(filepath.Join(dir, "hash-index.db")); err == nil {
		t.Error("hash-index.db plaintext should not exist at rest")
	}

	// Reopen with same key
	idx2, err := index.NewDedupIndex(dir, 1000, 0.01, 1, mk)
	if err != nil {
		t.Fatalf("NewDedupIndex reopen: %v", err)
	}
	defer idx2.Close()

	// Verify all entries are found
	for i, id := range ids {
		result, err := idx2.Check(id)
		if err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
		if result.IsNew {
			t.Errorf("entry %d should exist but Check says new", i)
		}
	}
}

// TestDedupIndexReadOnlyRoundTrip verifies the prune workflow: create an index
// with the normal constructor, populate it, close; then reopen with ReadOnly,
// call ReadAllEntries, and confirm all entries are returned. Also verifies that
// no .htab file is created during the ReadOnly open.
func TestDedupIndexReadOnlyRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Session 1: normal index, insert entries.
	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	const total = 200
	ids := make([]hasher.ChunkID, total)
	for i := range total {
		ids[i] = hasher.ChunkID{
			WeakHash:   uint64(i * 7),
			StrongHash: entryHash(i),
		}
		idx.Insert(ids[i], uint32(i%5), uint64(i*100), uint32(8192))
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Session 2: open read-only.
	roIdx, err := index.NewDedupIndexReadOnly(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndexReadOnly: %v", err)
	}
	defer roIdx.Close()

	// No .htab should exist.
	htabPath := filepath.Join(dir, "hash-index.db.htab")
	if _, err := os.Stat(htabPath); !os.IsNotExist(err) {
		t.Error(".htab should not exist with ReadOnly")
	}

	// ReadAllEntries should return all entries.
	all, err := roIdx.ReadAllEntries()
	if err != nil {
		t.Fatalf("ReadAllEntries: %v", err)
	}
	if len(all) != total {
		t.Errorf("ReadAllEntries: got %d, want %d", len(all), total)
	}

	// Verify field values via a map lookup on strong hash.
	found := make(map[[32]byte]index.IndexEntry, len(all))
	for _, e := range all {
		found[e.StrongHash] = e
	}
	for i, id := range ids {
		e, ok := found[id.StrongHash]
		if !ok {
			t.Errorf("entry %d not found in ReadAllEntries", i)
			continue
		}
		if e.PackNumber != uint32(i%5) {
			t.Errorf("entry %d: PackNumber got %d want %d", i, e.PackNumber, i%5)
		}
	}
}

// TestDedupIndexReadOnlyStagingPattern exercises the staging-index pattern from
// prune: open ReadOnly on a new empty dir, Insert entries, Close, then reopen
// normally and verify entries are present.
func TestDedupIndexReadOnlyStagingPattern(t *testing.T) {
	dir := t.TempDir()

	// Open read-only on empty dir, insert, close.
	idx, err := index.NewDedupIndexReadOnly(dir, 500, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndexReadOnly: %v", err)
	}
	const total = 300
	for i := range total {
		id := hasher.ChunkID{
			WeakHash:   uint64(i),
			StrongHash: entryHash(i),
		}
		idx.Insert(id, uint32(i), uint64(i*48), 4096)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with normal constructor and verify via Check.
	idx2, err := index.NewDedupIndex(dir, 500, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer idx2.Close()

	missing := 0
	for i := range total {
		id := hasher.ChunkID{
			WeakHash:   uint64(i),
			StrongHash: entryHash(i),
		}
		result, err := idx2.Check(id)
		if err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
		if result.IsNew {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d/%d entries missing after ReadOnly staging round-trip", missing, total)
	}
}

func TestDedupIndexEncryptedWrongKey(t *testing.T) {
	dir := t.TempDir()

	mk1, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk1.Destroy()

	// Create and populate index
	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 1, mk1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	idx.Insert(hasher.ChunkID{WeakHash: 42, StrongHash: [32]byte{1}}, 0, 0, 100)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Try to open with a different key
	mk2, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk2.Destroy()

	_, err = index.NewDedupIndex(dir, 1000, 0.01, 1, mk2)
	if err == nil {
		t.Error("expected error opening with wrong key, got nil")
	}
}

// #183: NewDedupIndexReadOnly returned not-found for genuinely-present
// entries whenever no .htab file sat next to hash-index.db — exactly the
// state of an index downloaded from cloud storage (only the .db and
// bloom.bin are published). HashIndex.Lookup consulted the in-memory map,
// then the hash table, and never fell back to the sorted on-disk file.
func TestReadOnlyLookupWithoutHtab(t *testing.T) {
	dir := t.TempDir()
	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	var hashes [][32]byte
	for i := 0; i < 50; i++ {
		var h [32]byte
		h[0], h[1], h[2] = byte(i), byte(i>>8), 0xA5
		hashes = append(hashes, h)
		idx.Insert(hasher.ChunkID{StrongHash: h}, uint32(i%4), uint64(i*100), 64)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	// The cloud-download scenario: db + bloom present, NO .htab sidecar.
	if err := os.Remove(filepath.Join(dir, "hash-index.db.htab")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	ro, err := index.NewDedupIndexReadOnly(dir, 1000, 0.01, 16, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.CloseDiscard()
	for _, h := range hashes {
		e, found, err := ro.LookupDirect(h)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("read-only index missed inserted hash %x with no .htab present", h[:4])
		}
		if e.ChunkLength != 64 {
			t.Fatalf("entry mismatch for %x: %+v", h[:4], e)
		}
	}
}
