// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"sort"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	diskfs "github.com/diskfs/go-diskfs"
)

// ScanFAT32Partition catalogs the FAT32 filesystem in one partition of a
// partitioned disk (device or image) at sourcePath: partition is the
// 1-based ordinal among the disk's table entries, partOffset the
// partition's byte offset (extents are reported relative to the disk). It
// is how a boot-structure check reads an EFI System Partition (#223).
func ScanFAT32Partition(sourcePath string, partition int, partOffset int64) ([]manifest.FileEntry, error) {
	if partition < 1 {
		return nil, fmt.Errorf("partition ordinal %d: partitions are numbered from 1", partition)
	}
	disk, err := diskfs.Open(sourcePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("opening disk: %w", err)
	}
	defer disk.Close()
	fs, err := disk.GetFilesystem(partition)
	if err != nil {
		return nil, fmt.Errorf("getting partition %d's filesystem: %w", partition, err)
	}
	var files []manifest.FileEntry
	if err := walkFAT32Dir(fs, "/", "", &files, partOffset); err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
