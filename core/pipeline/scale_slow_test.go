//go:build slow

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// writeLargeFile writes size bytes of random data to path in 4 MB chunks,
// avoiding a single large heap allocation.
func writeLargeFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	chunk := make([]byte, 4*1024*1024)
	var written int64
	for written < size {
		n := int64(len(chunk))
		if written+n > size {
			n = size - written
		}
		if _, err := rand.Read(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		written += n
	}
}

// modifyLargeFileRegion overwrites length bytes at offset with random data,
// also writing in 4 MB chunks to avoid a large heap allocation.
func modifyLargeFileRegion(t *testing.T, path string, offset, length int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	chunk := make([]byte, 4*1024*1024)
	var done int64
	for done < length {
		n := int64(len(chunk))
		if done+n > length {
			n = length - done
		}
		if _, err := rand.Read(chunk[:n]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(chunk[:n], offset+done); err != nil {
			t.Fatal(err)
		}
		done += n
	}
}

// hashFile returns the hex-encoded SHA-256 of the file at path.
func hashFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// restoreVolume runs a full volume restore of backupID into restorePath and
// returns the path.  It opens its own dedup index and chunk store.
func restoreVolume(t *testing.T, repoPath, backupID, restorePath string) {
	t.Helper()
	cfg := func() interface{} { return nil }
	_ = cfg

	b, err := manifest.Load(repoPath, backupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Re-open the dedup index from the repo.  expectedChunks and fpRate are
	// advisory; use conservative values for the restore path.
	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, 0.001, 64, nil)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	w, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	restorer := restore.NewRestorer(dedupIdx, chunkStore, newLogger())
	_, err = restorer.Restore(context.Background(), b, w)
	w.Close()
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
}

// TestScaleLargeFileVolumeMode backs up a 256 MB file with 4 workers, asserts
// at least 1000 chunks, verifies sort order, then restores and checks the
// output is byte-identical to the original.
func TestScaleLargeFileVolumeMode(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)
	cfg.HashWorkers = 4

	const size = 256 * 1024 * 1024
	writeLargeFile(t, sourcePath, size)

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

	if result.TotalChunks < 1000 {
		t.Errorf("TotalChunks: got %d, want >= 1000", result.TotalChunks)
	}
	assertEntriesSorted(t, repoPath, result.BackupID)

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	restoreVolume(t, repoPath, result.BackupID, restorePath)

	if hashFile(t, sourcePath) != hashFile(t, restorePath) {
		t.Fatal("restored data does not match source")
	}
	t.Logf("256 MB volume: %d chunks, ratio %.2f", result.TotalChunks, result.DedupRatio)
}

// TestScaleLargeFileIncrementalVolumeMode backs up a 256 MB file, modifies a
// 16 MB region, and verifies the incremental has both changed and unchanged chunks.
func TestScaleLargeFileIncrementalVolumeMode(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)
	cfg.HashWorkers = 4

	const size = 256 * 1024 * 1024
	writeLargeFile(t, sourcePath, size)

	p := pipeline.New(cfg, newLogger(), noEnc())

	reader1, err := volume.NewReader(sourcePath, 4*1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result1, err := p.Backup(context.Background(), reader1, sourcePath, reader1.Size(), repoPath)
	reader1.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Overwrite a 16 MB region at offset 64 MB.
	modifyLargeFileRegion(t, sourcePath, 64*1024*1024, 16*1024*1024)

	reader2, err := volume.NewReader(sourcePath, 4*1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result2, err := p.BackupIncremental(context.Background(), reader2, sourcePath, reader2.Size(), repoPath, result1.BackupID)
	reader2.Close()
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}

	if result2.ChangedChunks == 0 {
		t.Error("expected changed chunks after modifying 16 MB region")
	}
	if result2.UnchangedChunks == 0 {
		t.Error("expected unchanged chunks from unmodified regions")
	}
	t.Logf("256 MB incremental: %d changed, %d unchanged",
		result2.ChangedChunks, result2.UnchangedChunks)
}

// TestScaleLargeFileFileMode backs up 3 files of 50 MB each in file-mode,
// verifies the catalog has 3 regular entries, entries are sorted, and
// TotalChunks is proportional to the input size.
func TestScaleLargeFileFileMode(t *testing.T) {
	repoPath, _, cfg := setupRepo(t)
	sourceDir := t.TempDir()

	const (
		numFiles = 3
		fileSize = 50 * 1024 * 1024
	)

	for i := 0; i < numFiles; i++ {
		path := filepath.Join(sourceDir, fmt.Sprintf("large%d.bin", i))
		writeLargeFile(t, path, fileSize)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	result := fileModeFullBackup(t, p, sourceDir, repoPath)

	b, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var regularFiles int
	for _, f := range b.FileCatalog {
		if !f.IsDir && !f.IsSymlink {
			regularFiles++
		}
	}
	if regularFiles != numFiles {
		t.Errorf("FileCatalog regular entries: got %d, want %d", regularFiles, numFiles)
	}

	// At 64 KB max chunk size: 3 × 50 MB / 64 KB ≈ 2400 minimum chunks.
	const minChunks = int64(numFiles * fileSize / (64 * 1024))
	if result.TotalChunks < minChunks {
		t.Errorf("TotalChunks: got %d, want >= %d", result.TotalChunks, minChunks)
	}

	assertEntriesSorted(t, repoPath, result.BackupID)
	t.Logf("3 × 50 MB file-mode: %d total chunks, %d unique, ratio %.2f",
		result.TotalChunks, result.UniqueChunks, result.DedupRatio)
}
