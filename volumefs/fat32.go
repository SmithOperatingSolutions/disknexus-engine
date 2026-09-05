// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/fat32"
)

// scanFAT32 enumerates all files on a FAT32 filesystem and builds
// VolumeExtents from their physical disk locations.
// sourcePath is used to open the disk via go-diskfs.
func scanFAT32(sourcePath string, partOffset int64) ([]manifest.FileEntry, error) {
	disk, err := diskfs.Open(sourcePath, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("opening disk: %w", err)
	}
	defer disk.Close()

	var fs filesystem.FileSystem
	if partOffset > 0 {
		// Disk with partition table: get the first partition's filesystem
		fs, err = disk.GetFilesystem(1)
	} else {
		// Raw filesystem (no partition table)
		fs, err = disk.GetFilesystem(0)
	}
	if err != nil {
		return nil, fmt.Errorf("getting filesystem: %w", err)
	}

	var files []manifest.FileEntry
	err = walkFAT32Dir(fs, "/", "", &files, partOffset)
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// walkFAT32Dir recursively walks a FAT32 directory tree.
func walkFAT32Dir(fs filesystem.FileSystem, fsPath, catalogPath string, files *[]manifest.FileEntry, partOffset int64) error {
	entries, err := fs.ReadDir(fsPath)
	if err != nil {
		return fmt.Errorf("reading dir %s: %w", fsPath, err)
	}

	for _, info := range entries {
		name := info.Name()
		// Skip hidden/system entries
		if name == "." || name == ".." {
			continue
		}

		entryFSPath := fsPath + "/" + name
		if fsPath == "/" {
			entryFSPath = "/" + name
		}

		entryPath := name
		if catalogPath != "" {
			entryPath = catalogPath + "/" + name
		}

		if info.IsDir() {
			*files = append(*files, manifest.FileEntry{
				Path:    entryPath,
				IsDir:   true,
				Mode:    uint32(os.ModeDir | 0755),
				ModTime: info.ModTime(),
			})
			if err := walkFAT32Dir(fs, entryFSPath, entryPath, files, partOffset); err != nil {
				continue // skip inaccessible dirs
			}
		} else {
			entry := manifest.FileEntry{
				Path:    entryPath,
				Size:    info.Size(),
				Mode:    uint32(0644),
				ModTime: info.ModTime(),
			}

			// Get physical extents
			extents := getFAT32Extents(fs, entryFSPath, partOffset)
			entry.VolumeExtents = extents

			*files = append(*files, entry)
		}
	}
	return nil
}

// getFAT32Extents opens a file and retrieves its physical disk ranges.
func getFAT32Extents(fs filesystem.FileSystem, fsPath string, partOffset int64) []manifest.VolumeExtent {
	file, err := fs.OpenFile(fsPath, os.O_RDONLY)
	if err != nil {
		return nil
	}

	fatFile, ok := file.(*fat32.File)
	if !ok {
		return nil
	}

	ranges, err := fatFile.GetDiskRanges()
	if err != nil {
		return nil
	}

	var extents []manifest.VolumeExtent
	var fileOffset int64
	for _, r := range ranges {
		extents = append(extents, manifest.VolumeExtent{
			FileOffset:   fileOffset,
			VolumeOffset: partOffset + int64(r.Offset),
			Length:       int64(r.Length),
		})
		fileOffset += int64(r.Length)
	}
	return extents
}

// trimFAT32Name removes trailing spaces from FAT32 filenames.
func trimFAT32Name(name string) string {
	return strings.TrimRight(name, " ")
}
