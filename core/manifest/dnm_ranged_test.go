// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"
)

// Prod defect 2026-08-19: Web Restore browse of a disk-member backup
// answered an ingress-level Bad Gateway because the controller downloaded
// the ENTIRE .dnm (block entries for every chunk of a physical partition)
// just to read the file catalog. The DNM layout is sectioned precisely so
// this isn't necessary: a ranged catalog read must fetch only the header,
// section index and catalog — never the entries bulk.
func buildBigManifest(t *testing.T, entries int, files int) (string, string) {
	t.Helper()
	repo := t.TempDir()
	b := &Backup{BackupID: "ranged-test", Timestamp: time.Now().UTC()}
	for i := 0; i < entries; i++ {
		var h [32]byte
		rand.Read(h[:])
		b.Entries = append(b.Entries, Entry{VolumeOffset: int64(i) * 4096, ChunkHash: h, ChunkLength: 4096})
	}
	for i := 0; i < files; i++ {
		b.FileCatalog = append(b.FileCatalog, FileEntry{
			Path: fmt.Sprintf("dir/file-%04d.txt", i), Size: 123, ModTime: time.Now().UTC(),
		})
	}
	b.TotalBytes = int64(entries) * 4096
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
	return repo, DNMPath(repo, "ranged-test")
}

func TestStreamCatalogRangedFetchesOnlyCatalog(t *testing.T) {
	_, dnm := buildBigManifest(t, 50_000, 40) // entries dwarf the catalog
	data, err := os.ReadFile(dnm)
	if err != nil {
		t.Fatal(err)
	}
	var fetched int64
	readRange := func(off, n int64) ([]byte, error) {
		if off < 0 || off+n > int64(len(data)) {
			return nil, fmt.Errorf("range [%d,%d) outside %d", off, off+n, len(data))
		}
		fetched += n
		return data[off : off+n], nil
	}

	var got []FileEntry
	if err := StreamCatalogRanged(int64(len(data)), readRange, func(fe FileEntry) bool {
		got = append(got, fe)
		return true
	}); err != nil {
		t.Fatalf("StreamCatalogRanged: %v", err)
	}
	if len(got) != 40 {
		t.Fatalf("catalog = %d files, want 40", len(got))
	}
	// Every record, in stored order — not just the first one.
	for i, fe := range got {
		if want := fmt.Sprintf("dir/file-%04d.txt", i); fe.Path != want || fe.Size != 123 {
			t.Fatalf("record %d = %q size %d, want %q size 123", i, fe.Path, fe.Size, want)
		}
	}
	// The point of the exercise: the 50k-entry bulk must not be fetched.
	if max := int64(len(data)) / 10; fetched > max {
		t.Fatalf("fetched %d of %d bytes — the entries section was downloaded (limit %d)", fetched, len(data), max)
	}
}

// The streamed layout (zero header sentinel + 8-byte trailer) is the NORM
// for cloud-uploaded manifests — ranged reads must handle it.
func TestStreamCatalogRangedStreamedLayout(t *testing.T) {
	b := &Backup{BackupID: "ranged-streamed", Timestamp: time.Now().UTC()}
	for i := 0; i < 7; i++ {
		b.FileCatalog = append(b.FileCatalog, FileEntry{Path: fmt.Sprintf("f%d", i), Size: 9})
	}
	var buf bytes.Buffer
	st := NewDNMStreamer(4096, func(p []byte) error { buf.Write(p); return nil })
	for i := 0; i < 1000; i++ {
		var h [32]byte
		rand.Read(h[:])
		if err := st.WriteEntry(Entry{VolumeOffset: int64(i) * 4096, ChunkHash: h, ChunkLength: 4096}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish(b); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	var fetched int64
	var got []string
	err := StreamCatalogRanged(int64(len(data)), func(off, n int64) ([]byte, error) {
		fetched += n
		if off < 0 || off+n > int64(len(data)) {
			return nil, fmt.Errorf("bad range")
		}
		return data[off : off+n], nil
	}, func(fe FileEntry) bool {
		got = append(got, fe.Path)
		return true
	})
	if err != nil {
		t.Fatalf("StreamCatalogRanged(streamed): %v", err)
	}
	if len(got) != 7 || got[0] != "f0" || got[6] != "f6" {
		t.Fatalf("catalog = %v, want f0..f6", got)
	}
	if fetched > int64(len(data))/2 {
		t.Fatalf("streamed layout fetched %d of %d bytes", fetched, len(data))
	}
}

// #419: fetching only the catalog is not enough — the catalog SECTION of a
// whole-volume manifest is tens of megabytes on its own, and reading it in
// one range materializes all of it before a single record is decoded. The
// reader must window the section: no single read may be proportional to the
// number of files on the volume.
func TestCatalogSectionIsNeverReadInOneRange(t *testing.T) {
	_, dnm := buildBigManifest(t, 2_000, 60_000) // a volume catalog, not a file one
	data, err := os.ReadFile(dnm)
	if err != nil {
		t.Fatal(err)
	}
	var reads int
	var biggest int64
	readRange := func(off, n int64) ([]byte, error) {
		if off < 0 || off+n > int64(len(data)) {
			return nil, fmt.Errorf("range [%d,%d) outside %d", off, off+n, len(data))
		}
		reads++
		biggest = max(biggest, n)
		return data[off : off+n], nil
	}

	var files int
	if err := StreamCatalogRanged(int64(len(data)), readRange, func(FileEntry) bool {
		files++
		return true
	}); err != nil {
		t.Fatalf("ranged catalog read: %v", err)
	}
	if reads == 0 {
		t.Fatal("no ranged reads were made — this guard scanned nothing")
	}
	if files != 60_000 {
		t.Fatalf("catalog = %d files, want 60000", files)
	}

	// One window, generously sized. The catalog section here is several MB;
	// on a 63.8 GB NTFS member it is tens of MB.
	const window = 4 << 20
	if biggest > window {
		t.Fatalf("the largest single read of the manifest was %d bytes for a %d-file catalog "+
			"(object is %d bytes) — the whole catalog section is pulled into one buffer before "+
			"any record is decoded, so the controller's peak heap tracks the number of files on "+
			"the volume. On a real 63.8 GB member that buffer plus the decoded records is ~180 MB "+
			"against a 256Mi limit: the pod is OOM-killed (exit 137) and the panel shows a bare 502.",
			biggest, files, len(data))
	}
}

// A caller that has found what it came for stops the scan — the zip restore
// resolves a handful of exact paths this way and must not pay for the rest of
// the volume. Records still arrive in stored order up to the stop.
func TestStreamCatalogRangedStopsWhenVisitSaysSo(t *testing.T) {
	// The catalog has to span several windows for a stop to be observable at
	// all; one that fits in a single read is already bounded.
	_, dnm := buildBigManifest(t, 100, 60_000)
	data, err := os.ReadFile(dnm)
	if err != nil {
		t.Fatal(err)
	}
	var fetched int64
	readRange := func(off, n int64) ([]byte, error) {
		fetched += n
		return data[off : off+n], nil
	}

	// Positive control: the same fixture, read to the end, sees all 5000.
	var all int
	if err := StreamCatalogRanged(int64(len(data)), readRange, func(FileEntry) bool {
		all++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if all != 60_000 {
		t.Fatalf("full read saw %d records, want 60000", all)
	}
	fullFetch := fetched
	if fullFetch <= catalogRangeWindow {
		t.Fatalf("the fixture's catalog is %d bytes — it fits in one window, so an early stop "+
			"cannot be distinguished from a full read", fullFetch)
	}

	fetched = 0
	var seen []string
	if err := StreamCatalogRanged(int64(len(data)), readRange, func(fe FileEntry) bool {
		seen = append(seen, fe.Path)
		return len(seen) < 3
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != "dir/file-0000.txt" || seen[2] != "dir/file-0002.txt" {
		head := seen
		if len(head) > 5 {
			head = head[:5]
		}
		t.Fatalf("a caller that stopped after 3 records was handed %d, starting %v — the stop is "+
			"ignored, so resolving a couple of paths out of a whole-volume catalog decodes every "+
			"file on the volume", len(seen), head)
	}
	if fetched >= fullFetch {
		t.Fatalf("stopping after 3 of 60000 records still fetched %d bytes (a full read fetches %d) — "+
			"the reader is not honouring the stop, so a zip of two files pays for the whole volume",
			fetched, fullFetch)
	}
}
