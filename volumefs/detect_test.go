// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"encoding/binary"
	"os"
	"testing"
)

func TestDetectFSAtNTFS(t *testing.T) {
	// Create a minimal buffer with NTFS magic at offset 3
	buf := make([]byte, 2048)
	copy(buf[3:7], "NTFS")

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, 0)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "ntfs" {
		t.Errorf("got %q, want ntfs", fsType)
	}
}

func TestDetectFSAtFAT32(t *testing.T) {
	// Create a minimal buffer with FAT32 string at offset 0x52
	buf := make([]byte, 2048)
	copy(buf[0x52:0x5A], "FAT32   ")

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, 0)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "fat32" {
		t.Errorf("got %q, want fat32", fsType)
	}
}

func TestDetectFSAtFAT32Alt(t *testing.T) {
	// FAT32 string at alternate offset 0x36
	buf := make([]byte, 2048)
	copy(buf[0x36:0x3B], "FAT32")

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, 0)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "fat32" {
		t.Errorf("got %q, want fat32", fsType)
	}
}

func TestDetectFSAtExFAT(t *testing.T) {
	// exFAT: bytes 3-10 = "EXFAT   " (8 bytes with trailing spaces)
	buf := make([]byte, 2048)
	copy(buf[3:11], "EXFAT   ")

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, 0)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "exfat" {
		t.Errorf("got %q, want exfat", fsType)
	}
}

func TestDetectFSAtExt4(t *testing.T) {
	// ext4: magic 0xEF53 at superblock offset 1024+56, with the EXTENTS
	// incompat feature (0x40) set at 1024+0x60.
	buf := make([]byte, 2048)
	binary.LittleEndian.PutUint16(buf[1024+56:1024+58], 0xEF53)
	binary.LittleEndian.PutUint32(buf[1024+0x60:1024+0x64], 0x40)

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, 0)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "ext4" {
		t.Errorf("got %q, want ext4", fsType)
	}
}

// TestDetectFSAtRejectsExt2 proves that an ext2/ext3 filesystem (magic 0xEF53
// but no EXTENTS incompat feature) is rejected instead of being treated as
// ext4. ext2/3 inodes are block-mapped, so the extent-tree scanner produces
// zero extents and every file would restore as all zeros with no error.
func TestDetectFSAtRejectsExt2(t *testing.T) {
	buf := make([]byte, 2048)
	binary.LittleEndian.PutUint16(buf[1024+56:1024+58], 0xEF53)
	// feature_incompat without the EXTENTS bit (e.g. filetype 0x2 only).
	binary.LittleEndian.PutUint32(buf[1024+0x60:1024+0x64], 0x2)

	f := writeTempFile(t, buf)
	defer f.Close()

	if _, err := detectFSAt(f, 0); err == nil {
		t.Fatal("detectFSAt accepted an ext2/ext3 (no-extents) superblock as ext4; want an error")
	}
}

func TestDetectFSAtWithOffset(t *testing.T) {
	// NTFS filesystem starting at offset 1048576 (1 MB partition offset)
	partOffset := int64(1048576)
	buf := make([]byte, partOffset+2048)
	copy(buf[partOffset+3:partOffset+7], "NTFS")

	f := writeTempFile(t, buf)
	defer f.Close()

	fsType, err := detectFSAt(f, partOffset)
	if err != nil {
		t.Fatalf("detectFSAt: %v", err)
	}
	if fsType != "ntfs" {
		t.Errorf("got %q, want ntfs", fsType)
	}
}

func TestDetectFSAtUnknown(t *testing.T) {
	buf := make([]byte, 2048) // all zeros

	f := writeTempFile(t, buf)
	defer f.Close()

	_, err := detectFSAt(f, 0)
	if err == nil {
		t.Fatal("expected error for unknown filesystem")
	}
}

func TestDetectFSAtTooSmall(t *testing.T) {
	buf := make([]byte, 100) // too small

	f := writeTempFile(t, buf)
	defer f.Close()

	_, err := detectFSAt(f, 0)
	if err == nil {
		t.Fatal("expected error for too-small input")
	}
}

// writeTempFile creates a temp file with the given contents and returns it.
func writeTempFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "detect_test_*")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("writing temp file: %v", err)
	}
	return f
}
