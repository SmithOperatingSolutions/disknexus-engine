// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"crypto/sha256"
	"testing"
)

// A file's ContentHash is the SHA-256 over the hashes of the chunks that
// overlap its range — the identity a file-level verify and a cross-backup
// dedup decision rest on. The expectation here is computed by hand from
// the overlap rule, not by calling the function twice.
func TestContentHashesFollowTheChunkOverlapRule(t *testing.T) {
	// Four 1000-byte chunks in a stream of 4000 bytes.
	entries := make([]Entry, 4)
	for i := range entries {
		entries[i] = Entry{VolumeOffset: int64(i) * 1000, ChunkLength: 1000}
		entries[i].ChunkHash[0] = byte(0x10 + i)
	}
	hashOf := func(idx ...int) [32]byte {
		h := sha256.New()
		for _, i := range idx {
			h.Write(entries[i].ChunkHash[:])
		}
		var out [32]byte
		copy(out[:], h.Sum(nil))
		return out
	}
	b := &Backup{
		Entries: entries,
		FileCatalog: []FileEntry{
			{Path: "inside-one", StreamOffset: 100, StreamLength: 500},    // chunk 0 only
			{Path: "spans-two", StreamOffset: 900, StreamLength: 200},     // chunks 0 and 1
			{Path: "exact-chunk", StreamOffset: 2000, StreamLength: 1000}, // chunk 2 exactly
			{Path: "to-the-end", StreamOffset: 2500, StreamLength: 1500},  // chunks 2 and 3
			{Path: "empty", StreamOffset: 3000, StreamLength: 0},          // no bytes: no hash
			{Path: "dir", IsDir: true},
		},
	}
	ComputeContentHashes(b)
	want := map[string][32]byte{
		"inside-one":  hashOf(0),
		"spans-two":   hashOf(0, 1),
		"exact-chunk": hashOf(2),
		"to-the-end":  hashOf(2, 3),
	}
	for _, f := range b.FileCatalog {
		w, has := want[f.Path]
		if !has {
			if f.ContentHash != ([32]byte{}) {
				t.Errorf("%s: got a content hash for a file with no bytes", f.Path)
			}
			continue
		}
		if f.ContentHash != w {
			t.Errorf("%s: content hash is not the SHA-256 over its overlapping chunk hashes", f.Path)
		}
	}
	// Sensitivity: change chunk 1's hash and only the file spanning it moves.
	before := b.FileCatalog[1].ContentHash
	b.Entries[1].ChunkHash[5] = 0xff
	ComputeContentHashes(b)
	if b.FileCatalog[1].ContentHash == before {
		t.Fatal("changing a covering chunk's hash left the file's content hash unchanged")
	}
	if b.FileCatalog[0].ContentHash != want["inside-one"] {
		t.Fatal("a file outside the changed chunk moved too")
	}

	// A backup with no entries or no catalog is a no-op, not a panic.
	ComputeContentHashes(&Backup{FileCatalog: b.FileCatalog})
	ComputeContentHashes(&Backup{Entries: entries})
}

// Volume-file catalogs locate a file by physical extents, possibly several
// and out of order; the hash is over the chunks overlapping ANY extent.
func TestContentHashForVolumeFileCoversEveryExtent(t *testing.T) {
	entries := make([]Entry, 6)
	for i := range entries {
		entries[i] = Entry{VolumeOffset: int64(i) * 4096, ChunkLength: 4096}
		entries[i].ChunkHash[0] = byte(0x20 + i)
	}
	ea := NewSliceEntryAccessor(entries)
	f := &FileEntry{Path: "fragmented", VolumeExtents: []VolumeExtent{
		{VolumeOffset: 4096 * 4, Length: 100},   // chunk 4
		{VolumeOffset: 100, Length: 3900},       // chunk 0 only: [100, 4000) stops short of chunk 1
		{VolumeOffset: 4096*2 - 10, Length: 20}, // chunks 1 and 2
	}}
	ComputeContentHashesForVolumeFile(f, ea)
	h := sha256.New()
	for _, i := range []int{4, 0, 1, 2} { // extent order, chunk order within an extent
		h.Write(entries[i].ChunkHash[:])
	}
	var want [32]byte
	copy(want[:], h.Sum(nil))
	if f.ContentHash != want {
		t.Fatal("volume-file content hash is not the SHA-256 over the chunks overlapping its extents, in extent order")
	}
	none := &FileEntry{Path: "no-extents"}
	ComputeContentHashesForVolumeFile(none, ea)
	if none.ContentHash != ([32]byte{}) {
		t.Fatal("a file with no extents got a content hash")
	}
}
