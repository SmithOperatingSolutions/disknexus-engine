//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestAddSubtreeExclusionRanges_NTFS is the repo-self-capture guarantee at the
// extent level: excluding the "dir1" subtree must cover every physical extent
// of dir1/data.bin while leaving root files (hello.txt) untouched. This is the
// mechanism that keeps a repo/temp directory stored on the captured volume out
// of its own backup stream.
func TestAddSubtreeExclusionRanges_NTFS(t *testing.T) {
	img := testdataPath(t, "ntfs.img")

	m := volume.NewExclusionMap()
	if err := AddSubtreeExclusionRanges(img, "dir1", m); err != nil {
		t.Fatalf("AddSubtreeExclusionRanges: %v", err)
	}
	if m.Len() == 0 {
		t.Fatal("no ranges excluded for existing subtree dir1")
	}

	// Ground truth from the scanner (same parser, independent path).
	res, err := ScanVolume(context.Background(), img, 8<<20, nil, "")
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}
	var dataExts int
	var dataBytes int64
	for _, f := range res.Files {
		if strings.TrimPrefix(f.Path, "./") != "dir1/data.bin" {
			continue
		}
		for _, e := range f.VolumeExtents {
			dataExts++
			dataBytes += e.Length
			if !m.IsExcluded(e.VolumeOffset, e.Length) {
				t.Errorf("dir1/data.bin extent [%d,+%d) not excluded", e.VolumeOffset, e.Length)
			}
		}
	}
	if dataExts == 0 {
		t.Fatal("fixture has no extents for dir1/data.bin — fixture regression?")
	}

	// Negative probes: the exclusion must be surgical. The boot sector and the
	// backup boot sector region must never be excluded (hello.txt is
	// MFT-resident, so any over-exclusion would show up as filesystem-metadata
	// ranges like these landing in the map).
	if m.IsExcluded(0, 512) {
		t.Error("boot sector wrongly excluded")
	}
	if m.IsExcluded(8<<20-4096, 4096) {
		t.Error("end-of-volume region wrongly excluded")
	}
	// Total exclusion must be on the order of data.bin (cluster-rounded), not
	// the whole volume: no more than 4 ranges and 4× the file's extent bytes.
	if m.Len() > 4 {
		t.Errorf("expected a handful of ranges for one 4 KB file, got %d", m.Len())
	}
	var covered int64
	for off := int64(0); off < 8<<20; off += 4096 {
		if m.IsExcluded(off, 4096) {
			covered += 4096
		}
	}
	if covered > 4*dataBytes {
		t.Errorf("excluded ~%d bytes for a %d-byte file — over-exclusion", covered, dataBytes)
	}
}

// TestAddSubtreeExclusionRanges_MissingSubtree: an absent path is a clean
// no-op, not an error (the repo may simply not live on the captured volume).
func TestAddSubtreeExclusionRanges_MissingSubtree(t *testing.T) {
	img := testdataPath(t, "ntfs.img")
	m := volume.NewExclusionMap()
	if err := AddSubtreeExclusionRanges(img, "no/such/dir", m); err != nil {
		t.Fatalf("missing subtree must not error, got: %v", err)
	}
	if m.Len() != 0 {
		t.Fatalf("missing subtree excluded %d ranges", m.Len())
	}
}

// TestAddSubtreeExclusionRanges_NonNTFS: non-NTFS sources are a no-op.
func TestAddSubtreeExclusionRanges_NonNTFS(t *testing.T) {
	img := testdataPath(t, "ext4.img")
	m := volume.NewExclusionMap()
	if err := AddSubtreeExclusionRanges(img, "dir1", m); err != nil {
		t.Fatalf("non-NTFS must be a no-op, got: %v", err)
	}
	if m.Len() != 0 {
		t.Fatalf("non-NTFS excluded %d ranges", m.Len())
	}
}

// TestMarkExcludedFiles_NTFSFixture is #94 end-to-end at the volumefs level:
// scan the real fixture, exclude the dir1 subtree, mark the catalog — the
// file inside dir1 is flagged, root files are not.
func TestMarkExcludedFiles_NTFSFixture(t *testing.T) {
	img := testdataPath(t, "ntfs.img")
	m := volume.NewExclusionMap()
	if err := AddSubtreeExclusionRanges(img, "dir1", m); err != nil {
		t.Fatal(err)
	}
	res, err := ScanVolume(context.Background(), img, 8<<20, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if n := MarkExcludedFiles(res.Files, m); n == 0 {
		t.Fatal("nothing marked despite dir1 exclusion")
	}
	seen := map[string]bool{}
	for _, f := range res.Files {
		seen[strings.TrimPrefix(f.Path, "./")] = f.IsExcluded
	}
	if !seen["dir1/data.bin"] {
		t.Fatal("dir1/data.bin not marked excluded")
	}
	if seen["hello.txt"] {
		t.Fatal("hello.txt wrongly marked excluded")
	}
}
