// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func excludedBackup() *manifest.Backup {
	return &manifest.Backup{
		BackupID:   "test-excluded-entry",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "pagefile.sys", Size: 4096, Mode: 0644, IsExcluded: true,
				VolumeExtents: []manifest.VolumeExtent{{FileOffset: 0, VolumeOffset: 8192, Length: 4096}},
			},
		},
	}
}

// TestExtractExcludedEntryRefused (#94): a catalog entry whose blocks were
// zeroed by the capture exclusion map must be refused with an error that says
// why — never silently restored as zero-filled content.
func TestExtractExcludedEntryRefused(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	out := filepath.Join(t.TempDir(), "pagefile.sys")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	_, err := restorer.ExtractFile(context.Background(), excludedBackup(), "pagefile.sys", out)
	if err == nil {
		t.Fatal("excluded entry restored without error (would be silent zeros)")
	}
	if !strings.Contains(err.Error(), "excluded") {
		t.Fatalf("error does not explain the exclusion: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("output file was created for a refused excluded entry")
	}
}

// TestRestoreFilesSkipsExcludedWithIgnoreErrors: under IgnoreErrors the
// excluded entry is skipped (logged) while other files still restore.
func TestRestoreFilesSkipsExcludedWithIgnoreErrors(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	b := excludedBackup()
	// A healthy restorable neighbor via the volume-extent path.
	b.Entries = []manifest.Entry{{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)}}
	b.FileCatalog = append(b.FileCatalog, manifest.FileEntry{
		Path: "good.txt", Size: int64(len(chunkData)), Mode: 0644,
		VolumeExtents: []manifest.VolumeExtent{{FileOffset: 0, VolumeOffset: 0, Length: int64(len(chunkData))}},
	})

	target := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)
	restorer.IgnoreErrors = true

	res, err := restorer.RestoreFiles(context.Background(), b, target, nil)
	if err != nil {
		t.Fatalf("IgnoreErrors restore failed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(target, "pagefile.sys")); statErr == nil {
		t.Error("excluded file was written")
	}
	if _, statErr := os.Stat(filepath.Join(target, "good.txt")); statErr != nil {
		t.Errorf("healthy neighbor not restored: %v", statErr)
	}
	if res.RestoredFiles == 0 {
		t.Error("no files counted as restored")
	}

	// Without IgnoreErrors the same restore must fail loudly.
	strict := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)
	if _, err := strict.RestoreFiles(context.Background(), b, t.TempDir(), nil); err == nil {
		t.Fatal("excluded entry did not fail a strict RestoreFiles")
	}
}
