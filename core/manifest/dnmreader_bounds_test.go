// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest_test

import (
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestDNMReaderEntryBoundsChecked guards issue #16: EntryAt/EntriesRange did no
// bounds checking, returning garbage (or an opaque read error) for out-of-range
// indices instead of a clear error, diverging from the slice accessor.
func TestDNMReaderEntryBoundsChecked(t *testing.T) {
	repo := t.TempDir()
	if err := store.InitRepo(repo, store.RepoConfig{}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	b := &manifest.Backup{
		BackupID: "33333333-3333-3333-3333-333333333333",
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkLength: 10},
			{VolumeOffset: 10, ChunkLength: 10},
		},
	}
	if err := b.Save(repo); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r, err := manifest.OpenDNMReader(filepath.Join(repo, "manifests", b.BackupID+".dnm"))
	if err != nil {
		t.Fatalf("OpenDNMReader: %v", err)
	}
	defer r.Close()

	count := uint64(r.EntriesCount())
	if count != 2 {
		t.Fatalf("EntriesCount = %d, want 2", count)
	}

	if _, err := r.EntryAt(count); err == nil {
		t.Fatal("EntryAt(count) should be out of range")
	}
	if _, err := r.EntriesRange(0, count+1); err == nil {
		t.Fatal("EntriesRange(0, count+1) should be out of range")
	}

	// In-range access must still work.
	if _, err := r.EntryAt(0); err != nil {
		t.Fatalf("EntryAt(0): %v", err)
	}
}
