// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// TestIgnoreErrorsKeepsPreexistingFile guards the round-3 finding: the
// IgnoreErrors cleanup used to os.Remove the target path even when the restore
// failed BEFORE creating the output (the incomplete-catalog-entry check runs
// pre-Create). Restoring over a directory that already contained good files —
// an in-place refresh — then deleted files the restore never opened.
func TestIgnoreErrorsKeepsPreexistingFile(t *testing.T) {
	r := NewFileRestorer(nil, nil, t.TempDir(), quietLogger())
	r.IgnoreErrors = true

	// Catalog entry with Size>0 but StreamLength==0: the documented
	// broken-scanner case that errors BEFORE the output is created.
	backup := &manifest.Backup{
		BackupID: "77777777-7777-7777-7777-777777777777",
		FileCatalog: []manifest.FileEntry{{
			Path: "precious.txt",
			Size: 1000, // no StreamLength, no VolumeExtents, no InlineData
		}},
	}

	targetDir := t.TempDir()
	pre := filepath.Join(targetDir, "precious.txt")
	if err := os.WriteFile(pre, []byte("existing good copy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.RestoreFiles(context.Background(), backup, targetDir, nil); err != nil {
		t.Fatalf("RestoreFiles with IgnoreErrors: %v", err)
	}

	got, err := os.ReadFile(pre)
	if err != nil {
		t.Fatalf("pre-existing file was DELETED by a restore that never opened it: %v", err)
	}
	if string(got) != "existing good copy" {
		t.Fatalf("pre-existing file was modified: %q", got)
	}
}

// TestIgnoreErrorsStillRemovesOwnPartial confirms the ownership move kept the
// original guarantee: a file the restore DID create and then failed on is
// removed, not left short-and-silent.
func TestIgnoreErrorsStillRemovesOwnPartial(t *testing.T) {
	r := NewFileRestorer(nil, nil, t.TempDir(), quietLogger())
	r.IgnoreErrors = true

	// StreamLength claims bytes but no entries cover them: restoreFile creates
	// the output, writes 0 of 1000, and errors — its own partial must go.
	backup := &manifest.Backup{
		BackupID: "66666666-6666-6666-6666-666666666666",
		FileCatalog: []manifest.FileEntry{{
			Path:         "partial.dat",
			Size:         1000,
			StreamLength: 1000,
		}},
	}

	targetDir := t.TempDir()
	if _, err := r.RestoreFiles(context.Background(), backup, targetDir, nil); err != nil {
		t.Fatalf("RestoreFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "partial.dat")); err == nil {
		t.Fatal("restore-created partial file was left behind")
	}
}
