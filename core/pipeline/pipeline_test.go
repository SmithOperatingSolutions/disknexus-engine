// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// newLogger returns a silent logger suitable for tests.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func setupRepo(t *testing.T) (string, string, config.Config) {
	t.Helper()
	return setupRepoWithGeometry(t, config.Default())
}

// setupRepoFineGrained pins the pre-#148 8 KB geometry. The incremental
// STATS tests (added/removed/scale) compare chunk counts across small-file
// fixtures — under the 64 KB default a single chunk spans dozens of files,
// making those comparisons meaningless (surfaced as run-to-run Windows CI
// "flakes" whose failing member varied with unseeded fixture content).
func setupRepoFineGrained(t *testing.T) (string, string, config.Config) {
	t.Helper()
	cfg := config.Default()
	cfg.ChunkMinSize = 4 << 10
	cfg.ChunkAvgSize = 8 << 10
	cfg.ChunkMaxSize = 64 << 10
	cfg.BuzhashMask = uint64(8<<10) - 1
	return setupRepoWithGeometry(t, cfg)
}

func setupRepoWithGeometry(t *testing.T, cfg config.Config) (string, string, config.Config) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	sourcePath := filepath.Join(dir, "source.img")
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
	})

	return repoPath, sourcePath, cfg
}

func TestIncrementalUnchangedFile(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	// Create source file
	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	os.WriteFile(sourcePath, sourceData, 0644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, noEnc())

	// Full backup
	reader1, _ := volume.NewReader(sourcePath, 1024*1024)
	result1, err := p.Backup(context.Background(), reader1, sourcePath, reader1.Size(), repoPath)
	reader1.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	t.Logf("full backup: %d unique, %d dedup", result1.UniqueChunks, result1.DedupChunks)

	// Incremental backup of same unchanged file
	reader2, _ := volume.NewReader(sourcePath, 1024*1024)
	result2, err := p.BackupIncremental(context.Background(), reader2, sourcePath, reader2.Size(), repoPath, result1.BackupID)
	reader2.Close()
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}

	t.Logf("incremental: %d unique, %d dedup, %d changed, %d unchanged",
		result2.UniqueChunks, result2.DedupChunks, result2.ChangedChunks, result2.UnchangedChunks)

	// All chunks should be deduped — 0 new unique chunks
	if result2.UniqueChunks != 0 {
		t.Errorf("expected 0 new unique chunks, got %d", result2.UniqueChunks)
	}

	// All chunks should be unchanged vs parent
	if result2.ChangedChunks != 0 {
		t.Errorf("expected 0 changed chunks, got %d", result2.ChangedChunks)
	}

	if result2.UnchangedChunks != result2.TotalChunks {
		t.Errorf("expected all %d chunks unchanged, got %d", result2.TotalChunks, result2.UnchangedChunks)
	}

	if result2.ParentBackupID != result1.BackupID {
		t.Errorf("parent: got %q, want %q", result2.ParentBackupID, result1.BackupID)
	}
}

func TestIncrementalModifiedFile(t *testing.T) {
	// Fine-grained geometry: at the 64 KB default a 128 KB fixture is ~2
	// chunks, and a head modification can legitimately change both — the
	// "expected some unchanged chunks" assertion was probabilistic (flaked
	// on PR #165's unit leg and reproduces locally at -count=30).
	repoPath, sourcePath, cfg := setupRepoFineGrained(t)

	// Create source file
	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	os.WriteFile(sourcePath, sourceData, 0644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, noEnc())

	// Full backup
	reader1, _ := volume.NewReader(sourcePath, 1024*1024)
	result1, err := p.Backup(context.Background(), reader1, sourcePath, reader1.Size(), repoPath)
	reader1.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Modify a portion of the file
	modifiedData := make([]byte, len(sourceData))
	copy(modifiedData, sourceData)
	rand.Read(modifiedData[0 : 16*1024]) // modify first 16 KB
	os.WriteFile(sourcePath, modifiedData, 0644)

	// Incremental backup of modified file
	reader2, _ := volume.NewReader(sourcePath, 1024*1024)
	result2, err := p.BackupIncremental(context.Background(), reader2, sourcePath, reader2.Size(), repoPath, result1.BackupID)
	reader2.Close()
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}

	t.Logf("incremental: %d unique, %d dedup, %d changed, %d unchanged",
		result2.UniqueChunks, result2.DedupChunks, result2.ChangedChunks, result2.UnchangedChunks)

	// Should have some changed and some unchanged chunks
	if result2.ChangedChunks == 0 {
		t.Error("expected some changed chunks")
	}
	if result2.UnchangedChunks == 0 {
		t.Error("expected some unchanged chunks")
	}
	if result2.ChangedChunks+result2.UnchangedChunks != result2.TotalChunks {
		t.Errorf("changed(%d) + unchanged(%d) != total(%d)",
			result2.ChangedChunks, result2.UnchangedChunks, result2.TotalChunks)
	}
}

func TestParallelCompressionRoundTrip(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	// Use 4 workers to exercise parallel compression
	cfg.HashWorkers = 4

	// Create source data (256 KB of random data)
	sourceData := make([]byte, 256*1024)
	rand.Read(sourceData)
	os.WriteFile(sourcePath, sourceData, 0644)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, noEnc())

	// Backup
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	t.Logf("backup: %d chunks (%d unique), stored %d bytes",
		result.TotalChunks, result.UniqueChunks, result.StoredBytes)

	if result.TotalChunks == 0 {
		t.Fatal("expected at least one chunk")
	}
	if result.StoredBytes == 0 {
		t.Fatal("expected stored bytes > 0")
	}

	// Restore
	backup, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	_, err = restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Compare source and restored data
	restoredData, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(sourceData, restoredData) {
		t.Fatalf("restored data does not match source (source=%d bytes, restored=%d bytes)",
			len(sourceData), len(restoredData))
	}
}

// TestEntriesSortedByVolumeOffset verifies that the manifest entries written
// by a multi-worker backup are in strict ascending VolumeOffset order.
//
// Without the sequencer, parallel hash workers deliver chunks to Stage 3 in
// non-deterministic order, producing an unsorted sidecar. The binary-search
// helpers SearchEntries / SearchEntriesEnd silently return wrong indices when
// entries are unsorted, causing restore-files to produce correctly-sized but
// corrupt output. This test would fail intermittently on that old code.
func TestEntriesSortedByVolumeOffset(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	// Use many workers and the default chunk sizes (4–64 KB avg ~8 KB).
	// 2 MB / ~8 KB avg ≈ 250 chunks — enough to give workers many chances
	// to complete out of order.
	cfg.HashWorkers = 8
	source := make([]byte, 2*1024*1024)
	rand.Read(source)
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	reader, err := volume.NewReader(sourcePath, 4*1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if result.TotalChunks < 10 {
		t.Fatalf("expected >=10 chunks to exercise sequencer, got %d", result.TotalChunks)
	}

	assertEntriesSorted(t, repoPath, result.BackupID)
}

// TestEntriesSortedByVolumeOffsetIncremental applies the same ordering check
// to the incremental backup path, which also uses the parallel hash pipeline.
func TestEntriesSortedByVolumeOffsetIncremental(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)
	cfg.HashWorkers = 8

	source := make([]byte, 2*1024*1024)
	rand.Read(source)
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())

	reader1, err := volume.NewReader(sourcePath, 4*1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	full, err := p.Backup(context.Background(), reader1, sourcePath, reader1.Size(), repoPath)
	reader1.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Modify a region so the incremental has a mix of new and deduped chunks.
	rand.Read(source[512*1024 : 768*1024])
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reader2, err := volume.NewReader(sourcePath, 4*1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	incr, err := p.BackupIncremental(context.Background(), reader2, sourcePath, reader2.Size(), repoPath, full.BackupID)
	reader2.Close()
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}

	assertEntriesSorted(t, repoPath, incr.BackupID)
}

// assertEntriesSorted loads the entry accessor for backupID and verifies that
// every entry has a VolumeOffset >= the previous entry's VolumeOffset.
func assertEntriesSorted(t *testing.T, repoPath, backupID string) {
	t.Helper()
	ea, closer, err := manifest.NewEntryAccessor(repoPath, backupID)
	if err != nil {
		t.Fatalf("NewEntryAccessor: %v", err)
	}
	defer closer.Close()

	n := ea.Count()
	var prev int64 = -1
	for i := int64(0); i < n; i++ {
		e, err := ea.At(i)
		if err != nil {
			t.Fatalf("At(%d): %v", i, err)
		}
		if e.VolumeOffset < prev {
			t.Errorf("entries not sorted at index %d: VolumeOffset %d < previous %d",
				i, e.VolumeOffset, prev)
		}
		prev = e.VolumeOffset
	}
}
