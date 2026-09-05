// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/exportimport"
)

// TestExportIncludesLegacyManifestAncestor guards the round-3 finding: the
// ancestor traversal staged manifests by copying DNMPath verbatim, so an
// ancestor that exists only in legacy .manifest form (pre-.dnm history — still
// fully supported by Load/List/prune) aborted the whole export with ENOENT.
// Legacy ancestors must be re-serialized to .dnm instead.
func TestExportIncludesLegacyManifestAncestor(t *testing.T) {
	ctx := context.Background()
	repoPath, cfg := setupRepo(t)
	_ = cfg

	// Backup A: a real backup, then DEMOTED to legacy form — its .dnm is
	// rewritten as a .manifest JSON with embedded entries and the .dnm removed.
	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	aID := doBackup(t, repoPath, sourceData, cfg)

	aFull, err := manifest.Load(repoPath, aID)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	legacyJSON, err := json.Marshal(aFull)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "manifests", aID+".manifest"), legacyJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest.DNMPath(repoPath, aID)); err != nil {
		t.Fatal(err)
	}

	// Backup B: a modern .dnm incremental whose unchanged file references A.
	bID := "00000000-0000-0000-0000-0000000000c3"
	b := &manifest.Backup{
		BackupID:       bID,
		Timestamp:      time.Unix(1700000000, 0),
		SourceVolume:   "files",
		BackupMode:     "file",
		BackupType:     "incremental",
		ParentBackupID: aID,
		SourcePaths:    []string{"src"},
		FileCatalog: []manifest.FileEntry{{
			Path:         "unchanged.dat",
			Size:         int64(len(sourceData)),
			StreamLength: int64(len(sourceData)),
			Unchanged:    true,
			DataBackupID: aID,
		}},
	}
	if err := b.Save(repoPath); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// Export ONLY B — pre-fix this failed with "copying manifest for <A>: no
	// such file" because A has no .dnm.
	zipPath := filepath.Join(t.TempDir(), "b.zip")
	if err := exportimport.Export(repoPath, []string{bID}, zipPath, nil); err != nil {
		t.Fatalf("Export with a legacy-format ancestor failed: %v", err)
	}

	// Import into a fresh repo: the legacy ancestor must be present (as .dnm).
	repoPath2, _ := setupRepo(t)
	if err := exportimport.Import(ctx, repoPath2, zipPath, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := manifest.Load(repoPath2, aID); err != nil {
		t.Fatalf("legacy ancestor missing after import: %v", err)
	}
}
