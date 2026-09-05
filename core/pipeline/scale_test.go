// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
)

// TestScaleManySmallFiles backs up 5,000 small files spread across 50
// subdirectories and verifies the catalog count, chunk production, and sort order.
func TestScaleManySmallFiles(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	const (
		numDirs  = 50
		numFiles = 5000
	)
	sizes := []int{64, 128, 256, 512, 1024, 2048, 4096}

	for i := 0; i < numDirs; i++ {
		if err := os.MkdirAll(filepath.Join(sourceDir, fmt.Sprintf("dir%03d", i)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < numFiles; i++ {
		dir := fmt.Sprintf("dir%03d", i%numDirs)
		size := sizes[i%len(sizes)]
		path := filepath.Join(sourceDir, dir, fmt.Sprintf("file%04d.bin", i))
		if err := os.WriteFile(path, randData(size), 0644); err != nil {
			t.Fatal(err)
		}
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
		t.Errorf("FileCatalog regular files: got %d, want %d", regularFiles, numFiles)
	}
	if result.TotalChunks == 0 {
		t.Error("TotalChunks should be > 0")
	}

	assertEntriesSorted(t, repoPath, result.BackupID)
	t.Logf("5000 files: %d total chunks, %d unique", result.TotalChunks, result.UniqueChunks)
}

// TestScaleManySmallFilesIncremental backs up 2,000 files, modifies 10% of
// them, and verifies the incremental has both changed and unchanged chunks.
func TestScaleManySmallFilesIncremental(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	const numFiles = 2000
	sizes := []int{64, 128, 256, 512, 1024, 2048, 4096}

	paths := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		size := sizes[i%len(sizes)]
		path := filepath.Join(sourceDir, fmt.Sprintf("file%04d.bin", i))
		if err := os.WriteFile(path, randData(size), 0644); err != nil {
			t.Fatal(err)
		}
		paths[i] = path
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)
	if result1.TotalChunks == 0 {
		t.Fatal("full backup produced 0 chunks")
	}

	// Modify every 10th file (~10% of the tree).
	for i := 0; i < numFiles; i += 10 {
		size := sizes[i%len(sizes)]
		if err := os.WriteFile(paths[i], randData(size), 0644); err != nil {
			t.Fatal(err)
		}
	}

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	if result2.ChangedChunks == 0 {
		t.Error("ChangedChunks should be > 0 after modifying 10% of files")
	}
	if result2.UnchangedChunks == 0 {
		t.Error("UnchangedChunks should be > 0 (90% of files unchanged)")
	}
	if result2.ParentBackupID != result1.BackupID {
		t.Errorf("ParentBackupID: got %q, want %q", result2.ParentBackupID, result1.BackupID)
	}

	assertEntriesSorted(t, repoPath, result2.BackupID)
	t.Logf("2000 files incremental: %d changed, %d unchanged",
		result2.ChangedChunks, result2.UnchangedChunks)
}
