// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/partition/part"
)

// detectFilesystem reads magic bytes to identify the filesystem type.
// If a partition table is found, it returns the byte offset of the first
// data partition. Otherwise partOffset is 0.
func detectFilesystem(f *os.File) (fsType string, partOffset int64, err error) {
	// Try direct filesystem detection first (no partition table)
	fsType, err = detectFSAt(f, 0)
	if err == nil {
		return fsType, 0, nil
	}

	// Check for partition table (MBR or GPT)
	partOffset, err = findFirstPartition(f)
	if err != nil {
		return "", 0, fmt.Errorf("no recognized filesystem or partition table")
	}

	fsType, err = detectFSAt(f, partOffset)
	if err != nil {
		return "", 0, fmt.Errorf("no recognized filesystem in partition at offset %d", partOffset)
	}
	return fsType, partOffset, nil
}

// detectFSAt tries to identify a filesystem starting at the given byte offset.
func detectFSAt(r io.ReaderAt, offset int64) (string, error) {
	// Read first 2048 bytes from the offset (enough for boot sector + ext4 superblock)
	buf := make([]byte, 2048)
	n, err := r.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return "", err
	}
	if n < 1088 { // need at least 1024+64 for ext4 superblock
		return "", fmt.Errorf("insufficient data")
	}

	// NTFS: bytes 3-7 = "NTFS"
	if n >= 7 && string(buf[3:7]) == "NTFS" {
		return "ntfs", nil
	}

	// exFAT: bytes 3-10 = "EXFAT   " (8 bytes)
	if n >= 11 && string(buf[3:11]) == "EXFAT   " {
		return "exfat", nil
	}

	// FAT32: check for FAT32 string at offset 0x52 (82) in boot sector
	if n >= 90 && string(buf[0x52:0x5A]) == "FAT32   " {
		return "fat32", nil
	}
	// Also check older location at offset 0x36
	if n >= 62 && string(buf[0x36:0x3B]) == "FAT32" {
		return "fat32", nil
	}

	// ext4: superblock at offset 1024 within the filesystem, magic 0xEF53 at +56.
	// ext2/ext3/ext4 all share this magic, but only ext4 uses extent-mapped
	// inodes (the extent-tree scanner). ext2/ext3 use block/indirect mapping,
	// which the scanner does not parse — it would silently produce zero-extent
	// (all-zero) files. Require the EXTENTS incompat feature (0x40, at
	// s_feature_incompat = superblock offset 0x60) so ext2/ext3 volumes fail
	// loudly rather than backing up empty files.
	if n >= 1082 {
		magic := binary.LittleEndian.Uint16(buf[1024+56 : 1024+58])
		if magic == 0xEF53 {
			const featureIncompatExtents = 0x40
			if n >= 1124 {
				featIncompat := binary.LittleEndian.Uint32(buf[1024+0x60 : 1024+0x64])
				if featIncompat&featureIncompatExtents == 0 {
					return "", fmt.Errorf("ext2/ext3 (block-mapped) filesystems are not supported; only ext4 with extents")
				}
			}
			return "ext4", nil
		}
	}

	return "", fmt.Errorf("unrecognized filesystem")
}

// findFirstPartition uses go-diskfs to parse MBR/GPT and return the start
// byte offset of the first data partition.
func findFirstPartition(f *os.File) (int64, error) {
	// diskfs.Open opens the path AGAIN — its own handle, independent of f.
	// Closing it is ours to do: every capture probes its source here, so a
	// missed close leaked one handle per backup (invisible on Linux, fatal to
	// deletion on Windows).
	disk, err := diskfs.Open(f.Name(), diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return 0, fmt.Errorf("opening disk: %w", err)
	}
	defer disk.Close()

	table, err := disk.GetPartitionTable()
	if err != nil {
		return 0, fmt.Errorf("reading partition table: %w", err)
	}

	partitions := table.GetPartitions()
	if len(partitions) == 0 {
		return 0, fmt.Errorf("no partitions found")
	}

	// Find the first partition with a non-zero start
	for _, p := range partitions {
		pp, ok := p.(part.Partition)
		if !ok {
			continue
		}
		start := pp.GetStart()
		size := pp.GetSize()
		if start > 0 && size > 0 {
			return start, nil
		}
	}

	return 0, fmt.Errorf("no suitable partition found")
}
