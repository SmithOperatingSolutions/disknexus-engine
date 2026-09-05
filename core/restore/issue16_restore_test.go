// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestRestoreZeroEntriesFailsLoud guards issue #16: a backup claiming data
// (TotalBytes > 0) but carrying no chunk entries — e.g. a legacy manifest whose
// .entries sidecar was lost — used to "restore" successfully: Restore truncated
// the target and returned success with zero chunks, silently producing an
// all-zero volume. Prune treats this state as an error; restore must too.
func TestRestoreZeroEntriesFailsLoud(t *testing.T) {
	r := NewRestorer(nil, nil, quietLogger())
	backup := &manifest.Backup{
		BackupID:   "99999999-9999-9999-9999-999999999999",
		TotalBytes: 4096,
		// Entries deliberately empty.
	}
	if _, err := r.Restore(context.Background(), backup, nil); err == nil {
		t.Fatal("Restore of a zero-entries backup claiming 4096 bytes returned success; would produce an all-zero volume")
	}
}

// TestIgnoreErrorsRemovesPartialFile guards issue #16: with IgnoreErrors=true, a
// mid-file restore failure used to leave the truncated output file in place —
// indistinguishable from a successfully restored file. The partial file must be
// removed (absent-and-logged, not short-and-silent).
func TestIgnoreErrorsRemovesPartialFile(t *testing.T) {
	r := NewFileRestorer(nil, nil, t.TempDir(), quietLogger())
	r.IgnoreErrors = true

	// The catalog claims 1000 bytes but there are no covering entries, so
	// restoreFile creates the output, writes 0 of 1000 bytes, and errors.
	backup := &manifest.Backup{
		BackupID: "88888888-8888-8888-8888-888888888888",
		FileCatalog: []manifest.FileEntry{{
			Path:         "partial.dat",
			Size:         1000,
			StreamOffset: 0,
			StreamLength: 1000,
		}},
	}

	targetDir := t.TempDir()
	res, err := r.RestoreFiles(context.Background(), backup, targetDir, nil)
	if err != nil {
		t.Fatalf("RestoreFiles with IgnoreErrors should succeed overall: %v", err)
	}
	if res.RestoredFiles != 0 {
		t.Fatalf("RestoredFiles = %d, want 0", res.RestoredFiles)
	}

	if _, statErr := os.Stat(filepath.Join(targetDir, "partial.dat")); statErr == nil {
		t.Fatal("IgnoreErrors left a truncated partial.dat in place; a partial output must be removed, not silently kept")
	}
}
