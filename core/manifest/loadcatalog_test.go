// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest_test

import (
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestLoadCatalogSkipsEntries guards issue #16: prune's DataBackupID walk only
// needs metadata + FileCatalog, but used manifest.Load, which also pulls the
// full entries section into RAM (GBs for large manifests). LoadCatalog must
// return the catalog and metadata WITHOUT loading entries.
func TestLoadCatalogSkipsEntries(t *testing.T) {
	repo := t.TempDir()
	if err := store.InitRepo(repo, store.RepoConfig{}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	b := &manifest.Backup{
		BackupID:   "44444444-4444-4444-4444-444444444444",
		Timestamp:  time.Unix(1700000000, 0),
		BackupMode: "file",
		TotalBytes: 30,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkLength: 10},
			{VolumeOffset: 10, ChunkLength: 20},
		},
		FileCatalog: []manifest.FileEntry{{
			Path:         "a.txt",
			Size:         30,
			StreamLength: 30,
			DataBackupID: "55555555-5555-5555-5555-555555555555",
		}},
	}
	if err := b.Save(repo); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := manifest.LoadCatalog(repo, b.BackupID)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(got.FileCatalog) != 1 || got.FileCatalog[0].DataBackupID != "55555555-5555-5555-5555-555555555555" {
		t.Fatalf("catalog not loaded: %+v", got.FileCatalog)
	}
	if got.BackupMode != "file" || got.TotalBytes != 30 {
		t.Fatalf("metadata not loaded: mode=%q total=%d", got.BackupMode, got.TotalBytes)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("LoadCatalog loaded %d entries; it must skip the entries section entirely", len(got.Entries))
	}

	// Sanity: full Load still returns the entries.
	full, err := manifest.Load(repo, b.BackupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(full.Entries) != 2 {
		t.Fatalf("Load returned %d entries, want 2", len(full.Entries))
	}
}
