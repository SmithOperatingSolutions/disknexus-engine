// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/dsoprea/go-exfat"
)

// scanExFAT enumerates all files on an exFAT filesystem and builds
// VolumeExtents from their physical disk locations.
func scanExFAT(f *os.File, partOffset int64) ([]manifest.FileEntry, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	// Create a ReadSeeker starting at the partition offset.
	var rs io.ReadSeeker
	if partOffset > 0 {
		rs = io.NewSectionReader(f, partOffset, info.Size()-partOffset)
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek: %w", err)
		}
		rs = f
	}

	er := exfat.NewExfatReader(rs)
	if err := er.Parse(); err != nil {
		return nil, fmt.Errorf("parsing exfat: %w", err)
	}

	bsh := er.ActiveBootSectorHeader()
	sectorSize := uint64(bsh.SectorSize())
	sectorsPerCluster := uint64(bsh.SectorsPerCluster())
	clusterHeapOffset := uint64(bsh.ClusterHeapOffset) // in sectors
	clusterBytes := int64(sectorsPerCluster * sectorSize)

	// clusterToOffset converts a cluster number to an absolute byte offset
	// within the backed-up stream.
	clusterToOffset := func(c uint32) int64 {
		return partOffset + int64((clusterHeapOffset+uint64(c-2)*sectorsPerCluster)*sectorSize)
	}

	tree := exfat.NewTree(er)
	if err := tree.Load(); err != nil {
		return nil, fmt.Errorf("loading tree: %w", err)
	}

	var files []manifest.FileEntry

	err = tree.Visit(func(pathParts []string, node *exfat.TreeNode) error {
		// Skip root node (empty pathParts)
		if len(pathParts) == 0 {
			return nil
		}

		path := strings.Join(pathParts, "/")
		fde := node.FileDirectoryEntry()

		if node.IsDirectory() {
			entry := manifest.FileEntry{
				Path:  path,
				IsDir: true,
				Mode:  uint32(os.ModeDir | 0755),
			}
			if fde != nil {
				entry.ModTime = fde.LastModifiedTimestamp()
			}
			files = append(files, entry)
			return nil
		}

		// Regular file
		sede := node.StreamDirectoryEntry()
		if sede == nil {
			return nil
		}

		entry := manifest.FileEntry{
			Path: path,
			Size: int64(sede.ValidDataLength),
			Mode: uint32(0644),
		}
		if fde != nil {
			entry.ModTime = fde.LastModifiedTimestamp()
		}

		if sede.FirstCluster >= 2 && sede.ValidDataLength > 0 {
			entry.VolumeExtents = getExFATExtents(er, sede, clusterToOffset, clusterBytes)
		}

		files = append(files, entry)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking tree: %w", err)
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// getExFATExtents computes VolumeExtents for a file from its cluster chain.
func getExFATExtents(er *exfat.ExfatReader, sede *exfat.ExfatStreamExtensionDirectoryEntry, clusterToOffset func(uint32) int64, clusterBytes int64) []manifest.VolumeExtent {
	fileSize := int64(sede.ValidDataLength)
	useFat := !sede.GeneralSecondaryFlags.NoFatChain()

	if !useFat {
		// Sequential allocation — single contiguous extent
		return []manifest.VolumeExtent{{
			FileOffset:   0,
			VolumeOffset: clusterToOffset(sede.FirstCluster),
			Length:       fileSize,
		}}
	}

	// FAT chain — enumerate clusters and merge contiguous runs
	var clusters []uint32
	_ = er.EnumerateClusters(sede.FirstCluster, func(ec *exfat.ExfatCluster) (bool, error) {
		clusters = append(clusters, ec.ClusterNumber())
		// Stop when we have enough clusters to cover the file
		if int64(len(clusters))*clusterBytes >= fileSize {
			return false, nil
		}
		return true, nil
	}, true)

	if len(clusters) == 0 {
		return nil
	}

	// Merge consecutive clusters into extents
	var extents []manifest.VolumeExtent
	var fileOffset int64
	remaining := fileSize

	i := 0
	for i < len(clusters) && remaining > 0 {
		startCluster := clusters[i]
		runLen := 1
		for i+runLen < len(clusters) && clusters[i+runLen] == startCluster+uint32(runLen) {
			runLen++
		}

		extentLen := int64(runLen) * clusterBytes
		if extentLen > remaining {
			extentLen = remaining
		}

		extents = append(extents, manifest.VolumeExtent{
			FileOffset:   fileOffset,
			VolumeOffset: clusterToOffset(startCluster),
			Length:       extentLen,
		})

		fileOffset += extentLen
		remaining -= extentLen
		i += runLen
	}

	return extents
}
