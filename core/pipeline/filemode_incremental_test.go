// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/filemode"
)

// fileModeFullBackup walks sourceDir and runs a full file-mode backup.
func fileModeFullBackup(t *testing.T, p *pipeline.Pipeline, sourceDir, repoPath string) *pipeline.Result {
	t.Helper()
	cat, err := filemode.Walk(context.Background(), []string{sourceDir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	p.SetFileCatalog("file", cat.SourcePaths, cat.Files)
	r := filemode.NewMultiFileReader(cat)
	result, err := p.Backup(context.Background(), r, sourceDir, cat.TotalSize, repoPath)
	r.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return result
}

// fileModeIncrementalBackup walks sourceDir and runs an incremental file-mode backup.
func fileModeIncrementalBackup(t *testing.T, p *pipeline.Pipeline, sourceDir, repoPath, parentID string) *pipeline.Result {
	t.Helper()
	cat, err := filemode.Walk(context.Background(), []string{sourceDir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	p.SetFileCatalog("file", cat.SourcePaths, cat.Files)
	r := filemode.NewMultiFileReader(cat)
	result, err := p.BackupIncremental(context.Background(), r, sourceDir, cat.TotalSize, repoPath, parentID)
	r.Close()
	if err != nil {
		t.Fatalf("BackupIncremental: %v", err)
	}
	return result
}

// loadWithHashes loads a backup manifest and computes per-file content hashes.
func loadWithHashes(t *testing.T, repoPath, backupID string) *manifest.Backup {
	t.Helper()
	b, err := manifest.Load(repoPath, backupID)
	if err != nil {
		t.Fatalf("Load(%s): %v", backupID, err)
	}
	filemode.ComputeContentHashes(b)
	return b
}

// findFile returns the FileEntry with the given relative path, or fails the test.
func findFile(t *testing.T, b *manifest.Backup, path string) manifest.FileEntry {
	t.Helper()
	for _, f := range b.FileCatalog {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("file %q not found in FileCatalog", path)
	return manifest.FileEntry{}
}

// randData returns n cryptographically random bytes.
func randData(n int) []byte {
	buf := make([]byte, n)
	rand.Read(buf)
	return buf
}

// TestFileModeIncrementalUnchanged verifies that an incremental backup of an
// unmodified file tree reports 0 changed chunks and all chunks as unchanged.
func TestFileModeIncrementalUnchanged(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "a.txt"), randData(64*1024), 0644)
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), randData(64*1024), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)
	if result1.TotalChunks == 0 {
		t.Fatal("full backup produced 0 chunks")
	}

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	if result2.ChangedChunks != 0 {
		t.Errorf("ChangedChunks: got %d, want 0", result2.ChangedChunks)
	}
	if result2.UnchangedChunks != result2.TotalChunks {
		t.Errorf("UnchangedChunks: got %d, want %d (all)", result2.UnchangedChunks, result2.TotalChunks)
	}
	if result2.ParentBackupID != result1.BackupID {
		t.Errorf("ParentBackupID: got %q, want %q", result2.ParentBackupID, result1.BackupID)
	}

	t.Logf("full: %d chunks; incremental: %d changed, %d unchanged",
		result1.TotalChunks, result2.ChangedChunks, result2.UnchangedChunks)
}

// TestFileModeIncrementalModifiedFile verifies that modifying one file in a
// two-file tree produces both changed and unchanged chunks in the incremental.
func TestFileModeIncrementalModifiedFile(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	aData := randData(128 * 1024)
	bData := randData(128 * 1024)
	os.WriteFile(filepath.Join(sourceDir, "a.txt"), aData, 0644)
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), bData, 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)

	// Modify the first 16 KB of b.txt; a.txt remains unchanged.
	copy(bData[:16*1024], randData(16*1024))
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), bData, 0644)

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	if result2.ChangedChunks == 0 {
		t.Error("expected some changed chunks from modified b.txt")
	}
	if result2.UnchangedChunks == 0 {
		t.Error("expected some unchanged chunks from unmodified a.txt")
	}
	if result2.ChangedChunks+result2.UnchangedChunks != result2.TotalChunks {
		t.Errorf("changed(%d) + unchanged(%d) != total(%d)",
			result2.ChangedChunks, result2.UnchangedChunks, result2.TotalChunks)
	}

	t.Logf("incremental: %d changed, %d unchanged (total %d)",
		result2.ChangedChunks, result2.UnchangedChunks, result2.TotalChunks)
}

// TestFileModeIncrementalAddedFile verifies that adding a new file in an
// incremental backup produces new unique chunks and a larger total.
func TestFileModeIncrementalAddedFile(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "a.txt"), randData(64*1024), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)

	// Add a new file before the incremental.
	os.WriteFile(filepath.Join(sourceDir, "c.txt"), randData(64*1024), 0644)

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	if result2.UniqueChunks == 0 {
		t.Error("expected new unique chunks from added c.txt")
	}
	if result2.TotalChunks <= result1.TotalChunks {
		t.Errorf("incremental TotalChunks(%d) should exceed full(%d) after adding a file",
			result2.TotalChunks, result1.TotalChunks)
	}
	if result2.UnchangedChunks == 0 {
		t.Error("expected some unchanged chunks from unmodified a.txt")
	}

	// Both a.txt and c.txt must appear in the incremental's FileCatalog.
	b2 := loadWithHashes(t, repoPath, result2.BackupID)
	var hasA, hasC bool
	for _, f := range b2.FileCatalog {
		switch f.Path {
		case "a.txt":
			hasA = true
		case "c.txt":
			hasC = true
		}
	}
	if !hasA {
		t.Error("incremental FileCatalog missing a.txt")
	}
	if !hasC {
		t.Error("incremental FileCatalog missing c.txt")
	}
}

// TestFileModeIncrementalRemovedFile verifies that removing a file from the
// source tree results in fewer total chunks and the file absent from the catalog.
func TestFileModeIncrementalRemovedFile(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	// Sized well past the 64 KB-avg default chunk geometry (#148): at
	// 64 KB per file the chunk counts of full-vs-incremental could tie on
	// unlucky CDC cut points (observed flake in CI).
	os.WriteFile(filepath.Join(sourceDir, "a.txt"), randData(1024*1024), 0644)
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), randData(1024*1024), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)

	// Remove b.txt before the incremental.
	os.Remove(filepath.Join(sourceDir, "b.txt"))

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	if result2.TotalChunks >= result1.TotalChunks {
		t.Errorf("incremental TotalChunks(%d) should be less than full(%d) after removing a file",
			result2.TotalChunks, result1.TotalChunks)
	}

	// b.txt must not appear in the incremental's FileCatalog.
	b2 := loadWithHashes(t, repoPath, result2.BackupID)
	for _, f := range b2.FileCatalog {
		if f.Path == "b.txt" {
			t.Error("incremental FileCatalog should not contain removed b.txt")
		}
	}
}

// TestFileModeIncrementalManifestFields verifies that the manifest for a
// file-mode incremental backup contains the expected metadata fields.
func TestFileModeIncrementalManifestFields(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("hello world\n"), 0644)
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), []byte("goodbye world\n"), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)

	// Modify a.txt so the incremental has at least one changed chunk.
	os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("hello modified\n"), 0644)

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)

	b1 := loadWithHashes(t, repoPath, result1.BackupID)
	b2 := loadWithHashes(t, repoPath, result2.BackupID)

	// Full backup fields.
	if b1.BackupMode != "file" {
		t.Errorf("full BackupMode: got %q, want 'file'", b1.BackupMode)
	}
	if len(b1.SourcePaths) != 1 || b1.SourcePaths[0] != sourceDir {
		t.Errorf("full SourcePaths: got %v, want [%s]", b1.SourcePaths, sourceDir)
	}
	if len(b1.FileCatalog) < 2 {
		t.Errorf("full FileCatalog: got %d entries, want >= 2", len(b1.FileCatalog))
	}

	// Incremental backup fields.
	if b2.BackupMode != "file" {
		t.Errorf("incremental BackupMode: got %q, want 'file'", b2.BackupMode)
	}
	if b2.ParentBackupID != result1.BackupID {
		t.Errorf("incremental ParentBackupID: got %q, want %q", b2.ParentBackupID, result1.BackupID)
	}
	if b2.BackupType != "incremental" {
		t.Errorf("incremental BackupType: got %q, want 'incremental'", b2.BackupType)
	}
	if len(b2.SourcePaths) != 1 || b2.SourcePaths[0] != sourceDir {
		t.Errorf("incremental SourcePaths: got %v, want [%s]", b2.SourcePaths, sourceDir)
	}
	if len(b2.FileCatalog) < 2 {
		t.Errorf("incremental FileCatalog: got %d entries, want >= 2", len(b2.FileCatalog))
	}
	if result2.ParentBackupID != result1.BackupID {
		t.Errorf("result2 ParentBackupID: got %q, want %q", result2.ParentBackupID, result1.BackupID)
	}
}

// TestFileModeIncrementalContentHashes verifies that all regular files have
// non-zero ContentHash after ComputeContentHashes, and that a file replaced
// with entirely new random content produces a different ContentHash.
//
// Note: ContentHash is NOT stable across backups for unchanged files when
// adjacent file content differs, because CDC chunk boundaries depend on the
// entire byte stream, not just individual file content.
func TestFileModeIncrementalContentHashes(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "stable.txt"), randData(64*1024), 0644)
	os.WriteFile(filepath.Join(sourceDir, "changing.txt"), randData(64*1024), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())
	result1 := fileModeFullBackup(t, p, sourceDir, repoPath)
	b1 := loadWithHashes(t, repoPath, result1.BackupID)

	// All regular files in the full backup must have a non-zero ContentHash.
	var zero [32]byte
	for _, f := range b1.FileCatalog {
		if !f.IsDir && !f.IsSymlink && f.StreamLength > 0 {
			if f.ContentHash == zero {
				t.Errorf("full: file %q has zero ContentHash", f.Path)
			}
		}
	}

	// Replace changing.txt with completely new random content.
	os.WriteFile(filepath.Join(sourceDir, "changing.txt"), randData(64*1024), 0644)

	result2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, result1.BackupID)
	b2 := loadWithHashes(t, repoPath, result2.BackupID)

	// All regular files in the incremental must have a non-zero ContentHash.
	for _, f := range b2.FileCatalog {
		if !f.IsDir && !f.IsSymlink && f.StreamLength > 0 {
			if f.ContentHash == zero {
				t.Errorf("incremental: file %q has zero ContentHash", f.Path)
			}
		}
	}

	// The file replaced with new random data must have a different ContentHash.
	changing1 := findFile(t, b1, "changing.txt")
	changing2 := findFile(t, b2, "changing.txt")
	if changing1.ContentHash == changing2.ContentHash {
		t.Error("changing.txt ContentHash should differ after replacement with new random data")
	}
}

// TestFileModeIncrementalChainOf3 verifies a 3-step backup chain:
// full → incr1 → incr2, checking parent references at each level.
func TestFileModeIncrementalChainOf3(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "a.txt"), randData(64*1024), 0644)
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), randData(32*1024), 0644)

	p := pipeline.New(cfg, newLogger(), noEnc())

	resultFull := fileModeFullBackup(t, p, sourceDir, repoPath)

	// Modify a.txt for the first incremental.
	os.WriteFile(filepath.Join(sourceDir, "a.txt"), randData(64*1024), 0644)
	resultIncr1 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, resultFull.BackupID)

	// Modify b.txt for the second incremental.
	os.WriteFile(filepath.Join(sourceDir, "b.txt"), randData(32*1024), 0644)
	resultIncr2 := fileModeIncrementalBackup(t, p, sourceDir, repoPath, resultIncr1.BackupID)

	// Verify parent chain in results.
	if resultIncr1.ParentBackupID != resultFull.BackupID {
		t.Errorf("incr1 result ParentBackupID: got %q, want %q", resultIncr1.ParentBackupID, resultFull.BackupID)
	}
	if resultIncr2.ParentBackupID != resultIncr1.BackupID {
		t.Errorf("incr2 result ParentBackupID: got %q, want %q", resultIncr2.ParentBackupID, resultIncr1.BackupID)
	}

	// Verify parent chain in loaded manifests.
	bFull := loadWithHashes(t, repoPath, resultFull.BackupID)
	bIncr1 := loadWithHashes(t, repoPath, resultIncr1.BackupID)
	bIncr2 := loadWithHashes(t, repoPath, resultIncr2.BackupID)

	if bFull.ParentBackupID != "" {
		t.Errorf("full ParentBackupID: got %q, want ''", bFull.ParentBackupID)
	}
	if bIncr1.ParentBackupID != resultFull.BackupID {
		t.Errorf("incr1 manifest ParentBackupID: got %q, want %q", bIncr1.ParentBackupID, resultFull.BackupID)
	}
	if bIncr2.ParentBackupID != resultIncr1.BackupID {
		t.Errorf("incr2 manifest ParentBackupID: got %q, want %q", bIncr2.ParentBackupID, resultIncr1.BackupID)
	}
	if bIncr1.BackupType != "incremental" {
		t.Errorf("incr1 BackupType: got %q, want 'incremental'", bIncr1.BackupType)
	}
	if bIncr2.BackupType != "incremental" {
		t.Errorf("incr2 BackupType: got %q, want 'incremental'", bIncr2.BackupType)
	}

	// All three backups must contain both files in their catalog.
	for _, b := range []*manifest.Backup{bFull, bIncr1, bIncr2} {
		var hasA, hasB bool
		for _, f := range b.FileCatalog {
			switch f.Path {
			case "a.txt":
				hasA = true
			case "b.txt":
				hasB = true
			}
		}
		if !hasA || !hasB {
			t.Errorf("backup %s: missing files (a=%v, b=%v)", b.BackupID, hasA, hasB)
		}
	}

	t.Logf("full: %d chunks | incr1: %d changed/%d unchanged | incr2: %d changed/%d unchanged",
		resultFull.TotalChunks,
		resultIncr1.ChangedChunks, resultIncr1.UnchangedChunks,
		resultIncr2.ChangedChunks, resultIncr2.UnchangedChunks)
}

// TestFileModeSymlinkInBackup verifies that a symlink in the source tree is
// captured in the FileCatalog with correct metadata and does not contribute
// chunk data to the backup stream.
func TestFileModeSymlinkInBackup(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	os.WriteFile(filepath.Join(sourceDir, "real.txt"), randData(64*1024), 0644)
	if err := os.Symlink("real.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	result := fileModeFullBackup(t, p, sourceDir, repoPath)

	b := loadWithHashes(t, repoPath, result.BackupID)

	var linkEntry *manifest.FileEntry
	for i := range b.FileCatalog {
		if b.FileCatalog[i].Path == "link.txt" {
			e := b.FileCatalog[i]
			linkEntry = &e
			break
		}
	}
	if linkEntry == nil {
		t.Fatal("link.txt not found in FileCatalog")
	}
	if !linkEntry.IsSymlink {
		t.Error("link.txt: IsSymlink should be true")
	}
	if linkEntry.LinkTarget != "real.txt" {
		t.Errorf("link.txt: LinkTarget=%q, want 'real.txt'", linkEntry.LinkTarget)
	}
	if linkEntry.StreamLength != 0 {
		t.Errorf("link.txt: StreamLength=%d, want 0 (symlinks have no chunk data)", linkEntry.StreamLength)
	}
	if result.TotalChunks == 0 {
		t.Error("backup should have chunks from real.txt")
	}
}

// TestFileModeDeepPathInBackup verifies that a file nested 10 directory levels
// deep has its full relative path correctly stored in the manifest and a
// non-zero ContentHash.
func TestFileModeDeepPathInBackup(t *testing.T) {
	repoPath, _, cfg := setupRepoFineGrained(t)
	sourceDir := t.TempDir()

	deep := filepath.Join(sourceDir, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf.txt"), randData(4*1024), 0644); err != nil {
		t.Fatal(err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	result := fileModeFullBackup(t, p, sourceDir, repoPath)

	b := loadWithHashes(t, repoPath, result.BackupID)

	want := "a/b/c/d/e/f/g/h/i/j/leaf.txt"
	leaf := findFile(t, b, want)

	var zero [32]byte
	if leaf.ContentHash == zero {
		t.Error("leaf.txt: ContentHash should be non-zero")
	}
}
