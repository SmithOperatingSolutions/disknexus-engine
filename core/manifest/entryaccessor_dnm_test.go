// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// A DNM-backed accessor is what every block restore and file restore reads
// entries through; it must agree with the in-memory slice on every index,
// every range, and both binary searches — the searches are what a file
// restore uses to find the chunks covering a file's extents, so an
// off-by-one there restores the wrong bytes into the right file.
func TestDNMEntryAccessorAgreesWithTheSlice(t *testing.T) {
	repo := t.TempDir()
	const n = 1000
	entries := make([]Entry, n)
	var off int64
	for i := range entries {
		// Irregular lengths and a gap every 100 entries, so offsets are
		// not a multiple the searches could get right by arithmetic.
		length := 4096 + (i%7)*512
		if i%100 == 50 {
			off += 65536 // a hole: excluded/unmapped region
		}
		entries[i] = Entry{VolumeOffset: off, ChunkLength: length}
		entries[i].ChunkHash[0], entries[i].ChunkHash[1] = byte(i), byte(i>>8)
		off += int64(length)
	}
	b := &Backup{BackupID: "dnm-accessor", Timestamp: time.Now(), TotalBytes: off, TotalChunks: n, Entries: entries}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DNMPath(repo, b.BackupID)); err != nil {
		t.Fatalf("fixture: Save did not write a DNM: %v", err)
	}

	ea, closer, err := NewEntryAccessor(repo, b.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()
	if _, isSlice := ea.(*sliceEntryAccessor); isSlice {
		t.Fatal("NewEntryAccessor fell back to a slice with a DNM present — the O(1)-seek path is not under test")
	}
	if ea.Count() != n {
		t.Fatalf("Count = %d, want %d", ea.Count(), n)
	}
	for _, i := range []int64{0, 1, 49, 50, 51, 499, 999} {
		got, err := ea.At(i)
		if err != nil {
			t.Fatalf("At(%d): %v", i, err)
		}
		if got != entries[i] {
			t.Fatalf("At(%d) = %+v, want %+v", i, got, entries[i])
		}
	}
	if _, err := ea.At(n); err == nil {
		t.Fatal("At(Count) returned no error")
	}
	for _, r := range [][2]int64{{0, 1}, {0, n}, {49, 52}, {998, 1000}, {300, 300}} {
		got, err := ea.Range(r[0], r[1])
		if err != nil {
			t.Fatalf("Range%v: %v", r, err)
		}
		want := entries[r[0]:r[1]]
		if len(got) != len(want) {
			t.Fatalf("Range%v: %d entries, want %d", r, len(got), len(want))
		}
		for k := range want {
			if got[k] != want[k] {
				t.Fatalf("Range%v[%d] = %+v, want %+v", r, k, got[k], want[k])
			}
		}
	}

	// The searches, against a linear scan (the authority, §3).
	linearStart := func(target int64) int64 {
		for i, e := range entries {
			if e.VolumeOffset+int64(e.ChunkLength) > target {
				return int64(i)
			}
		}
		return n
	}
	linearEnd := func(target int64) int64 {
		return int64(sort.Search(n, func(i int) bool { return entries[i].VolumeOffset >= target }))
	}
	targets := []int64{0, 1, entries[50].VolumeOffset - 1, entries[50].VolumeOffset, entries[50].VolumeOffset + 1,
		entries[499].VolumeOffset + 100, entries[n-1].VolumeOffset, off - 1, off, off + 12345}
	for _, tg := range targets {
		s, err := SearchEntries(ea, tg)
		if err != nil {
			t.Fatal(err)
		}
		if want := linearStart(tg); s != want {
			t.Errorf("SearchEntries(%d) = %d, want %d (linear)", tg, s, want)
		}
		e, err := SearchEntriesEnd(ea, tg)
		if err != nil {
			t.Fatal(err)
		}
		if want := linearEnd(tg); e != want {
			t.Errorf("SearchEntriesEnd(%d) = %d, want %d (linear)", tg, e, want)
		}
	}
	// Anti-vacuity: the targets land inside the hole and past the end, so
	// start and end disagree somewhere and the past-end case returns Count.
	if s, _ := SearchEntries(ea, off+12345); s != n {
		t.Fatalf("past-end SearchEntries = %d, want Count", s)
	}
}

// ListIDs is the raw directory scan prune and forget source their ID set
// from: a corrupt-but-present manifest must be VISIBLE there even though
// List cannot parse it — otherwise a sweep would treat its packs as
// unreferenced and delete a backup that still exists.
func TestListIDsSeesWhatListCannotParse(t *testing.T) {
	repo := t.TempDir()
	good := &Backup{BackupID: "good-0000-0000-0000-000000000001", Timestamp: time.Now(), TotalBytes: 4096,
		Entries: []Entry{{VolumeOffset: 0, ChunkLength: 4096}}}
	if err := good.Save(repo); err != nil {
		t.Fatal(err)
	}
	corrupt := "bad0-0000-0000-0000-000000000002"
	if err := os.WriteFile(DNMPath(repo, corrupt), []byte("not a manifest at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	ids, err := ListIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != corrupt || ids[1] != good.BackupID {
		t.Fatalf("ListIDs = %v, want both the good and the corrupt backup", ids)
	}
	listed, err := List(repo)
	if err != nil {
		t.Fatalf("List with a corrupt sibling: %v", err)
	}
	if len(listed) != 1 || listed[0].BackupID != good.BackupID {
		t.Fatalf("List = %d backups, want only the parseable one (the cross-check ListIDs exists for)", len(listed))
	}
	if ids, _ := ListIDs(filepath.Join(repo, "nope")); len(ids) != 0 {
		t.Fatalf("ListIDs on a missing repo = %v", ids)
	}
}
