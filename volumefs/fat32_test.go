//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"testing"
)

// decompressFAT32 decompresses fat32.img.gz into a temp file and returns its path.
func decompressFAT32(t *testing.T) string {
	t.Helper()
	gz, err := os.Open(testdataPath(t, "fat32.img.gz"))
	if err != nil {
		t.Fatalf("open fat32.img.gz: %v", err)
	}
	defer gz.Close()

	r, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer r.Close()

	tmp, err := os.CreateTemp(t.TempDir(), "fat32_*.img")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	defer tmp.Close()

	if _, err := io.Copy(tmp, r); err != nil {
		t.Fatalf("decompressing fat32.img.gz: %v", err)
	}
	return tmp.Name()
}

func TestScanFAT32(t *testing.T) {
	imgPath := decompressFAT32(t)

	entries, err := scanFAT32(imgPath, 0)
	if err != nil {
		t.Fatalf("scanFAT32: %v", err)
	}

	hello := findFile(entries, "hello.txt")
	if hello == nil {
		t.Fatal("hello.txt not found in scan results")
	}
	if hello.Size != 17 {
		t.Errorf("hello.txt size: got %d, want 17", hello.Size)
	}
	if len(hello.VolumeExtents) == 0 {
		t.Error("hello.txt should have at least one VolumeExtent on FAT32")
	}

	dir1 := findFile(entries, "dir1")
	if dir1 == nil {
		t.Fatal("dir1 not found in scan results")
	}
	if !dir1.IsDir {
		t.Error("dir1.IsDir should be true")
	}

	data := findFile(entries, "dir1/data.bin")
	if data == nil {
		t.Fatal("dir1/data.bin not found in scan results")
	}
	if data.Size != 4096 {
		t.Errorf("dir1/data.bin size: got %d, want 4096", data.Size)
	}
	if len(data.VolumeExtents) == 0 {
		t.Error("dir1/data.bin should have at least one VolumeExtent")
	}
}

func TestScanVolumeFAT32(t *testing.T) {
	imgPath := decompressFAT32(t)

	result, err := ScanVolume(context.Background(), imgPath, 0, nil, "")
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}

	if result.Filesystem != "fat32" {
		t.Errorf("Filesystem: got %q, want fat32", result.Filesystem)
	}

	hello := findFile(result.Files, "hello.txt")
	if hello == nil {
		t.Fatal("hello.txt not found")
	}
	if hello.Size != 17 {
		t.Errorf("hello.txt size: got %d, want 17", hello.Size)
	}

	data := findFile(result.Files, "dir1/data.bin")
	if data == nil {
		t.Fatal("dir1/data.bin not found")
	}
	if data.Size != 4096 {
		t.Errorf("dir1/data.bin size: got %d, want 4096", data.Size)
	}
	if len(data.VolumeExtents) == 0 {
		t.Error("dir1/data.bin should have at least one VolumeExtent")
	}
}
