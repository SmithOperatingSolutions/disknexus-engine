// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func catalogFixture(n int) []FileEntry {
	files := make([]FileEntry, n)
	var off int64
	for i := range files {
		files[i] = FileEntry{Path: fmt.Sprintf("dir%d/file%d.bin", i%3, i), Size: int64(1000 + i), Mode: 0o644,
			ModTime: time.Unix(1700000000+int64(i), 0).UTC(), StreamOffset: off, StreamLength: int64(1000 + i)}
		files[i].ContentHash[0] = byte(i)
		off += int64(1000 + i)
	}
	return files
}

// StreamCatalog is the bounded-memory way to find a handful of paths in a
// whole-volume catalog (#419): every record, in stored order, one at a
// time, and a visitor that returns false stops the walk there.
func TestStreamCatalogWalksEveryRecordInOrderAndStopsOnFalse(t *testing.T) {
	repo := t.TempDir()
	files := catalogFixture(250)
	b := &Backup{BackupID: "stream-cat", Timestamp: time.Now(), TotalBytes: 4096, FileCatalog: files,
		Entries: []Entry{{VolumeOffset: 0, ChunkLength: 4096}}}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDNMReader(DNMPath(repo, b.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.CatalogCount() != int64(len(files)) {
		t.Fatalf("CatalogCount = %d, want %d", r.CatalogCount(), len(files))
	}
	var got []FileEntry
	if err := r.StreamCatalog(func(f FileEntry) bool { got = append(got, f); return true }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("streamed %d records, want %d", len(got), len(files))
	}
	for i := range files {
		if got[i].Path != files[i].Path || got[i].Size != files[i].Size || got[i].StreamOffset != files[i].StreamOffset ||
			got[i].ContentHash != files[i].ContentHash || !got[i].ModTime.Equal(files[i].ModTime) {
			t.Fatalf("record %d = %+v, want %+v", i, got[i], files[i])
		}
	}
	// Early stop: the visitor says false at the 10th record and sees no 11th.
	seen := 0
	if err := r.StreamCatalog(func(FileEntry) bool { seen++; return seen < 10 }); err != nil {
		t.Fatal(err)
	}
	if seen != 10 {
		t.Fatalf("visitor returned false at 10 and was called %d times", seen)
	}
	// And the streamed catalog is the same one Load materializes.
	full, err := Load(repo, b.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.FileCatalog) != len(files) || full.FileCatalog[249].Path != files[249].Path {
		t.Fatalf("Load's catalog differs from the stream: %d records", len(full.FileCatalog))
	}
}

// StreamChunkHashes yields every entry's hash in entry order without a
// []Entry — the GC mark phase's input (#482 arc).
func TestStreamChunkHashesYieldsEveryEntryInOrderAndStopsOnError(t *testing.T) {
	repo := t.TempDir()
	entries := make([]Entry, 300)
	for i := range entries {
		entries[i] = Entry{VolumeOffset: int64(i) * 4096, ChunkLength: 4096}
		entries[i].ChunkHash[0], entries[i].ChunkHash[1] = byte(i), byte(i>>8)
	}
	b := &Backup{BackupID: "stream-hashes", Timestamp: time.Now(), TotalBytes: 300 * 4096, Entries: entries}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDNMReader(DNMPath(repo, b.BackupID))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got [][32]byte
	if err := r.StreamChunkHashes(func(h [32]byte) error { got = append(got, h); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(entries) {
		t.Fatalf("streamed %d hashes, want %d", len(got), len(entries))
	}
	for i := range entries {
		if got[i] != entries[i].ChunkHash {
			t.Fatalf("hash %d differs", i)
		}
	}
	stop := fmt.Errorf("stop here")
	calls := 0
	err = r.StreamChunkHashes(func([32]byte) error {
		calls++
		if calls == 7 {
			return stop
		}
		return nil
	})
	if err != stop || calls != 7 {
		t.Fatalf("fn's error was not returned first (err=%v calls=%d)", err, calls)
	}
}

// The pipeline streams a large catalog through a sidecar file so the
// manifest writer never holds it whole: WriteCatalogSidecar produces it,
// Save consumes it, and what Load reads back is the catalog that went in.
func TestCatalogSidecarRoundTripsThroughSave(t *testing.T) {
	repo := t.TempDir()
	files := catalogFixture(5000)
	sidecar := filepath.Join(t.TempDir(), "catalog.sidecar")
	n, err := WriteCatalogSidecar(files, sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(files)) {
		t.Fatalf("WriteCatalogSidecar wrote %d records, want %d", n, len(files))
	}
	b := &Backup{BackupID: "sidecar-cat", Timestamp: time.Now(), TotalBytes: 4096, CatalogSidecarPath: sidecar,
		Entries: []Entry{{VolumeOffset: 0, ChunkLength: 4096}}}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
	got, err := Load(repo, b.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FileCatalog) != len(files) {
		t.Fatalf("loaded %d catalog records, want %d — the sidecar did not reach the manifest", len(got.FileCatalog), len(files))
	}
	for _, i := range []int{0, 1234, 4999} {
		if got.FileCatalog[i].Path != files[i].Path || got.FileCatalog[i].ContentHash != files[i].ContentHash || got.FileCatalog[i].Size != files[i].Size {
			t.Fatalf("record %d = %+v, want %+v", i, got.FileCatalog[i], files[i])
		}
	}
	if _, err := WriteCatalogSidecar(files, filepath.Join(t.TempDir(), "no", "such", "dir", "x")); err == nil {
		t.Fatal("writing a sidecar into a missing directory returned no error")
	}
}

// FileEntry's ContentHash travels as hex in JSON (the legacy manifest
// format and every API that emits catalog entries).
func TestFileEntryJSONRoundTripsTheContentHashAsHex(t *testing.T) {
	f := catalogFixture(1)[0]
	f.ContentHash = [32]byte{0xde, 0xad, 0xbe, 0xef}
	f.IsExcluded = true
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !containsAll(string(raw), `"content_hash":"deadbeef`, `"is_excluded":true`) {
		t.Fatalf("marshal = %s", raw)
	}
	var back FileEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.ContentHash != f.ContentHash || back.Path != f.Path || !back.IsExcluded || !back.ModTime.Equal(f.ModTime) {
		t.Fatalf("round trip = %+v, want %+v", back, f)
	}
	// A full-length value that is not hex is refused; a short or empty value
	// is "no hash recorded" (older manifests wrote none), never an error.
	bad := `{"path":"x","content_hash":"` + repeatString("zz", 32) + `"}`
	if err := json.Unmarshal([]byte(bad), &back); err == nil {
		t.Fatal("a 64-character non-hex content_hash unmarshalled without error")
	}
	var none FileEntry
	if err := json.Unmarshal([]byte(`{"path":"x","content_hash":""}`), &none); err != nil || none.ContentHash != ([32]byte{}) {
		t.Fatalf("an empty content_hash: err=%v hash=%x", err, none.ContentHash)
	}
}

func repeatString(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = os.Stat
