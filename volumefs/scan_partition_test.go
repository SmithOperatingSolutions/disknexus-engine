// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
)

// #151: machine snapshots need per-member catalogs — which means scanning
// an NTFS filesystem embedded at a partition offset INSIDE a disk image,
// via ReaderAt (no temp extraction). Same scan must work whether the disk
// around it is GPT or MBR.
func scanEmbedded(t *testing.T, img []byte, l *disklayout.DiskLayout, partIdx int) *ScanResult {
	t.Helper()
	p := l.Partitions[partIdx]
	res, err := ScanPartition(context.Background(), bytes.NewReader(img),
		p.Offset(l.SectorSize), p.Length(l.SectorSize))
	if err != nil {
		t.Fatalf("ScanPartition: %v", err)
	}
	return res
}

func embedNTFS(t *testing.T, img []byte, l *disklayout.DiskLayout, partIdx int) {
	t.Helper()
	ntfs, err := os.ReadFile("testdata/ntfs.img")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Partitions[partIdx]
	if int64(len(ntfs)) > p.Length(l.SectorSize) {
		t.Fatalf("fixture (%d) larger than partition (%d)", len(ntfs), p.Length(l.SectorSize))
	}
	copy(img[p.Offset(l.SectorSize):], ntfs)
}

func assertKnownCatalog(t *testing.T, res *ScanResult) {
	t.Helper()
	if res.Filesystem != "ntfs" {
		t.Fatalf("filesystem = %q", res.Filesystem)
	}
	found := map[string]bool{}
	for _, f := range res.Files {
		found[f.Path] = true
	}
	for _, want := range []string{"./hello.txt", "./dir1/data.bin"} {
		if !found[want] {
			t.Fatalf("catalog missing %s; have %d files", want, len(res.Files))
		}
	}
}

func TestScanPartitionInsideGPT(t *testing.T) {
	img := gpttest.BuildGPT(t, 512, 40960, []gpttest.SynthPart{
		{TypeGUID: disklayout.TypeMSBasicData, Name: "Basic data partition", Sectors: 20480},
	})
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	embedNTFS(t, img, l, 0)
	assertKnownCatalog(t, scanEmbedded(t, img, l, 0))
}

func TestScanPartitionInsideMBR(t *testing.T) {
	img := gpttest.BuildMBR(t, 512, 40960, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 20480, Bootable: true},
		{Type: 0x07, Sectors: 8192, Logical: true},
	})
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	embedNTFS(t, img, l, 0)
	assertKnownCatalog(t, scanEmbedded(t, img, l, 0))
}

// Pre-existing gap exposed by #151: resident NTFS files (data inside the
// MFT record) produced catalog entries with a size but neither extents nor
// InlineData — individually unrestorable from ANY capture-files backup.
// The scanner must inline resident content (restore already handles it).
func TestScanCapturesResidentFileData(t *testing.T) {
	res, err := ScanVolume(context.Background(), "testdata/ntfs.img", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Files {
		if f.Path == "./hello.txt" {
			if len(f.VolumeExtents) > 0 {
				// FIXTURE DRIFT IS A FAILURE, not an exemption (#402, the
				// #378 rule): this skip fired exactly when the behavior
				// under test was absent — a capture that stops storing
				// resident files inline reads as "fixture changed" and the
				// guard deletes itself.
				t.Fatalf("hello.txt is non-resident on this image (%d extents) — either the fixture drifted or resident-file capture (#151) stopped treating it as resident; both are failures this test exists to catch", len(f.VolumeExtents))
			}
			if len(f.InlineData) == 0 {
				t.Fatalf("resident file has neither extents nor InlineData: %+v", f)
			}
			return
		}
	}
	t.Fatal("./hello.txt not in catalog")
}
