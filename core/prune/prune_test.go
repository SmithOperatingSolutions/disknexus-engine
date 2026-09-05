// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func setupRepo(t *testing.T) (string, config.Config) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
	})

	return repoPath, cfg
}

func backupData(t *testing.T, repoPath string, cfg config.Config, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.img")
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, pipeline.MustBind(store.RepoConfig{}, nil))

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("opening reader: %v", err)
	}
	defer reader.Close()

	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}

	return result.BackupID
}

func TestPruneNoOrphans(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create a backup.
	data := make([]byte, 64*1024)
	rand.Read(data)
	backupData(t, repoPath, cfg, data)

	// Prune — should find 0 orphans.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.OrphanedChunks != 0 {
		t.Errorf("expected 0 orphaned chunks, got %d", result.OrphanedChunks)
	}
	if result.ReferencedChunks != result.TotalChunks {
		t.Errorf("referenced=%d, total=%d", result.ReferencedChunks, result.TotalChunks)
	}
	if result.BytesReclaimed != 0 {
		t.Errorf("expected 0 bytes reclaimed, got %d", result.BytesReclaimed)
	}
}

func TestPruneDryRun(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create two backups with different data.
	data1 := make([]byte, 64*1024)
	rand.Read(data1)
	id1 := backupData(t, repoPath, cfg, data1)

	data2 := make([]byte, 64*1024)
	rand.Read(data2)
	backupData(t, repoPath, cfg, data2)

	// Delete first backup to create orphans.
	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatalf("deleting manifest: %v", err)
	}

	// Get chunk data size before.
	bytesBefore := dirSize(filepath.Join(repoPath, "chunks"))

	// Dry run — should show orphans but not modify anything.
	result, err := prune.Run(context.Background(), prune.Options{
		RepoPath: repoPath,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("prune dry-run: %v", err)
	}

	if result.OrphanedChunks == 0 {
		t.Error("expected orphaned chunks in dry run")
	}

	// Verify nothing changed on disk.
	bytesAfter := dirSize(filepath.Join(repoPath, "chunks"))
	if bytesAfter != bytesBefore {
		t.Errorf("dry run modified chunk data: before=%d, after=%d", bytesBefore, bytesAfter)
	}
}

func TestPruneAfterDelete(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create first backup.
	data1 := make([]byte, 128*1024)
	rand.Read(data1)
	id1 := backupData(t, repoPath, cfg, data1)

	// Create second backup with completely different data (no shared chunks).
	data2 := make([]byte, 128*1024)
	rand.Read(data2)
	id2 := backupData(t, repoPath, cfg, data2)

	// Delete first backup.
	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatalf("deleting manifest: %v", err)
	}

	// Prune.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.OrphanedChunks == 0 {
		t.Error("expected orphaned chunks")
	}
	if result.BytesReclaimed <= 0 {
		t.Error("expected bytes to be reclaimed")
	}

	t.Logf("pruned: %d orphaned of %d total, reclaimed %d bytes",
		result.OrphanedChunks, result.TotalChunks, result.BytesReclaimed)

	// Verify second backup still restores correctly.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	backup2, err := manifest.Load(repoPath, id2)
	if err != nil {
		t.Fatalf("loading surviving manifest: %v", err)
	}

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening index: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer chunkStore.Close()

	// Restore to a temp file.
	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("opening writer: %v", err)
	}
	defer writer.Close()

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	_, err = restorer.Restore(context.Background(), backup2, writer)
	if err != nil {
		t.Fatalf("restore failed after prune: %v", err)
	}

	// Verify restored data matches original.
	restored, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(restored, data2) {
		t.Error("restored data does not match original after prune")
	}
}

func TestPruneCrashRecovery(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Create a backup so the repo has content.
	data := make([]byte, 64*1024)
	rand.Read(data)
	backupData(t, repoPath, cfg, data)

	// Simulate a crashed prune by creating leftover dirs.
	stagingDir := filepath.Join(repoPath, ".prune-staging")
	os.MkdirAll(filepath.Join(stagingDir, "chunks"), 0755)
	os.MkdirAll(filepath.Join(stagingDir, "index"), 0755)

	// Write a dummy file in staging to ensure it gets cleaned up.
	os.WriteFile(filepath.Join(stagingDir, "chunks", "dummy"), []byte("test"), 0644)

	// Run prune — should recover (clean up staging) and then prune normally.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune after crash recovery: %v", err)
	}

	// Staging dir should be gone.
	if _, err := os.Stat(stagingDir); !os.IsNotExist(err) {
		t.Error("staging dir still exists after recovery")
	}

	// No orphans expected since we didn't delete any backups.
	if result.OrphanedChunks != 0 {
		t.Errorf("expected 0 orphaned chunks, got %d", result.OrphanedChunks)
	}
}

func TestPruneEmptyRepo(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Put some chunks in the repo without any manifest referencing them.
	// This simulates orphaned chunks from e.g. a crashed backup.
	cs, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening index: %v", err)
	}

	for i := 0; i < 5; i++ {
		chunk := make([]byte, 4096)
		rand.Read(chunk)
		id := hasher.Sum(chunk)
		packNum, offset, _, err := cs.Store(chunk)
		if err != nil {
			t.Fatalf("storing chunk: %v", err)
		}
		dedupIdx.Insert(id, packNum, uint64(offset), uint32(len(chunk)))
	}

	dedupIdx.Close()
	cs.Close()

	// Prune — no manifests, all chunks are orphaned.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.OrphanedChunks != 5 {
		t.Errorf("expected 5 orphaned chunks, got %d", result.OrphanedChunks)
	}
	if result.ReferencedChunks != 0 {
		t.Errorf("expected 0 referenced chunks, got %d", result.ReferencedChunks)
	}
}

// TestPruneDuplicateChunks verifies that prune detects and removes duplicate
// index entries — the same StrongHash pointing to two different pack locations.
//
// Duplicates can occur after an interrupted backup followed by a retry, or
// after running index rebuild. The injection technique: after a normal backup,
// re-open the DedupIndex, Insert each existing entry again with a fake second
// pack location, then call ReadAllEntries (which calls Flush). Flush merges
// the on-disk entries with the new in-memory entries without deduplicating,
// producing a hash-index.db that has each hash twice.
func TestPruneDuplicateChunks(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Back up random data to populate the index.
	data := make([]byte, 128*1024)
	rand.Read(data)
	backupData(t, repoPath, cfg, data)

	indexDir := filepath.Join(repoPath, "index")

	// Read the current index entries.
	idx, err := index.NewDedupIndex(indexDir, 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening index: %v", err)
	}
	existing, err := idx.ReadAllEntries()
	if err != nil {
		idx.Close()
		t.Fatalf("reading index entries: %v", err)
	}
	idx.Close()

	if len(existing) == 0 {
		t.Fatal("backup produced no index entries")
	}

	// Inject duplicates directly into the sorted index file: write each
	// record twice in place (keeping sort order). This mirrors what index
	// rebuild-all produces when a chunk's frame appears more than once
	// across pack files. (Flush itself dedupes same-hash entries in favor
	// of the in-memory buffer, so duplicates cannot be injected via Insert.)
	indexPath := filepath.Join(indexDir, "hash-index.db")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("reading index file: %v", err)
	}
	dup := make([]byte, 0, len(raw)*2)
	for off := 0; off+index.EntrySize <= len(raw); off += index.EntrySize {
		rec := raw[off : off+index.EntrySize]
		dup = append(dup, rec...)
		dup = append(dup, rec...)
	}
	if err := os.WriteFile(indexPath, dup, 0644); err != nil {
		t.Fatalf("writing duplicated index file: %v", err)
	}

	idx2, err := index.NewDedupIndex(indexDir, 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening index after injection: %v", err)
	}
	after, err := idx2.ReadAllEntries()
	idx2.Close()
	if err != nil {
		t.Fatalf("reading injected duplicates: %v", err)
	}
	if len(after) != len(existing)*2 {
		t.Fatalf("expected %d entries after injection (got %d); injection did not produce duplicates",
			len(existing)*2, len(after))
	}

	// Run prune — it should detect and collapse the duplicates.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}

	if result.DuplicateChunks != int64(len(existing)) {
		t.Errorf("expected %d duplicate chunks, got %d", len(existing), result.DuplicateChunks)
	}
	if result.OrphanedChunks != 0 {
		t.Errorf("expected 0 orphaned chunks, got %d", result.OrphanedChunks)
	}

	// Verify the index after prune has no duplicates.
	idx3, err := index.NewDedupIndex(indexDir, 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening post-prune index: %v", err)
	}
	final, err := idx3.ReadAllEntries()
	idx3.Close()
	if err != nil {
		t.Fatalf("reading post-prune index: %v", err)
	}

	seen := make(map[[32]byte]bool, len(final))
	for _, e := range final {
		if seen[e.StrongHash] {
			t.Errorf("post-prune index still has duplicate StrongHash: %x", e.StrongHash[:8])
		}
		seen[e.StrongHash] = true
	}
	if len(final) != len(existing) {
		t.Errorf("expected %d unique entries after prune, got %d", len(existing), len(final))
	}
}

func dirSize(path string) int64 {
	var total int64
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}
