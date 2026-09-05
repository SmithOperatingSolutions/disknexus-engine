// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestRepairEntriesKeepsLegacySidecarWithoutDNM proves that RepairEntries
// does not delete the .entries sidecar of an unmigrated legacy backup (a
// .manifest JSON with no embedded entries and no .dnm). The sidecar is that
// backup's ONLY copy of its entries; removing it leaves the backup with zero
// entries, so restore produces nothing and a later prune deletes all of its
// chunks as orphans — unrecoverable data loss.
func TestRepairEntriesKeepsLegacySidecarWithoutDNM(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}
	const id = "legacy1"

	// Legacy format: JSON manifest with no embedded entries...
	b := Backup{BackupID: id, SourceVolume: "/dev/sda1"}
	buf, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "manifests", id+".manifest"), buf, 0644); err != nil {
		t.Fatal(err)
	}

	// ...and a sorted .entries sidecar holding the actual entries.
	entries := []Entry{
		{VolumeOffset: 0, ChunkLength: 100},
		{VolumeOffset: 100, ChunkLength: 100},
	}
	for i := range entries {
		entries[i].ChunkHash[0] = byte(i + 1)
	}
	if err := WriteEntries(repo, id, entries); err != nil {
		t.Fatalf("WriteEntries: %v", err)
	}

	// Sanity: no .dnm exists, and Load resolves entries via the sidecar.
	if _, err := os.Stat(DNMPath(repo, id)); !os.IsNotExist(err) {
		t.Fatalf("expected no .dnm file")
	}
	if loaded, err := Load(repo, id); err != nil || len(loaded.Entries) != 2 {
		t.Fatalf("precondition: Load got %d entries, err=%v; want 2", len(loaded.Entries), err)
	}

	if _, _, _, err := RepairEntries(repo, []string{id}, false); err != nil {
		t.Fatalf("RepairEntries: %v", err)
	}

	loaded, err := Load(repo, id)
	if err != nil {
		t.Fatalf("Load after repair: %v", err)
	}
	if len(loaded.Entries) != 2 {
		t.Fatalf("RepairEntries destroyed the only copy of entries: Load now returns %d, want 2", len(loaded.Entries))
	}
}
