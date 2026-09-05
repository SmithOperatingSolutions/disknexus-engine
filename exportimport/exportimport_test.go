// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/exportimport"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

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

func doBackup(t *testing.T, repoPath string, data []byte, cfg config.Config) string {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "source.img")
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := pipeline.New(cfg, newLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return result.BackupID
}

// totalPackBytes returns the total size in bytes of all pack files in repoPath.
func totalPackBytes(t *testing.T, repoPath string) int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "chunks"))
	if err != nil {
		t.Fatalf("ReadDir chunks: %v", err)
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err == nil {
			total += info.Size()
		}
	}
	return total
}

// TestExportImportRoundTrip verifies that exported data can be imported into a
// fresh repo and restored byte-for-byte identically.
func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()

	repoPath, cfg := setupRepo(t)
	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	backupID := doBackup(t, repoPath, sourceData, cfg)

	// Export to zip.
	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	if err := exportimport.Export(repoPath, []string{backupID}, zipPath, nil); err != nil {
		t.Fatalf("Export: %v", err)
	}
	info, err := os.Stat(zipPath)
	if err != nil {
		t.Fatalf("zip not created: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("zip is empty")
	}

	// Import into a fresh repo.
	repoPath2, _ := setupRepo(t)
	if err := exportimport.Import(ctx, repoPath2, zipPath, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// Verify the backup manifest is present.
	b, err := manifest.Load(repoPath2, backupID)
	if err != nil {
		t.Fatalf("manifest.Load after import: %v", err)
	}
	if b.BackupID != backupID {
		t.Errorf("backup ID mismatch: got %q, want %q", b.BackupID, backupID)
	}

	// Restore from dest repo and verify byte-exact match.
	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath2, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath2, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	restorer := restore.NewRestorer(dedupIdx, chunkStore, newLogger())
	if _, err := restorer.Restore(ctx, b, writer); err != nil {
		writer.Close()
		t.Fatalf("Restore: %v", err)
	}
	writer.Close()

	restoredData, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(sourceData, restoredData) {
		t.Fatalf("restored data does not match source: source=%d bytes, restored=%d bytes",
			len(sourceData), len(restoredData))
	}
}

// TestExportDeduplicatesChunks verifies that exporting two backups with
// identical content produces only one copy of each chunk in the zip.
func TestExportDeduplicatesChunks(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	id1 := doBackup(t, repoPath, sourceData, cfg)
	id2 := doBackup(t, repoPath, sourceData, cfg)

	zipPath := filepath.Join(t.TempDir(), "both.zip")
	if err := exportimport.Export(repoPath, []string{id1, id2}, zipPath, nil); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Count chunk entries in the zip.
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer r.Close()

	var chunkCount int
	for _, f := range r.File {
		if strings.HasPrefix(f.Name, "chunks/") && !f.FileInfo().IsDir() {
			chunkCount++
		}
	}

	// Count unique chunk hashes in backup 1 (same as backup 2 since data is identical).
	b1, err := manifest.Load(repoPath, id1)
	if err != nil {
		t.Fatalf("Load backup 1: %v", err)
	}
	seen := make(map[[32]byte]struct{})
	for _, e := range b1.Entries {
		if !e.IsExcluded {
			seen[e.ChunkHash] = struct{}{}
		}
	}
	uniqueCount := len(seen)

	if chunkCount != uniqueCount {
		t.Errorf("expected %d unique chunks in zip, got %d (dedup failed)", uniqueCount, chunkCount)
	}
}

// TestImportSkipsExistingChunks verifies that importing a zip into the same repo
// it was exported from writes no new pack data (all chunks already present).
func TestImportSkipsExistingChunks(t *testing.T) {
	ctx := context.Background()
	repoPath, cfg := setupRepo(t)

	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	backupID := doBackup(t, repoPath, sourceData, cfg)

	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	if err := exportimport.Export(repoPath, []string{backupID}, zipPath, nil); err != nil {
		t.Fatalf("Export: %v", err)
	}

	bytesBefore := totalPackBytes(t, repoPath)

	// Import into the same repo — chunks already exist, should be skipped.
	if err := exportimport.Import(ctx, repoPath, zipPath, nil); err != nil {
		t.Fatalf("Import into same repo: %v", err)
	}

	bytesAfter := totalPackBytes(t, repoPath)
	if bytesAfter != bytesBefore {
		t.Errorf("pack bytes changed after re-import: before=%d after=%d (expected no new chunks)",
			bytesBefore, bytesAfter)
	}

	// Manifest should still be loadable.
	if _, err := manifest.Load(repoPath, backupID); err != nil {
		t.Fatalf("manifest.Load after re-import: %v", err)
	}
}

// TestExportUnknownBackupID verifies that exporting a non-existent backup ID returns an error.
func TestExportUnknownBackupID(t *testing.T) {
	repoPath, _ := setupRepo(t)

	err := exportimport.Export(repoPath, []string{"nonexistent-backup-id"}, filepath.Join(t.TempDir(), "out.zip"), nil)
	if err == nil {
		t.Fatal("expected error for unknown backup ID, got nil")
	}
}

// TestImportTruncatedZip verifies that importing an empty/truncated zip returns an error.
func TestImportTruncatedZip(t *testing.T) {
	ctx := context.Background()
	repoPath, _ := setupRepo(t)

	err := exportimport.Import(ctx, repoPath, "/dev/null", nil)
	if err == nil {
		t.Fatal("expected error for truncated/empty zip, got nil")
	}
}
