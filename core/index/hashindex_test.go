// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// entryHash returns a deterministic SHA-256 hash for entry number n.
func entryHash(n int) [32]byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(n))
	return sha256.Sum256(buf[:])
}

func entryID(n int) hasher.ChunkID {
	return hasher.ChunkID{StrongHash: entryHash(n)}
}

func TestHashIndexLookupWithCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 1, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	// Insert many entries to fill multiple pages.
	const numEntries = 500
	var hashes [numEntries][32]byte
	for i := 0; i < numEntries; i++ {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(i))
		hashes[i] = sha256.Sum256(buf[:])
		idx.Insert(hasher.ChunkID{StrongHash: hashes[i]}, uint32(i), uint64(i*100), uint32(4096))
	}

	// Flush to disk.
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Lookup every entry — exercises the page cache.
	for i := 0; i < numEntries; i++ {
		entry, found, err := idx.Lookup(hasher.ChunkID{StrongHash: hashes[i]})
		if err != nil {
			t.Fatalf("Lookup %d: %v", i, err)
		}
		if !found {
			t.Fatalf("entry %d not found", i)
		}
		if entry.PackNumber != uint32(i) {
			t.Errorf("entry %d: PackNumber = %d, want %d", i, entry.PackNumber, i)
		}
	}

	// Lookup a missing entry.
	missing := sha256.Sum256([]byte("does not exist"))
	_, found, err := idx.Lookup(hasher.ChunkID{StrongHash: missing})
	if err != nil {
		t.Fatalf("Lookup missing: %v", err)
	}
	if found {
		t.Error("expected missing entry to not be found")
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHashIndexLookupWithoutCache(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// cacheMB=0 → no cache
	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], 42)
	h := sha256.Sum256(buf[:])
	idx.Insert(hasher.ChunkID{StrongHash: h}, 1, 100, 4096)

	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	entry, found, err := idx.Lookup(hasher.ChunkID{StrongHash: h})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("entry not found")
	}
	if entry.PackNumber != 1 {
		t.Errorf("PackNumber = %d, want 1", entry.PackNumber)
	}

	idx.Close()
}

// TestHashIndexDeltaLargeRoundTrip inserts 200 000 entries in five batches,
// calling FlushDelta between each batch, then does a final Flush and verifies
// every entry is findable with correct field values.
func TestHashIndexDeltaLargeRoundTrip(t *testing.T) {
	const total = 200_000
	const batches = 5
	const perBatch = total / batches

	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 4, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	for b := range batches {
		for i := range perBatch {
			n := b*perBatch + i
			idx.Insert(entryID(n), uint32(b), uint64(n*48), uint32(4096+n%1024))
		}
		if err := idx.FlushDelta(); err != nil {
			t.Fatalf("FlushDelta batch %d: %v", b, err)
		}
	}

	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Spot-check every 100th entry with full field verification.
	for n := range total {
		if n%100 != 0 {
			continue
		}
		b := n / perBatch
		entry, found, err := idx.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			t.Fatalf("entry %d not found after delta+flush", n)
		}
		if entry == nil {
			t.Fatalf("entry %d: nil IndexEntry returned", n)
		}
		if entry.PackNumber != uint32(b) {
			t.Errorf("entry %d: PackNumber got %d want %d", n, entry.PackNumber, b)
		}
		if entry.StoreOffset != uint64(n*48) {
			t.Errorf("entry %d: StoreOffset got %d want %d", n, entry.StoreOffset, n*48)
		}
		if entry.ChunkLength != uint32(4096+n%1024) {
			t.Errorf("entry %d: ChunkLength got %d want %d", n, entry.ChunkLength, 4096+n%1024)
		}
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestHashIndexDeltaWithinBackupDedup verifies that entries flushed to delta
// files are still detected as duplicates by Lookup (via binary search on the
// sorted delta file), even though they are not yet in the main sorted index.
func TestHashIndexDeltaWithinBackupDedup(t *testing.T) {
	const total = 50_000
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	// Insert all entries.
	for n := range total {
		idx.Insert(entryID(n), 0, uint64(n), 4096)
	}

	// Flush to delta — clears the in-memory map.
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}

	// Every entry should still be found (via the flushed set), even though
	// the in-memory map is now empty and the main file has not been written.
	missing := 0
	for n := range total {
		_, found, err := idx.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d/%d entries not found via flushed set — within-backup dedup broken", missing, total)
	}

	// A chunk that was never inserted must not be found.
	_, found, err := idx.Lookup(entryID(total + 999))
	if err != nil {
		t.Fatalf("Lookup absent: %v", err)
	}
	if found {
		t.Error("absent entry should not be found")
	}

	idx.Close()
}

// TestHashIndexFlushLifecycle verifies the .htab file lifecycle: created at
// NewHashIndex, present after FlushDelta (no delta files), still present after
// Flush (rebuilt), and .tmp is cleaned up.
func TestHashIndexFlushLifecycle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	// .htab should exist immediately after NewHashIndex.
	if _, err := os.Stat(path + ".htab"); err != nil {
		t.Errorf(".htab missing after NewHashIndex: %v", err)
	}

	const flushes = 4
	for f := range flushes {
		for i := range 1000 {
			n := f*1000 + i
			idx.Insert(entryID(n), 0, uint64(n), 4096)
		}
		if err := idx.FlushDelta(); err != nil {
			t.Fatalf("FlushDelta %d: %v", f, err)
		}
		// No delta files should ever be created.
		dp := fmt.Sprintf("%s.delta.%06d", path, f)
		if _, err := os.Stat(dp); !os.IsNotExist(err) {
			t.Errorf("delta file %d created after FlushDelta (should not exist)", f)
		}
	}

	// Final Flush should succeed.
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// .htab should still exist (rebuilt from sorted file).
	if _, err := os.Stat(path + ".htab"); err != nil {
		t.Errorf(".htab missing after Flush: %v", err)
	}

	// No .tmp file should remain.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp file still present after Flush")
	}

	idx.Close()
}

// TestHashIndexDeltaStaleCleaned verifies that leftover delta files and a
// stale .htab from a previous interrupted run are removed when NewHashIndex
// is called, and a fresh .htab is created.
func TestHashIndexDeltaStaleCleaned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Plant stale delta files as if a previous run was interrupted.
	for f := range 3 {
		dp := fmt.Sprintf("%s.delta.%06d", path, f)
		if err := os.WriteFile(dp, []byte("stale"), 0644); err != nil {
			t.Fatalf("writing stale delta: %v", err)
		}
	}
	// Plant a stale .htab from an interrupted run.
	if err := os.WriteFile(path+".htab", []byte("corrupt"), 0644); err != nil {
		t.Fatalf("writing stale htab: %v", err)
	}

	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	defer idx.Close()

	for f := range 3 {
		dp := fmt.Sprintf("%s.delta.%06d", path, f)
		if _, err := os.Stat(dp); !os.IsNotExist(err) {
			t.Errorf("stale delta file %d was not cleaned up by NewHashIndex", f)
		}
	}
	// A fresh .htab should have been created (not the stale corrupt one).
	if _, err := os.Stat(path + ".htab"); err != nil {
		t.Errorf(".htab missing after NewHashIndex: %v", err)
	}
}

// TestHashTableStaleCleaned verifies that a stale .htab from an interrupted
// run is always replaced with a fresh one at NewHashIndex, even when the
// sorted index file is non-empty.
func TestHashTableStaleCleaned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Session 1: write a real sorted file.
	idx1, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("session1 NewHashIndex: %v", err)
	}
	for n := range 1000 {
		idx1.Insert(entryID(n), 1, uint64(n), 4096)
	}
	if err := idx1.Close(); err != nil {
		t.Fatalf("session1 Close: %v", err)
	}

	// Simulate interrupted run: overwrite .htab with garbage.
	if err := os.WriteFile(path+".htab", []byte("corrupt stale data"), 0644); err != nil {
		t.Fatalf("writing corrupt htab: %v", err)
	}

	// Session 2: NewHashIndex must ignore the corrupt .htab and rebuild.
	idx2, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("session2 NewHashIndex: %v", err)
	}
	defer idx2.Close()

	// All entries from session 1 must be findable via the fresh hash table.
	missing := 0
	for n := range 1000 {
		_, found, err := idx2.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%d/1000 entries not found after stale-htab rebuild", missing)
	}
}

// TestHashTableGrowth verifies that inserting enough entries to exceed the
// 85% load factor triggers growHashTable() transparently, and all entries
// remain findable after growth.
func TestHashTableGrowth(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Start with an empty index (htab has 1024 slots, threshold ~870).
	// Insert 3000 entries across multiple FlushDelta calls to force
	// multiple growth cycles.
	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	const total = 3000
	const batchSize = 500
	for start := 0; start < total; start += batchSize {
		for i := range batchSize {
			idx.Insert(entryID(start+i), uint32(start+i), uint64(start+i), 4096)
		}
		if err := idx.FlushDelta(); err != nil {
			t.Fatalf("FlushDelta at %d: %v", start, err)
		}
	}

	// All entries must be findable after growth cycles.
	missing := 0
	for n := range total {
		entry, found, err := idx.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			missing++
			continue
		}
		if entry == nil {
			t.Errorf("entry %d: nil IndexEntry", n)
			continue
		}
		if entry.PackNumber != uint32(n) {
			t.Errorf("entry %d: PackNumber got %d want %d", n, entry.PackNumber, n)
		}
	}
	if missing > 0 {
		t.Errorf("%d/%d entries missing after hash table growth", missing, total)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestHashIndexDeltaAtomicFlush verifies that after Flush the main file has the
// expected byte size (n × EntrySize) and no leftover .tmp or .delta.* files.
func TestHashIndexDeltaAtomicFlush(t *testing.T) {
	const total = 30_000
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	for n := range total {
		idx.Insert(entryID(n), 0, uint64(n), 4096)
		if (n+1)%10_000 == 0 {
			if err := idx.FlushDelta(); err != nil {
				t.Fatalf("FlushDelta: %v", err)
			}
		}
	}

	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat main file: %v", err)
	}
	want := int64(total * index.EntrySize)
	if info.Size() != want {
		t.Errorf("main file size: got %d, want %d (%d entries × %d bytes)", info.Size(), want, total, index.EntrySize)
	}

	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp should not exist after Flush")
	}

	idx.Close()
}

// TestHashIndexDeltaIncrementalGrowth simulates two backup sessions back-to-back:
// session 1 writes 150 000 entries and closes; session 2 reopens the index,
// adds 100 000 more entries via three FlushDelta calls, then closes.
// After session 2, all 250 000 entries must be findable.
func TestHashIndexDeltaIncrementalGrowth(t *testing.T) {
	const session1 = 150_000
	const session2 = 100_000

	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Session 1: build a large existing index.
	idx1, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("session1 NewHashIndex: %v", err)
	}
	for n := range session1 {
		idx1.Insert(entryID(n), 1, uint64(n), 4096)
	}
	if err := idx1.Close(); err != nil { // Close calls Flush
		t.Fatalf("session1 Close: %v", err)
	}

	info, _ := os.Stat(path)
	t.Logf("main index after session1: %d bytes (%d entries)", info.Size(), info.Size()/int64(index.EntrySize))

	// Session 2: reopen, add new entries in batches via FlushDelta.
	idx2, err := index.NewHashIndex(path, 4, false)
	if err != nil {
		t.Fatalf("session2 NewHashIndex: %v", err)
	}
	const batchSize = session2 / 4
	for b := range 4 {
		for i := range batchSize {
			n := session1 + b*batchSize + i
			idx2.Insert(entryID(n), 2, uint64(n), 8192)
		}
		// Flush first 3 batches to delta files; leave the 4th in-memory
		// so Close() exercises the path where Flush merges both delta
		// files and an in-memory residual.
		if b < 3 {
			if err := idx2.FlushDelta(); err != nil {
				t.Fatalf("session2 FlushDelta %d: %v", b, err)
			}
		}
	}
	if err := idx2.Close(); err != nil {
		t.Fatalf("session2 Close: %v", err)
	}

	// Verify: all session1 and session2 entries present in the merged index.
	idx3, err := index.NewHashIndex(path, 4, false)
	if err != nil {
		t.Fatalf("verify NewHashIndex: %v", err)
	}
	defer idx3.Close()

	const total = session1 + session2
	missing := 0
	for n := range total {
		if n%500 != 0 { // spot-check every 500th
			continue
		}
		_, found, err := idx3.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			missing++
			if missing <= 5 {
				t.Errorf("entry %d missing from merged index", n)
			}
		}
	}
	if missing > 0 {
		t.Errorf("%d spot-checked entries missing after incremental merge", missing)
	}

	info2, _ := os.Stat(path)
	t.Logf("merged index: %d bytes (%d entries)", info2.Size(), info2.Size()/int64(index.EntrySize))
}

// --- Tests that run against both LSM and memFlushed modes ---

// flushModes lists the two index modes under test.
var flushModes = []struct {
	name       string
	memFlushed bool
}{
	{"lsm", false},
	{"memFlushed", true},
}

// openIdx creates a HashIndex and, when memFlushed is true, enables the
// in-memory flushed set.
func openIdx(t *testing.T, path string, cacheMB int, memFlushed bool) *index.HashIndex {
	t.Helper()
	idx, err := index.NewHashIndex(path, cacheMB, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	idx.SetMemFlushed(memFlushed)
	return idx
}

// TestBothModesDedup verifies that entries flushed to delta files are still
// detected as duplicates by Lookup in both LSM and memFlushed modes.
func TestBothModesDedup(t *testing.T) {
	const total = 50_000
	for _, m := range flushModes {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			idx := openIdx(t, filepath.Join(dir, "hash-index.db"), 0, m.memFlushed)

			for n := range total {
				idx.Insert(entryID(n), 0, uint64(n), 4096)
			}
			if err := idx.FlushDelta(); err != nil {
				t.Fatalf("FlushDelta: %v", err)
			}

			// All entries must be found after FlushDelta clears the in-memory map.
			missing := 0
			for n := range total {
				_, found, err := idx.Lookup(entryID(n))
				if err != nil {
					t.Fatalf("Lookup %d: %v", n, err)
				}
				if !found {
					missing++
				}
			}
			if missing > 0 {
				t.Errorf("%d/%d entries not found after FlushDelta", missing, total)
			}

			// An entry never inserted must not be found.
			_, found, err := idx.Lookup(entryID(total + 1))
			if err != nil {
				t.Fatalf("Lookup absent: %v", err)
			}
			if found {
				t.Error("absent entry incorrectly found")
			}

			idx.Close()
		})
	}
}

// TestBothModesRoundTrip inserts entries across multiple batches with
// FlushDelta between each, then verifies all field values after a final Flush.
func TestBothModesRoundTrip(t *testing.T) {
	const total = 100_000
	const batches = 5
	const perBatch = total / batches

	for _, m := range flushModes {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			idx := openIdx(t, filepath.Join(dir, "hash-index.db"), 4, m.memFlushed)

			for b := range batches {
				for i := range perBatch {
					n := b*perBatch + i
					idx.Insert(entryID(n), uint32(b), uint64(n*48), uint32(4096+n%512))
				}
				if err := idx.FlushDelta(); err != nil {
					t.Fatalf("FlushDelta %d: %v", b, err)
				}
			}

			if err := idx.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			// After the final Flush every entry must be present with correct fields.
			for n := range total {
				if n%200 != 0 {
					continue
				}
				b := n / perBatch
				entry, found, err := idx.Lookup(entryID(n))
				if err != nil {
					t.Fatalf("Lookup %d: %v", n, err)
				}
				if !found {
					t.Fatalf("entry %d not found after Flush", n)
				}
				if entry == nil {
					t.Fatalf("entry %d: nil IndexEntry after Flush", n)
				}
				if entry.PackNumber != uint32(b) {
					t.Errorf("entry %d: PackNumber got %d want %d", n, entry.PackNumber, b)
				}
				if entry.StoreOffset != uint64(n*48) {
					t.Errorf("entry %d: StoreOffset got %d want %d", n, entry.StoreOffset, n*48)
				}
				if entry.ChunkLength != uint32(4096+n%512) {
					t.Errorf("entry %d: ChunkLength got %d want %d", n, entry.ChunkLength, 4096+n%512)
				}
			}

			if err := idx.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

// TestFlushDeltaNoFiles verifies that FlushDelta never creates .delta files;
// only the .htab file is used. After the final Flush, no delta or .tmp files
// remain.
func TestFlushDeltaNoFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")
	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	const flushes = 4
	for f := range flushes {
		for i := range 1000 {
			idx.Insert(entryID(f*1000+i), 0, uint64(f*1000+i), 4096)
		}
		if err := idx.FlushDelta(); err != nil {
			t.Fatalf("FlushDelta %d: %v", f, err)
		}
		// No delta files should be created.
		for k := range flushes {
			dp := fmt.Sprintf("%s.delta.%06d", path, k)
			if _, err := os.Stat(dp); !os.IsNotExist(err) {
				t.Errorf("delta file %d exists after FlushDelta — should never be created", k)
			}
		}
	}

	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// No temp or delta files remain.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error(".tmp still present after Flush")
	}
	for f := range flushes {
		dp := fmt.Sprintf("%s.delta.%06d", path, f)
		if _, err := os.Stat(dp); !os.IsNotExist(err) {
			t.Errorf("delta file %d present after Flush", f)
		}
	}
	idx.Close()
}

// TestBothModesMultiDelta exercises the session-merge threshold (LSM) and
// the equivalent path in memFlushed mode by performing more than mergeThreshold
// FlushDelta calls, then verifying all entries survive the final Flush.
func TestBothModesMultiDelta(t *testing.T) {
	const flushes = 15 // > mergeThreshold (10)
	const perFlush = 1000

	for _, m := range flushModes {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "hash-index.db")
			idx := openIdx(t, path, 0, m.memFlushed)

			for f := range flushes {
				for i := range perFlush {
					idx.Insert(entryID(f*perFlush+i), uint32(f), uint64(f*perFlush+i), 4096)
				}
				if err := idx.FlushDelta(); err != nil {
					t.Fatalf("FlushDelta %d: %v", f, err)
				}
			}

			if err := idx.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}

			const total = flushes * perFlush
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			want := int64(total * index.EntrySize)
			if info.Size() != want {
				t.Errorf("main file size: got %d want %d", info.Size(), want)
			}

			// No session or delta files should remain.
			if _, err := os.Stat(path + ".session"); !os.IsNotExist(err) {
				t.Error(".session file still present after Flush")
			}
			if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
				t.Error(".tmp file still present after Flush")
			}

			idx.Close()
		})
	}
}

// --- memFlushed-specific tests ---

// TestFlushDeltaFullEntryAvailable verifies that Lookup returns the full
// IndexEntry (non-nil, with correct fields) for entries flushed via
// FlushDelta, both before and after the final Flush. The hash table stores
// full entries, so location data is available at all times.
func TestFlushDeltaFullEntryAvailable(t *testing.T) {
	dir := t.TempDir()
	// SetMemFlushed is a no-op; passing true or false makes no difference.
	idx := openIdx(t, filepath.Join(dir, "hash-index.db"), 0, true)

	idx.Insert(entryID(1), 7, 999, 4096)
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}

	// Full entry must be available immediately after FlushDelta.
	entry, found, err := idx.Lookup(entryID(1))
	if err != nil {
		t.Fatalf("Lookup before Flush: %v", err)
	}
	if !found {
		t.Fatal("entry should be found after FlushDelta")
	}
	if entry == nil {
		t.Fatal("nil IndexEntry after FlushDelta — hash table must return full entry")
	}
	if entry.PackNumber != 7 || entry.StoreOffset != 999 {
		t.Errorf("wrong fields before Flush: %+v", entry)
	}

	// After Flush the full entry must still be returned.
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	entry, found, err = idx.Lookup(entryID(1))
	if err != nil {
		t.Fatalf("Lookup after Flush: %v", err)
	}
	if !found {
		t.Fatal("entry not found after Flush")
	}
	if entry == nil {
		t.Fatal("nil IndexEntry after Flush")
	}
	if entry.PackNumber != 7 || entry.StoreOffset != 999 {
		t.Errorf("wrong fields after Flush: %+v", entry)
	}

	idx.Close()
}

// TestMemFlushedNoSessionFile verifies that the memFlushed mode never creates
// a .session file, even when the number of FlushDelta calls exceeds the LSM
// merge threshold.
func TestMemFlushedNoSessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")
	idx := openIdx(t, path, 0, true)

	// Exceed mergeThreshold to confirm no session merge occurs.
	for f := range 12 {
		for i := range 500 {
			idx.Insert(entryID(f*500+i), 0, uint64(f*500+i), 4096)
		}
		if err := idx.FlushDelta(); err != nil {
			t.Fatalf("FlushDelta %d: %v", f, err)
		}
	}

	if _, err := os.Stat(path + ".session"); !os.IsNotExist(err) {
		t.Error(".session file created in memFlushed mode — should never happen")
	}

	idx.Close()
}

// TestSetMemFlushedToggle verifies that SetMemFlushed can switch modes before
// any FlushDelta calls and that the correct path is taken.
func TestSetMemFlushedToggle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	// Enable, then disable — should leave the index in LSM mode.
	idx.SetMemFlushed(true)
	idx.SetMemFlushed(false)

	idx.Insert(entryID(1), 1, 100, 4096)
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}

	// In LSM mode the full entry is available from the sorted delta file.
	entry, found, err := idx.Lookup(entryID(1))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("entry not found")
	}
	if entry == nil {
		t.Fatal("nil entry in LSM mode — expected full entry from binary search")
	}
	if entry.PackNumber != 1 {
		t.Errorf("PackNumber got %d want 1", entry.PackNumber)
	}

	idx.Close()
}

// TestBuildHashTableEmptyIndex is a regression test for a Windows-specific bug
// where Seek on a newly-created, never-written bucket temp file returned
// ERROR_INVALID_HANDLE ("The handle is invalid").  The fix replaced the
// Seek(0)+ReadFull pattern with ReadAt(buf, 0), which uses an absolute file
// offset and never touches the Windows file pointer.
//
// The test exercises the two critical transitions:
//   - buildHashTable with diskSize=0  (empty sorted file → all 8 buckets empty)
//   - buildHashTable with diskSize>0  (after Flush writes a non-empty sorted file)
//
// It also asserts that no bucket temp files (hash-index.db.b*.tmp) survive
// either call, since a Seek failure on Windows aborted the build before the
// deferred closeBuckets cleanup could run.
func TestBuildHashTableEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	assertNoBucketFiles := func(label string) {
		t.Helper()
		matches, err := filepath.Glob(path + ".b*.tmp")
		if err != nil {
			t.Fatalf("%s: glob bucket files: %v", label, err)
		}
		if len(matches) > 0 {
			t.Errorf("%s: bucket temp files not cleaned up: %v", label, matches)
		}
	}

	// Step 1: open a fresh index — buildHashTable runs with diskSize=0.
	// On the broken code this returned "seeking bucket 0: The handle is invalid".
	idx, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex on empty index: %v", err)
	}
	assertNoBucketFiles("after NewHashIndex (empty)")

	// A lookup on an empty index must return not-found without error.
	_, found, err := idx.Lookup(entryID(0))
	if err != nil {
		t.Fatalf("Lookup on empty htab: %v", err)
	}
	if found {
		t.Error("empty index should return not-found for any key")
	}

	// Step 2: insert entries and flush to the hash table.
	const n = 500
	for i := range n {
		idx.Insert(entryID(i), uint32(i), uint64(i*48), 4096)
	}
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}
	for i := range n {
		_, found, err := idx.Lookup(entryID(i))
		if err != nil {
			t.Fatalf("Lookup %d after FlushDelta: %v", i, err)
		}
		if !found {
			t.Errorf("entry %d not found after FlushDelta", i)
		}
	}

	// Step 3: Flush writes the sorted file and calls buildHashTable(diskSize>0),
	// exercising the ReadAt path on non-empty buckets.
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	assertNoBucketFiles("after Flush (non-empty)")

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Step 4: reopen — buildHashTable runs against the populated sorted file.
	idx2, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex on populated index: %v", err)
	}
	defer idx2.Close()
	assertNoBucketFiles("after reopen (populated)")

	// Spot-check that all entries survived the round-trip.
	for i := range n {
		if i%50 != 0 {
			continue
		}
		entry, found, err := idx2.Lookup(entryID(i))
		if err != nil {
			t.Fatalf("Lookup %d after reopen: %v", i, err)
		}
		if !found {
			t.Errorf("entry %d not found after reopen", i)
			continue
		}
		if entry.PackNumber != uint32(i) {
			t.Errorf("entry %d: PackNumber got %d want %d", i, entry.PackNumber, i)
		}
		if entry.StoreOffset != uint64(i*48) {
			t.Errorf("entry %d: StoreOffset got %d want %d", i, entry.StoreOffset, i*48)
		}
	}
}

// --- skipHtab tests ---

// TestSkipHtabNoFile verifies that NewHashIndex with skipHtab=true does not
// create a .htab file, and that ReadAll still returns all entries from the
// sorted file.
func TestSkipHtabNoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Session 1: build a populated index with htab.
	idx1, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	const total = 500
	for n := range total {
		idx1.Insert(entryID(n), uint32(n), uint64(n*48), 4096)
	}
	if err := idx1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Session 2: reopen with skipHtab=true.
	idx2, err := index.NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex(skipHtab): %v", err)
	}
	defer idx2.Close()

	// .htab must not exist.
	if _, err := os.Stat(path + ".htab"); !os.IsNotExist(err) {
		t.Error(".htab file should not exist when skipHtab=true")
	}

	// ReadAll must return all entries.
	all, err := idx2.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(all) != total {
		t.Errorf("ReadAll: got %d entries, want %d", len(all), total)
	}
}

// TestSkipHtabLookupFindsDiskEntries: Lookup with a skipped htab used to
// return not-found for entries sitting in the sorted file — that "graceful"
// behavior WAS bug #183 (read-only opens of downloaded indexes were blind).
// Lookup now falls back to binary-searching the sorted file.
func TestSkipHtabLookupFindsDiskEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Write one entry to the sorted file.
	idx1, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}
	idx1.Insert(entryID(1), 1, 100, 4096)
	idx1.Close()

	// Reopen with skipHtab.
	idx2, err := index.NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex(skipHtab): %v", err)
	}
	defer idx2.Close()

	// Lookup must find the on-disk entry via the sorted-file fallback.
	e, found, err := idx2.Lookup(entryID(1))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("Lookup missed an on-disk entry with htab skipped (#183)")
	}
	if e.PackNumber != 1 || e.StoreOffset != 100 || e.ChunkLength != 4096 {
		t.Fatalf("entry mismatch: %+v", e)
	}
	// And a genuinely absent hash still comes back clean.
	if _, found, err := idx2.Lookup(entryID(999)); err != nil || found {
		t.Fatalf("absent hash: found=%v err=%v", found, err)
	}
}

// TestSkipHtabInsertFlushRoundTrip exercises the staging-index pattern used by
// prune: open with skipHtab, Insert entries, Close (which calls Flush), then
// reopen and verify all entries are in the sorted file.
func TestSkipHtabInsertFlushRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	// Open with skipHtab, insert, close.
	idx1, err := index.NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex(skipHtab): %v", err)
	}
	const total = 1000
	for n := range total {
		idx1.Insert(entryID(n), uint32(n), uint64(n*48), uint32(4096+n%256))
	}
	if err := idx1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// .htab must not exist after close.
	if _, err := os.Stat(path + ".htab"); !os.IsNotExist(err) {
		t.Error(".htab should not exist after skipHtab Close")
	}

	// Sorted file should have the right size.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	want := int64(total * index.EntrySize)
	if info.Size() != want {
		t.Errorf("sorted file: got %d bytes, want %d", info.Size(), want)
	}

	// Reopen with htab and verify all entries via Lookup.
	idx2, err := index.NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex (verify): %v", err)
	}
	defer idx2.Close()

	missing := 0
	for n := range total {
		entry, found, err := idx2.Lookup(entryID(n))
		if err != nil {
			t.Fatalf("Lookup %d: %v", n, err)
		}
		if !found {
			missing++
			continue
		}
		if entry.PackNumber != uint32(n) {
			t.Errorf("entry %d: PackNumber got %d want %d", n, entry.PackNumber, n)
		}
		if entry.ChunkLength != uint32(4096+n%256) {
			t.Errorf("entry %d: ChunkLength got %d want %d", n, entry.ChunkLength, 4096+n%256)
		}
	}
	if missing > 0 {
		t.Errorf("%d/%d entries missing after skipHtab round-trip", missing, total)
	}
}

// TestSkipHtabFlushNoRebuild verifies that Flush with skipHtab=true writes the
// sorted file but does not create a .htab file.
func TestSkipHtabFlushNoRebuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex(skipHtab): %v", err)
	}

	for n := range 100 {
		idx.Insert(entryID(n), 0, uint64(n), 4096)
	}
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// No .htab after Flush.
	if _, err := os.Stat(path + ".htab"); !os.IsNotExist(err) {
		t.Error(".htab should not exist after skipHtab Flush")
	}

	// Sorted file should be written.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 100*index.EntrySize {
		t.Errorf("sorted file: got %d bytes, want %d", info.Size(), 100*index.EntrySize)
	}

	idx.Close()
}

// TestSkipHtabEmptyIndex verifies that skipHtab works on a brand-new empty
// index (no sorted file yet).
func TestSkipHtabEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")

	idx, err := index.NewHashIndex(path, 0, true)
	if err != nil {
		t.Fatalf("NewHashIndex(skipHtab): %v", err)
	}

	all, err := idx.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("expected 0 entries, got %d", len(all))
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func BenchmarkHashIndexLookup(b *testing.B) {
	for _, cacheMB := range []int{0, 1, 16} {
		name := "no-cache"
		if cacheMB > 0 {
			name = "cache"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "hash-index.db")

			idx, err := index.NewHashIndex(path, cacheMB, false)
			if err != nil {
				b.Fatalf("NewHashIndex: %v", err)
			}

			const numEntries = 10000
			var hashes [numEntries][32]byte
			for i := 0; i < numEntries; i++ {
				var buf [8]byte
				binary.BigEndian.PutUint64(buf[:], uint64(i))
				hashes[i] = sha256.Sum256(buf[:])
				idx.Insert(hasher.ChunkID{StrongHash: hashes[i]}, uint32(i), uint64(i*100), 4096)
			}
			idx.Flush()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				idx.Lookup(hasher.ChunkID{StrongHash: hashes[i%numEntries]})
			}

			idx.Close()
		})
	}
}
