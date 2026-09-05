//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	parser "www.velocidex.com/golang/go-ntfs/parser"
)

// ntfsFileContentByPath reads the true content of a file from an NTFS image
// via the go-ntfs parser (ground truth), by walking the MFT tree.
func ntfsFileContentByPath(t *testing.T, f *os.File, want string, size int64) []byte {
	t.Helper()
	reader, _ := parser.NewPagedReader(f, 1024, 10000)
	ntfs, err := parser.GetNTFSContext(reader, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ntfs.Close()

	root, _ := ntfs.GetMFT(5)
	visited := map[int64]bool{}
	var find func(dir *parser.MFT_ENTRY, prefix string) *parser.MFT_ENTRY
	find = func(dir *parser.MFT_ENTRY, prefix string) *parser.MFT_ENTRY {
		for _, info := range parser.ListDir(ntfs, dir) {
			if info.Name == "." || info.Name == ".." || info.Name == "" {
				continue
			}
			p := info.Name
			if prefix != "" {
				p = prefix + "/" + info.Name
			}
			var n int64
			fmt.Sscanf(info.MFTId, "%d", &n)
			if p == want {
				e, _ := ntfs.GetMFT(n)
				return e
			}
			if info.IsDir && info.MFTId != "" && !visited[n] {
				visited[n] = true
				if child, err := ntfs.GetMFT(n); err == nil {
					if e := find(child, p); e != nil {
						return e
					}
				}
			}
		}
		return nil
	}
	entry := find(root, "")
	if entry == nil {
		t.Fatalf("%s not found via parser", want)
	}
	stream, err := parser.OpenStream(ntfs, entry, 128, 0xffff, "")
	if err != nil {
		t.Fatalf("OpenStream %s: %v", want, err)
	}
	buf := make([]byte, size)
	n, _ := stream.ReadAt(buf, 0)
	return buf[:n]
}

// TestNTFSExtentsPointAtPhysicalData proves the NTFS scanner records physical
// device offsets in VolumeExtent, not go-ntfs's file-logical run offsets.
// Reconstructing dir1/data.bin from its extents (reading the raw image at
// each VolumeOffset) must reproduce the file's true content. Before the fix,
// the extent's VolumeOffset was 0 (the boot sector).
func TestNTFSExtentsPointAtPhysicalData(t *testing.T) {
	f, err := os.Open(testdataPath(t, "ntfs.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	entries, err := scanNTFS(f, 0)
	if err != nil {
		t.Fatalf("scanNTFS: %v", err)
	}

	data := findFile(entries, "dir1/data.bin")
	if data == nil {
		t.Fatal("dir1/data.bin not found")
	}
	if len(data.VolumeExtents) == 0 {
		t.Fatal("dir1/data.bin has no extents")
	}

	want := ntfsFileContentByPath(t, f, "dir1/data.bin", data.Size)

	// Reconstruct the file from the raw image using the recorded extents.
	got := make([]byte, data.Size)
	for _, e := range data.VolumeExtents {
		raw := make([]byte, e.Length)
		if _, err := f.ReadAt(raw, e.VolumeOffset); err != nil {
			t.Fatalf("reading extent at VolumeOffset=%d: %v", e.VolumeOffset, err)
		}
		copy(got[e.FileOffset:], raw)
	}

	if !bytes.Equal(got, want) {
		t.Fatalf("reconstructed data.bin does not match true content\n got  first8=%x\n want first8=%x\n (VolumeOffset points at the wrong device location)",
			got[:8], want[:8])
	}
}

// TestNTFSResidentFileHasNoExtents proves that a resident file (data stored
// inside the MFT record, not on a data cluster) is cataloged with no
// VolumeExtents rather than a bogus {0,0,size} extent.
func TestNTFSResidentFileHasNoExtents(t *testing.T) {
	f, err := os.Open(testdataPath(t, "ntfs.img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	entries, err := scanNTFS(f, 0)
	if err != nil {
		t.Fatalf("scanNTFS: %v", err)
	}
	hello := findFile(entries, "hello.txt")
	if hello == nil {
		t.Fatal("hello.txt not found")
	}
	if len(hello.VolumeExtents) != 0 {
		t.Fatalf("resident hello.txt should have no extents, got %d: %+v", len(hello.VolumeExtents), hello.VolumeExtents)
	}
}
