// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// makeChunkID creates a hasher.ChunkID from a strong hash for testing.
func makeChunkID(strongHash [32]byte) hasher.ChunkID {
	return hasher.ChunkID{StrongHash: strongHash}
}

// TestRestoreRoundTrip is the most important test in Phase 2.
// It backs up a file, loads the manifest, restores to a new file,
// and byte-compares original vs restored.
func TestRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	sourcePath := filepath.Join(dir, "source.img")
	restorePath := filepath.Join(dir, "restored.img")

	// Create source file with mixed content
	sourceData := make([]byte, 256*1024) // 256 KB
	rand.Read(sourceData)
	// Add some repeating patterns for dedup
	copy(sourceData[64*1024:128*1024], sourceData[0:64*1024])

	if err := os.WriteFile(sourcePath, sourceData, 0644); err != nil {
		t.Fatalf("writing source: %v", err)
	}

	// Initialize repo
	cfg := config.Default()
	repoCfg := store.RepoConfig{
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
	}
	if err := store.InitRepo(repoPath, repoCfg); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// Backup
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, pipeline.MustBind(store.RepoConfig{}, nil))

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	t.Logf("backup ID: %s, chunks: %d unique, %d dedup", result.BackupID, result.UniqueChunks, result.DedupChunks)

	// Load manifest
	backup, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatalf("Load manifest: %v", err)
	}

	// Open index and store for restore
	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	// Restore
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	restoreResult, err := restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	t.Logf("restore: %d chunks, %d bytes in %s", restoreResult.RestoredChunks, restoreResult.BytesWritten, restoreResult.Duration)

	// Byte-compare
	restoredData, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("reading restored: %v", err)
	}

	if !bytes.Equal(sourceData, restoredData) {
		t.Fatalf("RESTORE MISMATCH: source (%d bytes) != restored (%d bytes)", len(sourceData), len(restoredData))
	}

	t.Log("round-trip verified: source and restored files are byte-identical")
}

func TestRestoreExcludedChunks(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	restorePath := filepath.Join(dir, "restored.img")

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	// Store a real chunk
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	chunkData := make([]byte, 4096)
	rand.Read(chunkData)
	packNum, offset, _, err := chunkStore.Store(chunkData)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	chunkHash := sha256.Sum256(chunkData)

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}

	dedupIdx.Insert(makeChunkID(chunkHash), packNum, uint64(offset), uint32(len(chunkData)))
	dedupIdx.Flush()

	// Create a manifest with one real chunk and one excluded chunk
	backup := &manifest.Backup{
		BackupID:   "test-excluded",
		TotalBytes: 8192,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: 4096},
			{VolumeOffset: 4096, ChunkLength: 4096, IsExcluded: true},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	writer, _ := volume.NewWriter(restorePath)

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	result, err := restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	dedupIdx.Close()
	chunkStore.Close()

	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if result.RestoredChunks != 1 {
		t.Errorf("RestoredChunks: got %d, want 1", result.RestoredChunks)
	}
	if result.ExcludedChunks != 1 {
		t.Errorf("ExcludedChunks: got %d, want 1", result.ExcludedChunks)
	}

	// Verify the restored file
	data, _ := os.ReadFile(restorePath)
	if !bytes.Equal(data[0:4096], chunkData) {
		t.Error("chunk data mismatch at offset 0")
	}

	// Excluded region should be zeros
	for i := 4096; i < 8192; i++ {
		if data[i] != 0 {
			t.Errorf("expected zero at offset %d, got %d", i, data[i])
			break
		}
	}
}

func TestRestoreCorruptedChunk(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	restorePath := filepath.Join(dir, "restored.img")

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	chunkStore, _ := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	chunkData := make([]byte, 4096)
	rand.Read(chunkData)
	packNum, offset, _, _ := chunkStore.Store(chunkData)

	// Use a WRONG hash in the manifest — should trigger integrity error
	wrongHash := sha256.Sum256([]byte("wrong data"))

	dedupIdx, _ := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	dedupIdx.Insert(makeChunkID(wrongHash), packNum, uint64(offset), uint32(len(chunkData)))
	dedupIdx.Flush()

	backup := &manifest.Backup{
		BackupID:   "test-corrupt",
		TotalBytes: 4096,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: wrongHash, ChunkLength: 4096},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	writer, _ := volume.NewWriter(restorePath)

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	_, err := restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	dedupIdx.Close()
	chunkStore.Close()

	if err == nil {
		t.Fatal("expected error for corrupted chunk")
	}
	t.Logf("correctly caught corruption: %v", err)
}

func TestRestoreMissingChunk(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	restorePath := filepath.Join(dir, "restored.img")

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	chunkStore, _ := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	dedupIdx, _ := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)

	// Manifest references a chunk that doesn't exist in the index
	missingHash := sha256.Sum256([]byte("nonexistent"))
	backup := &manifest.Backup{
		BackupID:   "test-missing",
		TotalBytes: 4096,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: missingHash, ChunkLength: 4096},
		},
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	writer, _ := volume.NewWriter(restorePath)

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	_, err := restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	dedupIdx.Close()
	chunkStore.Close()

	if err == nil {
		t.Fatal("expected error for missing chunk")
	}
	t.Logf("correctly caught missing chunk: %v", err)
}

func TestRestoreContextCancellation(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	restorePath := filepath.Join(dir, "restored.img")

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	chunkStore, _ := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	dedupIdx, _ := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)

	chunkData := make([]byte, 4096)
	rand.Read(chunkData)
	hash := sha256.Sum256(chunkData)
	packNum, offset, _, _ := chunkStore.Store(chunkData)
	dedupIdx.Insert(makeChunkID(hash), packNum, uint64(offset), 4096)
	dedupIdx.Flush()

	entries := make([]manifest.Entry, 1000)
	for i := range entries {
		entries[i] = manifest.Entry{VolumeOffset: int64(i) * 4096, ChunkHash: hash, ChunkLength: 4096}
	}
	backup := &manifest.Backup{BackupID: "test-cancel", TotalBytes: 4096 * 1000, Entries: entries}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	writer, _ := volume.NewWriter(restorePath)

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	_, err := restorer.Restore(ctx, backup, writer)
	writer.Close()
	dedupIdx.Close()
	chunkStore.Close()

	if err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestVerifyIntactBackup(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	sourcePath := filepath.Join(dir, "source.img")

	sourceData := make([]byte, 64*1024)
	rand.Read(sourceData)
	os.WriteFile(sourcePath, sourceData, 0644)

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, pipeline.MustBind(store.RepoConfig{}, nil))

	reader, _ := volume.NewReader(sourcePath, 1024*1024)
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	backup, _ := manifest.Load(repoPath, result.BackupID)

	dedupIdx, _ := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	defer dedupIdx.Close()
	chunkStore, _ := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	defer chunkStore.Close()

	verifyResult, err := restore.Verify(context.Background(), backup, dedupIdx, chunkStore)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if !verifyResult.OK() {
		for _, e := range verifyResult.Errors {
			t.Errorf("verify error: %v", e)
		}
		t.Fatalf("verify found %d errors", len(verifyResult.Errors))
	}

	t.Logf("verified %d chunks in %s", verifyResult.VerifiedChunks, verifyResult.Duration)
}

func TestVerifyCorruptedPack(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	sourcePath := filepath.Join(dir, "source.img")

	sourceData := make([]byte, 64*1024)
	rand.Read(sourceData)
	os.WriteFile(sourcePath, sourceData, 0644)

	cfg := config.Default()
	store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, pipeline.MustBind(store.RepoConfig{}, nil))

	reader, _ := volume.NewReader(sourcePath, 1024*1024)
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	backup, _ := manifest.Load(repoPath, result.BackupID)

	// Corrupt the pack file
	packPath := filepath.Join(repoPath, "chunks", "0000.pack")
	f, err := os.OpenFile(packPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("opening pack: %v", err)
	}
	info, _ := f.Stat()
	if info.Size() > 20 {
		f.WriteAt([]byte{0xFF, 0xFF, 0xFF, 0xFF}, info.Size()/2)
	}
	f.Close()

	dedupIdx, _ := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	defer dedupIdx.Close()
	chunkStore, _ := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	defer chunkStore.Close()

	verifyResult, err := restore.Verify(context.Background(), backup, dedupIdx, chunkStore)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	if verifyResult.OK() {
		t.Fatal("expected verification errors for corrupted pack")
	}

	t.Logf("correctly detected %d errors in corrupted pack", len(verifyResult.Errors))
	for _, e := range verifyResult.Errors {
		t.Logf("  %v", e)
	}
}
