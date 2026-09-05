// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// TestMissingBloomWithPopulatedIndexIsSuspect guards issue #16 (revised in the
// round-3 review): if bloom.bin is absent beside a populated hash-index.db, the
// open must SUCCEED — restore/verify/export use LookupDirect, which bypasses
// the bloom, and must keep working to recover intact data (failing at open
// permanently blocked managed repos, whose rebuild needs a controller). The
// corruption is instead surfaced via BloomSuspect(), which the backup write
// path checks (see pipeline.run) — a backup against this state would re-store
// the whole repo as duplicates.
func TestMissingBloomWithPopulatedIndexIsSuspect(t *testing.T) {
	dir := t.TempDir()

	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	var sh [32]byte
	sh[0] = 0x11
	idx.Insert(hasher.ChunkID{WeakHash: 42, StrongHash: sh}, 0, 0, 8192)
	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Delete the bloom filter, leaving a populated hash index behind.
	if err := os.Remove(filepath.Join(dir, "bloom.bin")); err != nil {
		t.Fatalf("removing bloom.bin: %v", err)
	}

	// Open must succeed (read paths depend on it)...
	reopened, err := index.NewDedupIndex(dir, 1000, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex with missing bloom must succeed for read paths (restore/verify/export), got: %v", err)
	}
	defer reopened.CloseDiscard()

	// ...LookupDirect must still find the chunk (bloom bypassed)...
	if _, found, err := reopened.LookupDirect(sh); err != nil || !found {
		t.Fatalf("LookupDirect after bloom loss: found=%v err=%v (restore would fail)", found, err)
	}

	// ...and the suspect state must be visible for the backup path to refuse.
	if !reopened.BloomSuspect() {
		t.Fatal("BloomSuspect() = false for a missing bloom beside a populated index; a backup would silently re-store everything")
	}

	// A genuinely fresh repo is NOT suspect.
	fresh, err := index.NewDedupIndex(t.TempDir(), 1000, 0.01, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex fresh: %v", err)
	}
	defer fresh.CloseDiscard()
	if fresh.BloomSuspect() {
		t.Fatal("BloomSuspect() = true for a fresh empty repo")
	}
}
