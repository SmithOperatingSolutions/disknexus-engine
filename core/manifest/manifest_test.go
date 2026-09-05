// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestManifestRoundTrip: the wire JSON decodes into the struct, and the
// struct encodes back to a document that decodes IDENTICALLY. The previous
// spelling unmarshalled and t.Logf'd three fields — it could not detect a
// renamed JSON tag or a hex decoder returning zeros (#402), and a manifest
// whose tags drift is a backup that decodes empty on the next reader.
func TestManifestRoundTrip(t *testing.T) {
	data := `{"backup_id":"test","timestamp":"2026-02-15T12:55:00Z","source_volume":"test","sector_size":0,"cluster_size":0,"total_bytes":100,"entries":[{"volume_offset":0,"chunk_hash":"2f31d065f61266a62d161821939f5b5a0358c5a3af2b684fc3f9ea40368b9860","chunk_length":100}],"total_chunks":1,"unique_chunks":1,"dedup_chunks":0,"raw_bytes":100,"stored_bytes":50,"dedup_ratio":0,"compression_ratio":2,"duration":"1s"}`

	var b Backup
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The fields the wire named must have LANDED — an unmarshal into a
	// struct whose tags drifted "succeeds" with zero values, and a Logf
	// cannot object.
	if b.BackupID != "test" {
		t.Fatalf("backup_id = %q, want %q — the wire's field did not land; a tag rename ships manifests every existing reader decodes as empty", b.BackupID, "test")
	}
	if b.TotalBytes != 100 || len(b.Entries) != 1 || b.Entries[0].ChunkLength != 100 {
		t.Fatalf("decoded manifest lost its numbers: total_bytes=%d entries=%d", b.TotalBytes, len(b.Entries))
	}
	wantHash := "2f31d065f61266a62d161821939f5b5a0358c5a3af2b684fc3f9ea40368b9860"
	if got := fmt.Sprintf("%x", b.Entries[0].ChunkHash); got != wantHash {
		t.Fatalf("chunk hash decoded to %s, want %s — a hex decoder returning zeros makes every chunk resolve to the zero hash", got, wantHash)
	}
	// And back: encode, decode, compare against the FIRST decode — a field
	// that marshals under a different name than it unmarshals is a manifest
	// this writer produces and no reader (this one included) can read.
	out, err := json.Marshal(&b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var b2 Backup
	if err := json.Unmarshal(out, &b2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !reflect.DeepEqual(b, b2) {
		t.Fatalf("round trip changed the manifest:\n first: %+v\nsecond: %+v", b, b2)
	}
}

func TestManifestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID:     "aaaa-bbbb-cccc",
		Timestamp:    time.Now(),
		SourceVolume: "/dev/sda",
		TotalBytes:   1024,
		Entries: []Entry{
			{VolumeOffset: 0, ChunkLength: 512},
			{VolumeOffset: 512, ChunkLength: 512},
		},
		TotalChunks:  2,
		UniqueChunks: 2,
		RawBytes:     1024,
		StoredBytes:  800,
		Duration:     "100ms",
	}

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, "aaaa-bbbb-cccc")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.BackupID != b.BackupID {
		t.Errorf("BackupID: got %q, want %q", loaded.BackupID, b.BackupID)
	}
	if len(loaded.Entries) != 2 {
		t.Errorf("Entries: got %d, want 2", len(loaded.Entries))
	}
}

func TestResolveID(t *testing.T) {
	dir := t.TempDir()

	// Create two backups with distinct prefixes
	b1 := &Backup{BackupID: "aaaa-1111", Timestamp: time.Now()}
	b2 := &Backup{BackupID: "bbbb-2222", Timestamp: time.Now()}
	b1.Save(dir)
	b2.Save(dir)

	// Resolve by prefix
	id, err := ResolveID(dir, "aaaa")
	if err != nil {
		t.Fatalf("ResolveID: %v", err)
	}
	if id != "aaaa-1111" {
		t.Errorf("got %q, want %q", id, "aaaa-1111")
	}

	id, err = ResolveID(dir, "bbbb")
	if err != nil {
		t.Fatalf("ResolveID: %v", err)
	}
	if id != "bbbb-2222" {
		t.Errorf("got %q, want %q", id, "bbbb-2222")
	}

	// Full ID works too
	id, err = ResolveID(dir, "aaaa-1111")
	if err != nil {
		t.Fatalf("ResolveID full: %v", err)
	}
	if id != "aaaa-1111" {
		t.Errorf("got %q, want %q", id, "aaaa-1111")
	}
}

func TestResolveIDAmbiguous(t *testing.T) {
	dir := t.TempDir()

	b1 := &Backup{BackupID: "aaaa-1111", Timestamp: time.Now()}
	b2 := &Backup{BackupID: "aaaa-2222", Timestamp: time.Now()}
	b1.Save(dir)
	b2.Save(dir)

	_, err := ResolveID(dir, "aaaa")
	if err == nil {
		t.Fatal("expected error for ambiguous prefix")
	}
}

func TestResolveIDNotFound(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{BackupID: "aaaa-1111", Timestamp: time.Now()}
	b.Save(dir)

	_, err := ResolveID(dir, "zzzz")
	if err == nil {
		t.Fatal("expected error for missing prefix")
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{BackupID: "aaaa-1111", Timestamp: time.Now()}
	b.Save(dir)

	if err := Delete(dir, "aaaa-1111"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := Load(dir, "aaaa-1111")
	if err == nil {
		t.Fatal("expected error loading deleted manifest")
	}
}

func TestGet(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID:     "aaaa-bbbb-cccc-dddd",
		Timestamp:    time.Now(),
		SourceVolume: "test",
		TotalBytes:   100,
	}
	b.Save(dir)

	loaded, err := Get(dir, "aaaa")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.BackupID != "aaaa-bbbb-cccc-dddd" {
		t.Errorf("got %q, want %q", loaded.BackupID, "aaaa-bbbb-cccc-dddd")
	}
}

func TestListSortedByTimestamp(t *testing.T) {
	dir := t.TempDir()

	now := time.Now()
	b1 := &Backup{BackupID: "cccc", Timestamp: now.Add(2 * time.Hour)}
	b2 := &Backup{BackupID: "aaaa", Timestamp: now}
	b3 := &Backup{BackupID: "bbbb", Timestamp: now.Add(1 * time.Hour)}

	b1.Save(dir)
	b2.Save(dir)
	b3.Save(dir)

	list, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(list) != 3 {
		t.Fatalf("got %d, want 3", len(list))
	}

	// Should be sorted by timestamp: aaaa, bbbb, cccc
	if list[0].BackupID != "aaaa" {
		t.Errorf("list[0]: got %q, want %q", list[0].BackupID, "aaaa")
	}
	if list[1].BackupID != "bbbb" {
		t.Errorf("list[1]: got %q, want %q", list[1].BackupID, "bbbb")
	}
	if list[2].BackupID != "cccc" {
		t.Errorf("list[2]: got %q, want %q", list[2].BackupID, "cccc")
	}
}

func TestIncrementalFields(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID:        "incr-test",
		Timestamp:       time.Now(),
		ParentBackupID:  "parent-id",
		BackupType:      "incremental",
		ChangedChunks:   10,
		UnchangedChunks: 90,
		TotalChunks:     100,
	}
	b.Save(dir)

	loaded, err := Load(dir, "incr-test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.ParentBackupID != "parent-id" {
		t.Errorf("ParentBackupID: got %q, want %q", loaded.ParentBackupID, "parent-id")
	}
	if loaded.BackupType != "incremental" {
		t.Errorf("BackupType: got %q, want %q", loaded.BackupType, "incremental")
	}
	if loaded.ChangedChunks != 10 {
		t.Errorf("ChangedChunks: got %d, want 10", loaded.ChangedChunks)
	}
	if loaded.UnchangedChunks != 90 {
		t.Errorf("UnchangedChunks: got %d, want 90", loaded.UnchangedChunks)
	}
}

func TestFileEntryRoundTrip(t *testing.T) {
	dir := t.TempDir()

	now := time.Now().Truncate(time.Second) // JSON loses sub-second on some platforms

	b := &Backup{
		BackupID:     "file-test-1234",
		Timestamp:    now,
		SourceVolume: "file-backup",
		TotalBytes:   5000,
		BackupMode:   "file",
		SourcePaths:  []string{"/home/user/project"},
		FileCatalog: []FileEntry{
			{
				Path:         "src/main.go",
				SourceIndex:  0,
				Size:         1234,
				Mode:         0644,
				ModTime:      now,
				StreamOffset: 0,
				StreamLength: 1234,
				ContentHash:  [32]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32},
			},
			{
				Path:        "src",
				SourceIndex: 0,
				Mode:        0755,
				ModTime:     now,
				IsDir:       true,
			},
			{
				Path:       "link.txt",
				IsSymlink:  true,
				LinkTarget: "src/main.go",
				ModTime:    now,
			},
		},
	}

	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, "file-test-1234")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.BackupMode != "file" {
		t.Errorf("BackupMode: got %q, want %q", loaded.BackupMode, "file")
	}
	if len(loaded.SourcePaths) != 1 || loaded.SourcePaths[0] != "/home/user/project" {
		t.Errorf("SourcePaths: got %v", loaded.SourcePaths)
	}
	if len(loaded.FileCatalog) != 3 {
		t.Fatalf("FileCatalog: got %d entries, want 3", len(loaded.FileCatalog))
	}

	f0 := loaded.FileCatalog[0]
	if f0.Path != "src/main.go" {
		t.Errorf("Path: got %q", f0.Path)
	}
	if f0.Size != 1234 {
		t.Errorf("Size: got %d", f0.Size)
	}
	if f0.StreamOffset != 0 || f0.StreamLength != 1234 {
		t.Errorf("Stream: offset=%d len=%d", f0.StreamOffset, f0.StreamLength)
	}
	if f0.ContentHash[0] != 1 || f0.ContentHash[31] != 32 {
		t.Errorf("ContentHash not preserved: %x", f0.ContentHash)
	}

	f1 := loaded.FileCatalog[1]
	if !f1.IsDir {
		t.Error("expected src to be directory")
	}

	f2 := loaded.FileCatalog[2]
	if !f2.IsSymlink || f2.LinkTarget != "src/main.go" {
		t.Errorf("symlink: isSymlink=%v target=%q", f2.IsSymlink, f2.LinkTarget)
	}
}

// TestEntrySidecarRoundTrip verifies the binary encode/decode path for all
// Entry fields, including non-zero hashes, IsExcluded, and boundary values.
func TestEntrySidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()

	var hash1, hash2 [32]byte
	for i := range hash1 {
		hash1[i] = byte(i + 1)
	}
	hash2[0] = 0xff
	hash2[31] = 0x01

	want := []Entry{
		{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 512, IsExcluded: false},
		{VolumeOffset: 512, ChunkHash: hash2, ChunkLength: 1024, IsExcluded: true},
		{VolumeOffset: 1<<40 - 1, ChunkHash: [32]byte{}, ChunkLength: 65536, IsExcluded: false},
	}

	if err := WriteEntries(dir, "test-id", want); err != nil {
		t.Fatalf("WriteEntries: %v", err)
	}

	got, err := ReadEntries(dir, "test-id")
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.VolumeOffset != w.VolumeOffset {
			t.Errorf("[%d] VolumeOffset: got %d, want %d", i, g.VolumeOffset, w.VolumeOffset)
		}
		if g.ChunkHash != w.ChunkHash {
			t.Errorf("[%d] ChunkHash: got %x, want %x", i, g.ChunkHash, w.ChunkHash)
		}
		if g.ChunkLength != w.ChunkLength {
			t.Errorf("[%d] ChunkLength: got %d, want %d", i, g.ChunkLength, w.ChunkLength)
		}
		if g.IsExcluded != w.IsExcluded {
			t.Errorf("[%d] IsExcluded: got %v, want %v", i, g.IsExcluded, w.IsExcluded)
		}
	}
}

// TestLoadOldFormatWithJSONEntries verifies that Load() correctly reads entries
// from the JSON manifest when no binary sidecar exists (old format).
func TestLoadOldFormatWithJSONEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}

	// Write a manifest in the old format (entries embedded in JSON, no sidecar).
	oldJSON := `{
		"backup_id": "old-format-1",
		"timestamp": "2025-01-01T00:00:00Z",
		"source_volume": "/dev/sda",
		"total_bytes": 1024,
		"entries": [
			{"volume_offset": 0,   "chunk_hash": "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", "chunk_length": 512},
			{"volume_offset": 512, "chunk_hash": "2122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f40", "chunk_length": 512, "is_excluded": true}
		],
		"total_chunks": 2,
		"unique_chunks": 2,
		"dedup_chunks": 0,
		"raw_bytes": 1024,
		"stored_bytes": 800,
		"dedup_ratio": 0,
		"compression_ratio": 0,
		"duration": "1s"
	}`
	path := filepath.Join(dir, "manifests", "old-format-1.manifest")
	if err := os.WriteFile(path, []byte(oldJSON), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := Load(dir, "old-format-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.Entries) != 2 {
		t.Fatalf("Entries: got %d, want 2", len(b.Entries))
	}
	if b.Entries[0].VolumeOffset != 0 || b.Entries[0].ChunkLength != 512 {
		t.Errorf("entry[0]: %+v", b.Entries[0])
	}
	if !b.Entries[1].IsExcluded {
		t.Errorf("entry[1]: IsExcluded should be true")
	}
}

// TestSaveDNMOnlyNoLegacyFiles verifies that Save() writes only the .dnm file
// and leaves no .manifest or .entries files behind.
func TestSaveDNMOnlyNoLegacyFiles(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID:  "dnm-only",
		Timestamp: time.Now(),
		Entries:   []Entry{{VolumeOffset: 0, ChunkLength: 100}},
	}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// .dnm must exist.
	if _, err := os.Stat(DNMPath(dir, "dnm-only")); err != nil {
		t.Fatalf("expected .dnm to exist: %v", err)
	}
	// .manifest must NOT exist.
	if _, err := os.Stat(filepath.Join(dir, "manifests", "dnm-only.manifest")); !os.IsNotExist(err) {
		t.Error("Save() must not write a legacy .manifest file")
	}
	// .entries must NOT exist (saveDNM embeds them and Save() removes the sidecar).
	if _, err := os.Stat(EntriesPath(dir, "dnm-only")); !os.IsNotExist(err) {
		t.Error("Save() must not leave behind a .entries sidecar")
	}
}

// TestDeleteCleansDNM verifies that Delete() removes the .dnm file.
func TestDeleteCleansDNM(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID: "delete-me",
		Entries:  []Entry{{VolumeOffset: 0, ChunkLength: 128}},
	}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// .dnm should exist before deletion.
	if _, err := os.Stat(DNMPath(dir, "delete-me")); err != nil {
		t.Fatalf("dnm missing before delete: %v", err)
	}

	if err := Delete(dir, "delete-me"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(DNMPath(dir, "delete-me")); !os.IsNotExist(err) {
		t.Error(".dnm should be gone after Delete()")
	}
}

// TestStreamingSidecarWriterPath exercises the pipeline code path: entries are
// written to the sidecar via OpenEntryWriter before Save() is called, and Save()
// is called with an empty Backup.Entries. Load() must return the streamed entries.
func TestStreamingSidecarWriterPath(t *testing.T) {
	dir := t.TempDir()
	backupID := "streaming-test"

	var wantHash [32]byte
	for i := range wantHash {
		wantHash[i] = byte(i)
	}

	// Simulate the pipeline: stream entries to sidecar.
	ew, err := OpenEntryWriter(dir, backupID)
	if err != nil {
		t.Fatalf("OpenEntryWriter: %v", err)
	}
	for i := range 5 {
		if err := ew.WriteEntry(Entry{
			VolumeOffset: int64(i * 4096),
			ChunkHash:    wantHash,
			ChunkLength:  4096,
		}); err != nil {
			t.Fatalf("WriteEntry: %v", err)
		}
	}
	if err := ew.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Save manifest with no in-memory entries (as the pipeline does).
	b := &Backup{BackupID: backupID, Timestamp: time.Now(), TotalChunks: 5}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(dir, backupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Entries) != 5 {
		t.Fatalf("Entries: got %d, want 5", len(loaded.Entries))
	}
	for i, e := range loaded.Entries {
		if e.VolumeOffset != int64(i*4096) {
			t.Errorf("[%d] VolumeOffset: got %d, want %d", i, e.VolumeOffset, i*4096)
		}
		if e.ChunkHash != wantHash {
			t.Errorf("[%d] ChunkHash mismatch", i)
		}
	}
}

func TestFileEntryBackwardCompatibility(t *testing.T) {
	// Old manifest JSON without file-mode fields should unmarshal fine
	data := `{"backup_id":"old-backup","timestamp":"2026-01-01T00:00:00Z","source_volume":"disk","total_bytes":100,"entries":[],"total_chunks":0,"unique_chunks":0,"dedup_chunks":0,"raw_bytes":0,"stored_bytes":0,"dedup_ratio":0,"compression_ratio":0,"duration":"1s"}`

	var b Backup
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if b.BackupMode != "" {
		t.Errorf("BackupMode should be empty, got %q", b.BackupMode)
	}
	if len(b.SourcePaths) != 0 {
		t.Errorf("SourcePaths should be empty, got %v", b.SourcePaths)
	}
	if len(b.FileCatalog) != 0 {
		t.Errorf("FileCatalog should be empty, got %d entries", len(b.FileCatalog))
	}
}
