// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Migrate converts legacy JSON manifests to DNM in place: a converted
// backup loads identically afterwards, a DNM-only backup is left alone, a
// second run changes nothing, and a dry run writes nothing at all. A repo
// whose migration silently dropped a backup's entries is a repo whose
// restore of that backup writes zeros.
func TestMigrateConvertsLegacyManifestsIdempotently(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := &Backup{BackupID: "legacy-0000-0000-0000-000000000001", Timestamp: time.Unix(1700000000, 0).UTC(),
		SourceVolume: "C:", TotalBytes: 3 * 1024, TotalChunks: 3, ContentDigest: "abc123",
		Entries:     []Entry{{VolumeOffset: 0, ChunkLength: 1024}, {VolumeOffset: 1024, ChunkLength: 1024}, {VolumeOffset: 2048, ChunkLength: 1024}},
		FileCatalog: []FileEntry{{Path: "a.txt", Size: 5, StreamOffset: 0, StreamLength: 5}}}
	legacy.Entries[1].ChunkHash[0] = 0x42
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "manifests", legacy.BackupID+".manifest"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// A DNM-only backup is not the migration's business at all; a backup
	// with BOTH files (an earlier partial migration) is counted as skipped.
	modern := &Backup{BackupID: "modern-0000-0000-0000-000000000002", Timestamp: time.Now(), TotalBytes: 1024,
		Entries: []Entry{{VolumeOffset: 0, ChunkLength: 1024}}}
	if err := modern.Save(repo); err != nil {
		t.Fatal(err)
	}
	both := &Backup{BackupID: "both00-0000-0000-0000-000000000003", Timestamp: time.Now(), TotalBytes: 1024,
		Entries: []Entry{{VolumeOffset: 0, ChunkLength: 1024}}}
	if err := both.Save(repo); err != nil {
		t.Fatal(err)
	}
	bothRaw, _ := json.Marshal(both)
	if err := os.WriteFile(filepath.Join(repo, "manifests", both.BackupID+".manifest"), bothRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Dry run: reports, writes nothing.
	converted, skipped, failed, err := Migrate(repo, true)
	if err != nil {
		t.Fatal(err)
	}
	if converted != 1 || skipped != 1 || failed != 0 {
		t.Fatalf("dry run: converted=%d skipped=%d failed=%d, want 1/1/0", converted, skipped, failed)
	}
	if _, err := os.Stat(DNMPath(repo, legacy.BackupID)); !os.IsNotExist(err) {
		t.Fatal("a dry run wrote a DNM")
	}

	converted, skipped, failed, err = Migrate(repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if converted != 1 || skipped != 1 || failed != 0 {
		t.Fatalf("migrate: converted=%d skipped=%d failed=%d, want 1/1/0", converted, skipped, failed)
	}
	// The legacy file is kept aside as .bak, never deleted.
	if _, err := os.Stat(filepath.Join(repo, "manifests", legacy.BackupID+".manifest.bak")); err != nil {
		t.Fatalf("the legacy manifest was not kept as .bak: %v", err)
	}
	got, err := Load(repo, legacy.BackupID)
	if err != nil {
		t.Fatalf("loading the migrated backup: %v", err)
	}
	if len(got.Entries) != 3 || got.Entries[1].ChunkHash[0] != 0x42 || got.TotalBytes != legacy.TotalBytes ||
		got.SourceVolume != "C:" || !got.Timestamp.Equal(legacy.Timestamp) || len(got.FileCatalog) != 1 || got.FileCatalog[0].Path != "a.txt" {
		t.Fatalf("migrated backup differs from the legacy one: %+v", got)
	}
	if _, err := os.Stat(DNMPath(repo, legacy.BackupID)); err != nil {
		t.Fatalf("no DNM after migration: %v", err)
	}
	// Idempotent: the converted one is .bak now, the both-files one stays skipped.
	converted, skipped, failed, err = Migrate(repo, false)
	if err != nil || converted != 0 || skipped != 1 || failed != 0 {
		t.Fatalf("second run: converted=%d skipped=%d failed=%d err=%v, want 0/1/0", converted, skipped, failed, err)
	}
	// A repo with no manifests directory is not an error.
	if _, _, _, err := Migrate(t.TempDir(), false); err != nil {
		t.Fatalf("empty repo: %v", err)
	}
}
