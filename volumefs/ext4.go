// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	ext4 "github.com/dsoprea/go-ext4"
)

// scanExt4 enumerates all files on an ext4 filesystem and builds
// VolumeExtents from their physical block locations.
func scanExt4(f *os.File, partOffset int64) ([]manifest.FileEntry, error) {
	// Parse superblock at offset 1024 within the filesystem
	_, err := f.Seek(partOffset+ext4.Superblock0Offset, io.SeekStart)
	if err != nil {
		return nil, fmt.Errorf("seeking to superblock: %w", err)
	}

	sb, err := ext4.NewSuperblockWithReader(f)
	if err != nil {
		return nil, fmt.Errorf("parsing superblock: %w", err)
	}

	blockSize := int64(sb.BlockSize())

	// Load block group descriptors
	bgdl, err := ext4.NewBlockGroupDescriptorListWithReadSeeker(f, sb)
	if err != nil {
		return nil, fmt.Errorf("parsing block group descriptors: %w", err)
	}

	// Walk directory tree starting from root inode (2)
	bgd, err := bgdl.GetWithAbsoluteInode(ext4.InodeRootDirectory)
	if err != nil {
		return nil, fmt.Errorf("getting root BGD: %w", err)
	}

	dw, err := ext4.NewDirectoryWalk(f, bgd, ext4.InodeRootDirectory)
	if err != nil {
		return nil, fmt.Errorf("creating directory walker: %w", err)
	}

	var files []manifest.FileEntry

	for {
		fullPath, de, err := dw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip entries with errors
		}

		name := de.Name()
		if name == "." || name == ".." || name == "lost+found" {
			continue
		}

		// Clean up the path (remove leading /)
		entryPath := cleanPath(fullPath)
		if entryPath == "" || entryPath == "." {
			continue
		}

		inodeNum := int(de.Data().Inode)
		if inodeNum == 0 {
			continue
		}

		if de.IsDirectory() {
			files = append(files, manifest.FileEntry{
				Path:  entryPath,
				IsDir: true,
				Mode:  uint32(os.ModeDir | 0755),
			})
		} else if de.IsRegular() {
			bgdFile, err := bgdl.GetWithAbsoluteInode(inodeNum)
			if err != nil {
				continue
			}

			inode, err := ext4.NewInodeWithReadSeeker(bgdFile, f, inodeNum)
			if err != nil {
				continue
			}

			fileSize := int64(inode.Size())
			entry := manifest.FileEntry{
				Path: entryPath,
				Size: fileSize,
				Mode: uint32(0644),
			}

			// Set modification time
			entry.ModTime = inode.ModificationTime()

			// Get physical extents from the inode's extent tree
			extents := getExt4Extents(f, inode, blockSize, partOffset, fileSize)
			entry.VolumeExtents = extents

			files = append(files, entry)
		}
		// Skip symlinks, special files, etc.
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// getExt4Extents reads the extent tree from an inode and converts to VolumeExtents.
func getExt4Extents(f *os.File, inode *ext4.Inode, blockSize, partOffset, fileSize int64) []manifest.VolumeExtent {
	data := inode.Data()
	if data == nil {
		return nil
	}

	// The inode's IBlock field contains the extent tree (60 bytes)
	iBlock := data.IBlock
	extents := parseExtentTree(f, iBlock[:], blockSize, partOffset)

	return trimExtentsToSize(extents, fileSize)
}

// trimExtentsToSize caps each extent to the file's logical size using the
// extent's own FileOffset. This is correct even when the extents have gaps
// (sparse files, or holes left by omitted uninitialized extents); the old
// running-total approach let a post-hole extent claim volume bytes past EOF.
func trimExtentsToSize(extents []manifest.VolumeExtent, fileSize int64) []manifest.VolumeExtent {
	var trimmed []manifest.VolumeExtent
	for _, ext := range extents {
		maxLen := fileSize - ext.FileOffset
		if maxLen <= 0 {
			continue // extent starts at or beyond EOF
		}
		if ext.Length > maxLen {
			ext.Length = maxLen
		}
		trimmed = append(trimmed, ext)
	}
	return trimmed
}

// parseExtentTree parses an ext4 extent tree from the iBlock bytes.
func parseExtentTree(f *os.File, iBlock []byte, blockSize, partOffset int64) []manifest.VolumeExtent {
	if len(iBlock) < 12 {
		return nil
	}

	// Parse extent header
	magic := binary.LittleEndian.Uint16(iBlock[0:2])
	if magic != 0xF30A {
		return nil // not an extent tree
	}
	entries := binary.LittleEndian.Uint16(iBlock[2:4])
	depth := binary.LittleEndian.Uint16(iBlock[8:10])

	if depth == 0 {
		// Leaf nodes
		return parseExtentLeaves(iBlock[12:], int(entries), blockSize, partOffset)
	}

	// Internal nodes: follow index entries to child blocks
	var allExtents []manifest.VolumeExtent
	for i := 0; i < int(entries); i++ {
		offset := 12 + i*12
		if offset+12 > len(iBlock) {
			break
		}
		// Parse index node
		physBlockLo := binary.LittleEndian.Uint32(iBlock[offset+4 : offset+8])
		physBlockHi := binary.LittleEndian.Uint16(iBlock[offset+8 : offset+10])
		childBlock := int64(physBlockHi)<<32 | int64(physBlockLo)

		// Read the child block
		childData := make([]byte, blockSize)
		_, err := f.ReadAt(childData, partOffset+childBlock*blockSize)
		if err != nil {
			continue
		}

		childExtents := parseExtentTree(f, childData, blockSize, partOffset)
		allExtents = append(allExtents, childExtents...)
	}
	return allExtents
}

// parseExtentLeaves converts raw extent leaf entries to VolumeExtents.
func parseExtentLeaves(data []byte, count int, blockSize, partOffset int64) []manifest.VolumeExtent {
	var extents []manifest.VolumeExtent
	for i := 0; i < count; i++ {
		offset := i * 12
		if offset+12 > len(data) {
			break
		}
		logicalBlock := binary.LittleEndian.Uint32(data[offset : offset+4])
		blockCount := binary.LittleEndian.Uint16(data[offset+4 : offset+6])
		physBlockHi := binary.LittleEndian.Uint16(data[offset+6 : offset+8])
		physBlockLo := binary.LittleEndian.Uint32(data[offset+8 : offset+12])

		// ee_len > 32768 marks an uninitialized (fallocated) extent whose real
		// length is ee_len - 32768 and whose blocks are preallocated but
		// unwritten. Omit it: the region must read as zeros, which restore
		// produces for any file range not covered by an extent. Emitting a
		// physical extent here would leak stale on-disk bytes, and using the
		// raw block count as the length would claim a huge span that drops the
		// real extents following it.
		if blockCount > 32768 {
			continue
		}

		physBlock := int64(physBlockHi)<<32 | int64(physBlockLo)
		length := int64(blockCount) * blockSize

		extents = append(extents, manifest.VolumeExtent{
			FileOffset:   int64(logicalBlock) * blockSize,
			VolumeOffset: partOffset + physBlock*blockSize,
			Length:       length,
		})
	}
	return extents
}
