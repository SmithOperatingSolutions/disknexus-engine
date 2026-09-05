//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"os"
	"testing"
)

func TestScanExt4(t *testing.T) {
	f, err := os.Open(testdataPath(t, "ext4.img"))
	if err != nil {
		t.Fatalf("open ext4.img: %v", err)
	}
	defer f.Close()

	entries, err := scanExt4(f, 0)
	if err != nil {
		t.Fatalf("scanExt4: %v", err)
	}

	hello := findFile(entries, "hello.txt")
	if hello == nil {
		t.Fatal("hello.txt not found in scan results")
	}
	if hello.Size != 17 {
		t.Errorf("hello.txt size: got %d, want 17", hello.Size)
	}
	if len(hello.VolumeExtents) == 0 {
		t.Error("hello.txt should have at least one VolumeExtent on ext4")
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

func TestScanVolumeExt4(t *testing.T) {
	imgPath := testdataPath(t, "ext4.img")
	result, err := ScanVolume(context.Background(), imgPath, 0, nil, "")
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}

	if result.Filesystem != "ext4" {
		t.Errorf("Filesystem: got %q, want ext4", result.Filesystem)
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
