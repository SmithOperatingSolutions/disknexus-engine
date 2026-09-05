// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestPrune_CorruptManifestDeletesLiveChunks is a regression test: prune
// must refuse to run when any manifest is unreadable. Skipping an unreadable
// manifest leaves the referenced hash set incomplete, so chunks exclusively
// owned by that manifest would be classified as orphans and permanently
// deleted. A transient I/O error must never cause irreversible data loss.
func TestPrune_CorruptManifestDeletesLiveChunks(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create two backups.
	data1 := make([]byte, 128*1024)
	rand.Read(data1)
	id1 := backupData(t, repoPath, cfg, data1)

	data2 := make([]byte, 128*1024)
	rand.Read(data2)
	_ = backupData(t, repoPath, cfg, data2)

	// Corrupt the first manifest so streamHashes fails.
	dnmPath := manifest.DNMPath(repoPath, id1)
	if err := os.WriteFile(dnmPath, []byte("corrupted"), 0644); err != nil {
		t.Fatalf("corrupting manifest: %v", err)
	}

	// Prune MUST return an error when it cannot read all manifests.
	// The referenced hash set is incomplete — proceeding would delete
	// chunks that may still be needed.
	_, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})

	if err == nil {
		t.Error("prune succeeded despite unreadable manifest — should return error when reference set is incomplete")
	}
}

// TestPrune_CrashRecoveryIndexChunksMismatch is a regression test: crash
// recovery must keep chunks/ and index/ consistent with each other.
//
// Simulated crash state: after atomicSwap moved chunks and old index aside,
// but before staging index was moved into place.
// Disk state:
//   - chunks/ = new compacted packs
//   - chunks.prune-old/ = old packs
//   - index.prune-old/ = old index (references old pack numbers)
//   - .prune-staging/index/ = new index (matches new packs)
//   - NO index/ directory
//
// Correct recovery: roll the swap forward using .prune-staging/index (which
// matches the current chunks), never the old index.
func TestPrune_CrashRecoveryIndexChunksMismatch(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create two backups and delete one to create orphans.
	data1 := make([]byte, 128*1024)
	rand.Read(data1)
	id1 := backupData(t, repoPath, cfg, data1)

	data2 := make([]byte, 128*1024)
	rand.Read(data2)
	id2 := backupData(t, repoPath, cfg, data2)

	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatalf("deleting manifest: %v", err)
	}

	// Run a real prune to get proper staging content, but simulate a crash
	// by manually recreating the mid-swap state.
	//
	// First, do the prune normally to generate correct staging.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("initial prune: %v", err)
	}
	if result.OrphanedChunks == 0 {
		t.Fatal("expected orphans from deleted backup")
	}

	// The prune completed successfully. Now we need to simulate the crash
	// state. We'll do another prune cycle with a second delete.
	// Instead, let's directly simulate the crash state from scratch.

	// Re-backup to have content to prune again.
	data3 := make([]byte, 64*1024)
	rand.Read(data3)
	id3 := backupData(t, repoPath, cfg, data3)
	_ = id3

	indexDir := filepath.Join(repoPath, "index")

	// Create the crash state manually:
	// 1. chunks/ stays as-is (the "new" compacted packs)
	// 2. chunks.prune-old/ has old chunks
	// 3. index.prune-old/ has old index
	// 4. .prune-staging/index/ has new index that matches chunks/
	// 5. NO index/ directory

	oldChunks := filepath.Join(repoPath, "chunks.prune-old")
	oldIndex := filepath.Join(repoPath, "index.prune-old")
	stagingDir := filepath.Join(repoPath, ".prune-staging")
	stagingIndex := filepath.Join(stagingDir, "index")

	// Make old dirs with dummy content (simulating pre-swap state).
	os.MkdirAll(oldChunks, 0755)
	os.WriteFile(filepath.Join(oldChunks, "0000.pack"), []byte("old-pack-data"), 0644)

	// Move current index to staging (this is the "new" index matching current chunks).
	os.MkdirAll(stagingDir, 0755)
	os.Rename(indexDir, stagingIndex)

	// Create old index pointing to wrong pack numbers.
	os.MkdirAll(oldIndex, 0755)
	os.WriteFile(filepath.Join(oldIndex, "hash-index.db"), []byte("stale-index"), 0644)

	// Move old index to index.prune-old (it's already there).
	// Now: chunks/ exists, chunks.prune-old/ exists, index.prune-old/ exists,
	// .prune-staging/index/ exists, index/ does NOT exist.

	// Verify index/ doesn't exist.
	if _, err := os.Stat(indexDir); !os.IsNotExist(err) {
		t.Fatal("index dir should not exist for crash simulation")
	}

	// Run prune — this triggers crash recovery.
	_, err = prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	// We accept either success or failure, but the index must be consistent.

	// After recovery, the index dir should exist and be usable.
	if _, statErr := os.Stat(indexDir); os.IsNotExist(statErr) {
		t.Fatal("index dir does not exist after crash recovery")
	}

	// Check that the staging index (which matched the current chunks) was
	// preserved, NOT the old stale index.
	staleData, readErr := os.ReadFile(filepath.Join(indexDir, "hash-index.db"))
	if readErr == nil && string(staleData) == "stale-index" {
		t.Error("crash recovery restored the OLD index instead of the staging index — index/chunks mismatch")
	}

	// The staging dir with the correct new index should not have been deleted
	// before its contents were moved into place.
	if _, statErr := os.Stat(stagingIndex); !os.IsNotExist(statErr) {
		// Staging still exists — recovery didn't clean up properly, but at
		// least data isn't lost.
		t.Log("staging index still exists after recovery (not fully cleaned up)")
	}

	// Try to actually restore backup 2 with the recovered state.
	if err == nil {
		backup2, loadErr := manifest.Load(repoPath, id2)
		if loadErr != nil {
			t.Logf("cannot load manifest after recovery: %v", loadErr)
			return
		}

		logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		dedupIdx, idxErr := index.NewDedupIndex(indexDir, 10000, cfg.BloomFPRate, 1)
		if idxErr != nil {
			t.Errorf("cannot open index after crash recovery: %v", idxErr)
			return
		}
		defer dedupIdx.Close()

		chunkStore, storeErr := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
		if storeErr != nil {
			t.Errorf("cannot open chunk store after crash recovery: %v", storeErr)
			return
		}
		defer chunkStore.Close()

		restorePath := filepath.Join(t.TempDir(), "restored.img")
		writer, writerErr := volume.NewWriter(restorePath)
		if writerErr != nil {
			t.Fatalf("opening writer: %v", writerErr)
		}
		defer writer.Close()

		restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
		_, restoreErr := restorer.Restore(context.Background(), backup2, writer)
		if restoreErr != nil {
			t.Errorf("restore failed after crash recovery: %v — index/chunks mismatch", restoreErr)
		}
	}
}
