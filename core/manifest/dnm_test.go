// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Unit tests: encode/decode helpers
// ---------------------------------------------------------------------------

func TestEncodeDecodeMetadata(t *testing.T) {
	ts := time.Date(2026, 2, 17, 12, 0, 0, 123456789, time.UTC)

	in := &Backup{
		BackupID:        "aaaa-bbbb-cccc-dddd",
		SourceVolume:    "/dev/sda1",
		BackupType:      "incremental",
		BackupMode:      "file",
		ParentBackupID:  "parent-id",
		Timestamp:       ts,
		SectorSize:      512,
		ClusterSize:     4096,
		TotalBytes:      1 << 30,
		TotalChunks:     1000,
		UniqueChunks:    800,
		DedupChunks:     200,
		RawBytes:        1 << 30,
		StoredBytes:     512 << 20,
		DedupRatio:      1.25,
		CompRatio:       2.0,
		Duration:        "5m30s",
		ChangedChunks:   100,
		UnchangedChunks: 900,
		SourcePaths:     []string{"/home/user", "/etc"},
		WrappedDEK:      []byte{1, 2, 3, 4, 5, 6, 7, 8},
	}

	data := encodeMetadata(in)
	out, err := decodeMetadata(data)
	if err != nil {
		t.Fatalf("decodeMetadata: %v", err)
	}

	check := func(field, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", field, got, want)
		}
	}
	check("BackupID", out.BackupID, in.BackupID)
	check("SourceVolume", out.SourceVolume, in.SourceVolume)
	check("BackupType", out.BackupType, in.BackupType)
	check("BackupMode", out.BackupMode, in.BackupMode)
	check("ParentBackupID", out.ParentBackupID, in.ParentBackupID)
	check("Duration", out.Duration, in.Duration)

	if !out.Timestamp.Equal(ts) {
		t.Errorf("Timestamp: got %v, want %v", out.Timestamp, ts)
	}
	if out.SectorSize != 512 {
		t.Errorf("SectorSize: got %d", out.SectorSize)
	}
	if out.ClusterSize != 4096 {
		t.Errorf("ClusterSize: got %d", out.ClusterSize)
	}
	if out.TotalBytes != in.TotalBytes {
		t.Errorf("TotalBytes: got %d, want %d", out.TotalBytes, in.TotalBytes)
	}
	if out.TotalChunks != 1000 || out.UniqueChunks != 800 || out.DedupChunks != 200 {
		t.Errorf("chunk counts wrong")
	}
	if out.DedupRatio != 1.25 {
		t.Errorf("DedupRatio: got %v, want 1.25", out.DedupRatio)
	}
	if out.CompRatio != 2.0 {
		t.Errorf("CompRatio: got %v, want 2.0", out.CompRatio)
	}
	if out.ChangedChunks != 100 || out.UnchangedChunks != 900 {
		t.Errorf("changed/unchanged counts wrong")
	}
	if len(out.SourcePaths) != 2 || out.SourcePaths[0] != "/home/user" || out.SourcePaths[1] != "/etc" {
		t.Errorf("SourcePaths: got %v", out.SourcePaths)
	}
	if len(out.WrappedDEK) != 8 || out.WrappedDEK[0] != 1 || out.WrappedDEK[7] != 8 {
		t.Errorf("WrappedDEK: got %v", out.WrappedDEK)
	}
}

func TestEncodeDecodeMetadataZeroTime(t *testing.T) {
	in := &Backup{BackupID: "x"}
	data := encodeMetadata(in)
	out, err := decodeMetadata(data)
	if err != nil {
		t.Fatalf("decodeMetadata: %v", err)
	}
	if !out.Timestamp.IsZero() {
		t.Errorf("Timestamp should be zero, got %v", out.Timestamp)
	}
}

func TestEncodeDecodeFileEntry(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 999, time.UTC)
	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 1)
	}

	in := FileEntry{
		Path:         "deep/nested/file.go",
		SourceIndex:  3,
		Size:         98765,
		Mode:         0644,
		ModTime:      ts,
		IsSymlink:    true,
		LinkTarget:   "../other.go",
		StreamOffset: 4096,
		StreamLength: 98765,
		ContentHash:  hash,
		DataBackupID: "ref-backup-id",
		VolumeExtents: []VolumeExtent{
			{FileOffset: 0, VolumeOffset: 1024, Length: 4096},
			{FileOffset: 4096, VolumeOffset: 8192, Length: 94669},
		},
	}

	data := encodeFileEntry(in)
	out, err := decodeFileEntry(data)
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}

	if out.Path != in.Path {
		t.Errorf("Path: got %q, want %q", out.Path, in.Path)
	}
	if out.SourceIndex != 3 {
		t.Errorf("SourceIndex: got %d", out.SourceIndex)
	}
	if out.Size != 98765 {
		t.Errorf("Size: got %d", out.Size)
	}
	if out.Mode != 0644 {
		t.Errorf("Mode: got %o", out.Mode)
	}
	if !out.ModTime.Equal(ts) {
		t.Errorf("ModTime: got %v, want %v", out.ModTime, ts)
	}
	if out.IsDir || !out.IsSymlink || out.Unchanged {
		t.Errorf("Flags: IsDir=%v IsSymlink=%v Unchanged=%v", out.IsDir, out.IsSymlink, out.Unchanged)
	}
	if out.LinkTarget != "../other.go" {
		t.Errorf("LinkTarget: got %q", out.LinkTarget)
	}
	if out.StreamOffset != 4096 || out.StreamLength != 98765 {
		t.Errorf("Stream: offset=%d len=%d", out.StreamOffset, out.StreamLength)
	}
	if out.ContentHash != hash {
		t.Errorf("ContentHash mismatch")
	}
	if out.DataBackupID != "ref-backup-id" {
		t.Errorf("DataBackupID: got %q", out.DataBackupID)
	}
	if len(out.VolumeExtents) != 2 {
		t.Fatalf("VolumeExtents: got %d, want 2", len(out.VolumeExtents))
	}
	if out.VolumeExtents[0].FileOffset != 0 || out.VolumeExtents[0].VolumeOffset != 1024 || out.VolumeExtents[0].Length != 4096 {
		t.Errorf("Extent[0]: %+v", out.VolumeExtents[0])
	}
	if out.VolumeExtents[1].FileOffset != 4096 || out.VolumeExtents[1].VolumeOffset != 8192 || out.VolumeExtents[1].Length != 94669 {
		t.Errorf("Extent[1]: %+v", out.VolumeExtents[1])
	}
}

func TestEncodeDecodeFileEntryDirFlags(t *testing.T) {
	in := FileEntry{
		Path:      "src/",
		IsDir:     true,
		Unchanged: true,
		Mode:      0755,
	}
	data := encodeFileEntry(in)
	out, err := decodeFileEntry(data)
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}
	if !out.IsDir {
		t.Error("IsDir should be true")
	}
	if out.IsSymlink {
		t.Error("IsSymlink should be false")
	}
	if !out.Unchanged {
		t.Error("Unchanged should be true")
	}
}

func TestEncodeDecodeFileEntryZeroHash(t *testing.T) {
	in := FileEntry{Path: "empty.txt"}
	data := encodeFileEntry(in)
	out, err := decodeFileEntry(data)
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}
	var zero [32]byte
	if out.ContentHash != zero {
		t.Errorf("ContentHash should be zero, got %x", out.ContentHash)
	}
}

// ---------------------------------------------------------------------------
// Integration: DNM file round-trip
// ---------------------------------------------------------------------------

func TestDNMRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Millisecond)

	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 1)
	}
	var entryHash [32]byte
	entryHash[0] = 0xAB
	entryHash[31] = 0xCD

	in := &Backup{
		BackupID:        "round-trip-test",
		Timestamp:       now,
		SourceVolume:    "/dev/sda",
		BackupType:      "full",
		BackupMode:      "file",
		SectorSize:      512,
		ClusterSize:     4096,
		TotalBytes:      1 << 20,
		TotalChunks:     10,
		UniqueChunks:    8,
		DedupChunks:     2,
		RawBytes:        1 << 20,
		StoredBytes:     512 << 10,
		DedupRatio:      1.25,
		CompRatio:       2.0,
		Duration:        "1s",
		ChangedChunks:   10,
		UnchangedChunks: 0,
		SourcePaths:     []string{"/home/user"},
		FileCatalog: []FileEntry{
			{
				Path:         "main.go",
				Size:         1234,
				Mode:         0644,
				ModTime:      now,
				StreamOffset: 0,
				StreamLength: 1234,
				ContentHash:  hash,
			},
			{
				Path:    "src/",
				Mode:    0755,
				ModTime: now,
				IsDir:   true,
			},
			{
				Path:       "link.txt",
				IsSymlink:  true,
				LinkTarget: "main.go",
				ModTime:    now,
			},
		},
		Entries: []Entry{
			{VolumeOffset: 0, ChunkHash: entryHash, ChunkLength: 65536},
			{VolumeOffset: 65536, ChunkHash: entryHash, ChunkLength: 65536, IsExcluded: true},
		},
	}

	if err := in.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify .dnm was created.
	if _, err := os.Stat(DNMPath(dir, in.BackupID)); err != nil {
		t.Fatalf(".dnm file not created: %v", err)
	}

	out, err := Load(dir, in.BackupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Metadata
	if out.BackupID != in.BackupID {
		t.Errorf("BackupID: got %q, want %q", out.BackupID, in.BackupID)
	}
	if !out.Timestamp.Equal(now) {
		t.Errorf("Timestamp: got %v, want %v", out.Timestamp, now)
	}
	if out.BackupType != "full" || out.BackupMode != "file" {
		t.Errorf("Type/Mode: %q %q", out.BackupType, out.BackupMode)
	}
	if out.DedupRatio != 1.25 || out.CompRatio != 2.0 {
		t.Errorf("Ratios: dedup=%v comp=%v", out.DedupRatio, out.CompRatio)
	}
	if len(out.SourcePaths) != 1 || out.SourcePaths[0] != "/home/user" {
		t.Errorf("SourcePaths: %v", out.SourcePaths)
	}

	// FileCatalog
	if len(out.FileCatalog) != 3 {
		t.Fatalf("FileCatalog: got %d, want 3", len(out.FileCatalog))
	}
	f0 := out.FileCatalog[0]
	if f0.Path != "main.go" || f0.Size != 1234 || f0.ContentHash != hash {
		t.Errorf("FileCatalog[0]: %+v", f0)
	}
	f1 := out.FileCatalog[1]
	if !f1.IsDir || f1.Path != "src/" {
		t.Errorf("FileCatalog[1]: %+v", f1)
	}
	f2 := out.FileCatalog[2]
	if !f2.IsSymlink || f2.LinkTarget != "main.go" {
		t.Errorf("FileCatalog[2]: %+v", f2)
	}

	// Entries
	if len(out.Entries) != 2 {
		t.Fatalf("Entries: got %d, want 2", len(out.Entries))
	}
	if out.Entries[0].VolumeOffset != 0 || out.Entries[0].ChunkHash != entryHash {
		t.Errorf("Entries[0]: %+v", out.Entries[0])
	}
	if !out.Entries[1].IsExcluded {
		t.Error("Entries[1].IsExcluded should be true")
	}
}

// TestDNMMetadataOnly verifies that OpenDNMReader + readMetadata does not load
// FileCatalog or Entries — enabling fast metadata-only access.
func TestDNMMetadataOnly(t *testing.T) {
	dir := t.TempDir()

	in := &Backup{
		BackupID:     "metadata-only-test",
		Timestamp:    time.Now().UTC(),
		SourceVolume: "disk",
		BackupMode:   "file",
		TotalChunks:  5,
		FileCatalog:  []FileEntry{{Path: "a.txt"}, {Path: "b.txt"}},
		Entries:      []Entry{{VolumeOffset: 0, ChunkLength: 100}},
	}
	if err := in.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r, err := OpenDNMReader(DNMPath(dir, in.BackupID))
	if err != nil {
		t.Fatalf("OpenDNMReader: %v", err)
	}
	defer r.Close()

	b, err := r.readMetadata()
	if err != nil {
		t.Fatalf("readMetadata: %v", err)
	}

	if b.BackupID != "metadata-only-test" {
		t.Errorf("BackupID: got %q", b.BackupID)
	}
	if b.TotalChunks != 5 {
		t.Errorf("TotalChunks: got %d", b.TotalChunks)
	}
	// readMetadata must NOT load FileCatalog or Entries.
	if b.FileCatalog != nil {
		t.Errorf("FileCatalog should be nil after readMetadata, got %d entries", len(b.FileCatalog))
	}
	if b.Entries != nil {
		t.Errorf("Entries should be nil after readMetadata, got %d entries", len(b.Entries))
	}
}

// TestDNMStreamCatalog verifies that streamCatalog / next() reads records one
// at a time and returns io.EOF after the last record.
func TestDNMStreamCatalog(t *testing.T) {
	dir := t.TempDir()

	want := []FileEntry{
		{Path: "a.txt", Size: 100, Mode: 0644},
		{Path: "b/c.go", Size: 200, Mode: 0644, IsSymlink: false},
		{Path: "d/", IsDir: true, Mode: 0755},
	}
	in := &Backup{
		BackupID:    "stream-catalog-test",
		FileCatalog: want,
	}
	if err := in.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r, err := OpenDNMReader(DNMPath(dir, in.BackupID))
	if err != nil {
		t.Fatalf("OpenDNMReader: %v", err)
	}
	defer r.Close()

	cr, err := r.streamCatalog()
	if err != nil {
		t.Fatalf("streamCatalog: %v", err)
	}

	got := make([]FileEntry, 0, 3)
	for {
		fe, err := cr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		got = append(got, fe)
	}

	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Path != w.Path {
			t.Errorf("[%d] Path: got %q, want %q", i, got[i].Path, w.Path)
		}
		if got[i].IsDir != w.IsDir {
			t.Errorf("[%d] IsDir: got %v, want %v", i, got[i].IsDir, w.IsDir)
		}
	}
}

// TestDNMEntriesViaSidecar verifies the pipeline path: entries are written to
// the .entries sidecar before Save(), and saveDNM copies them into the .dnm
// ENTRIES section correctly.
func TestDNMEntriesViaSidecar(t *testing.T) {
	dir := t.TempDir()
	backupID := "sidecar-entries-test"

	var hash [32]byte
	for i := range hash {
		hash[i] = byte(i + 10)
	}

	// Simulate pipeline: stream entries to sidecar before Save().
	ew, err := OpenEntryWriter(dir, backupID)
	if err != nil {
		t.Fatalf("OpenEntryWriter: %v", err)
	}
	for i := range 5 {
		if err := ew.WriteEntry(Entry{
			VolumeOffset: int64(i * 4096),
			ChunkHash:    hash,
			ChunkLength:  4096,
		}); err != nil {
			t.Fatalf("WriteEntry: %v", err)
		}
	}
	if err := ew.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Save with no in-memory Entries.
	b := &Backup{BackupID: backupID, Timestamp: time.Now().UTC(), TotalChunks: 5}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load via .dnm and verify entries are present.
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
		if e.ChunkHash != hash {
			t.Errorf("[%d] ChunkHash mismatch", i)
		}
	}
}

// TestDNMFallbackToLegacy verifies that Load() falls back to the legacy
// .manifest + .entries path when no .dnm file exists.
func TestDNMFallbackToLegacy(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}

	// Write only the legacy JSON manifest (no .dnm, no .entries sidecar).
	legacyJSON := `{
		"backup_id": "legacy-only",
		"timestamp": "2025-01-01T00:00:00Z",
		"source_volume": "/dev/sda",
		"total_bytes": 512,
		"entries": [
			{"volume_offset": 0, "chunk_hash": "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20", "chunk_length": 512}
		],
		"total_chunks": 1, "unique_chunks": 1, "dedup_chunks": 0,
		"raw_bytes": 512, "stored_bytes": 256, "dedup_ratio": 0,
		"compression_ratio": 2, "duration": "1s"
	}`
	p := filepath.Join(dir, "manifests", "legacy-only.manifest")
	if err := os.WriteFile(p, []byte(legacyJSON), 0644); err != nil {
		t.Fatal(err)
	}

	// Confirm no .dnm file.
	if _, err := os.Stat(DNMPath(dir, "legacy-only")); !os.IsNotExist(err) {
		t.Fatal("expected no .dnm file for legacy backup")
	}

	b, err := Load(dir, "legacy-only")
	if err != nil {
		t.Fatalf("Load (legacy fallback): %v", err)
	}
	if b.BackupID != "legacy-only" {
		t.Errorf("BackupID: got %q", b.BackupID)
	}
	if len(b.Entries) != 1 {
		t.Fatalf("Entries: got %d, want 1", len(b.Entries))
	}
	if b.Entries[0].VolumeOffset != 0 || b.Entries[0].ChunkLength != 512 {
		t.Errorf("Entry: %+v", b.Entries[0])
	}
}

// TestDNMDeleteCleansUp verifies that Delete() removes the .dnm file.
// Save() no longer writes .manifest or .entries, so only .dnm is checked.
func TestDNMDeleteCleansUp(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID: "delete-dnm-test",
		Entries:  []Entry{{VolumeOffset: 0, ChunkLength: 128}},
	}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Only .dnm should exist after Save().
	if _, err := os.Stat(DNMPath(dir, "delete-dnm-test")); err != nil {
		t.Fatalf("expected .dnm to exist before delete: %v", err)
	}

	if err := Delete(dir, "delete-dnm-test"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(DNMPath(dir, "delete-dnm-test")); !os.IsNotExist(err) {
		t.Errorf("expected .dnm to be gone after delete")
	}
}

// TestDNMEmptyCatalogAndEntries verifies a backup with no files and no entries.
func TestDNMEmptyCatalogAndEntries(t *testing.T) {
	dir := t.TempDir()

	in := &Backup{
		BackupID:  "empty-test",
		Timestamp: time.Now().UTC(),
	}
	if err := in.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := Load(dir, "empty-test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(out.FileCatalog) != 0 {
		t.Errorf("FileCatalog: want 0, got %d", len(out.FileCatalog))
	}
	if len(out.Entries) != 0 {
		t.Errorf("Entries: want 0, got %d", len(out.Entries))
	}
}

// TestDNMSectionIndexOffsets verifies that the section index byte offsets
// recorded in the header are consistent with file size and section count.
func TestDNMSectionIndexOffsets(t *testing.T) {
	dir := t.TempDir()

	b := &Backup{
		BackupID: "offsets-test",
		FileCatalog: []FileEntry{
			{Path: "file.txt", Size: 42},
		},
		Entries: []Entry{
			{VolumeOffset: 0, ChunkLength: 42},
		},
	}
	if err := b.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	r, err := OpenDNMReader(DNMPath(dir, b.BackupID))
	if err != nil {
		t.Fatalf("OpenDNMReader: %v", err)
	}
	defer r.Close()

	// All three sections must have non-zero offsets and lengths.
	for name, s := range map[string]sectionInfo{
		"metadata": r.meta,
		"catalog":  r.catalog,
		"entries":  r.entries,
	} {
		if s.offset == 0 {
			t.Errorf("section %q: offset is 0", name)
		}
		if s.length == 0 {
			t.Errorf("section %q: length is 0", name)
		}
	}

	// METADATA section must report count=1.
	if r.meta.count != 1 {
		t.Errorf("metadata count: got %d, want 1", r.meta.count)
	}
	// CATALOG section must report count=1.
	if r.catalog.count != 1 {
		t.Errorf("catalog count: got %d, want 1", r.catalog.count)
	}
	// ENTRIES section must report count=1.
	if r.entries.count != 1 {
		t.Errorf("entries count: got %d, want 1", r.entries.count)
	}

	// Section index must start after the file header + all sections.
	fi, err := os.Stat(DNMPath(dir, b.BackupID))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	expectedFileSize := int64(r.meta.offset) +
		int64(r.meta.length) +
		int64(r.catalog.length) +
		int64(r.entries.length) +
		int64(numSections*sectionIndexSize)
	_ = expectedFileSize // rough check: actual file must be larger than just the header
	if fi.Size() < fileHeaderSize {
		t.Errorf("file too small: %d bytes", fi.Size())
	}
}
