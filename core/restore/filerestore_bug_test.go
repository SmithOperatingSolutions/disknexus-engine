// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
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
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestExtractFile_IgnoreErrors_ReportsSuccess is a regression test:
// ExtractFile must not report RestoredFiles=1 with nil error when the file
// restore actually failed under IgnoreErrors mode. It must return
// RestoredFiles=0 so the caller knows the extraction did not happen.
func TestExtractFile_IgnoreErrors_ReportsSuccess(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")

	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	// Create a backup referencing a chunk that is NOT in the store/index.
	// This simulates missing data (e.g. corrupted pack file).
	fakeHash := sha256.Sum256([]byte("non-existent-chunk"))
	backup := &manifest.Backup{
		BackupID:   "test-ignore-errors",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "data.bin", Size: 4096, Mode: 0644, StreamOffset: 0, StreamLength: 4096},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: fakeHash, ChunkLength: 4096},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.bin")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)
	restorer.IgnoreErrors = true

	result, err := restorer.ExtractFile(context.Background(), backup, "data.bin", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile with IgnoreErrors: %v", err)
	}
	if result.RestoredFiles != 0 {
		t.Errorf("RestoredFiles = %d, want 0 for a file that failed to restore", result.RestoredFiles)
	}
	if result.TotalFiles != 1 {
		t.Errorf("TotalFiles = %d, want 1", result.TotalFiles)
	}
}

// TestRestoreFiles_DirectoryPermissions is a regression test: directory
// permissions must be restored to their original values. Phase 1 creates
// directories with mode|0700 (so they're writable during restore); Phase 4
// must chmod them back to the original mode after all content is in place.
//
// Setup: backup a directory with mode 0555 (read+execute only). Restore it.
// Expected: restored directory has mode 0555.
func TestRestoreFiles_DirectoryPermissions(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")

	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	// Store a real chunk for the file inside the directory.
	chunkData := make([]byte, 1024)
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
	defer dedupIdx.Close()

	dedupIdx.Insert(hasher.ChunkID{StrongHash: chunkHash}, packNum, uint64(offset), uint32(len(chunkData)))
	if err := dedupIdx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Create backup with a directory that has restricted permissions (0555 = r-xr-xr-x).
	backup := &manifest.Backup{
		BackupID:   "test-dir-perms",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "restricted", IsDir: true, Mode: 0555},
			{Path: "restricted/file.bin", Size: 1024, Mode: 0644, StreamOffset: 0, StreamLength: 1024},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
		},
	}

	targetDir := filepath.Join(t.TempDir(), "restore-target")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	// The restored directory ends up mode 0555 (that's the point of the test),
	// which blocks t.TempDir's RemoveAll cleanup on non-root runners: deleting
	// file.bin requires write permission on its parent. Registered after
	// t.TempDir() so it runs first (cleanups are LIFO) and re-opens the dir.
	t.Cleanup(func() {
		os.Chmod(filepath.Join(targetDir, "restricted"), 0755)
	})

	_, err = restorer.RestoreFiles(context.Background(), backup, targetDir, nil)
	if err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}

	// Check the directory's permissions.
	info, err := os.Stat(filepath.Join(targetDir, "restricted"))
	if err != nil {
		t.Fatalf("Stat restored dir: %v", err)
	}

	gotMode := info.Mode().Perm()
	wantMode := os.FileMode(0555)
	if gotMode != wantMode {
		t.Errorf("restored directory mode = %o, want %o (permissions not restored)", gotMode, wantMode)
	}
}
