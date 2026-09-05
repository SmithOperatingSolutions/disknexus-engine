// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// ScanResult holds the file catalog from a volume filesystem scan.
type ScanResult struct {
	Filesystem string               // "ntfs", "fat32", "exfat", "ext4"
	Files      []manifest.FileEntry // with VolumeExtents populated
}

// ScanVolume detects the filesystem and enumerates all files with their
// physical byte offsets. Opens the source path with its own file handle.
// On Windows, live volume paths (e.g., "\\.\C:") are scanned via the OS
// filesystem APIs to avoid go-ntfs internal caching overhead.
// onProgress, if non-nil, is called periodically with (scanned, total) file counts.
// shadowRoot, if non-empty, is a VSS shadow device path (e.g.,
// "\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1") used by nativeScan
// workers to open files via the snapshot instead of the live volume.
func ScanVolume(ctx context.Context, sourcePath string, volumeSize int64, onProgress func(scanned, total int), shadowRoot string) (*ScanResult, error) {
	if nativeScanAvailable(sourcePath) {
		files, err := nativeScan(ctx, sourcePath, onProgress, shadowRoot)
		if err != nil {
			return nil, fmt.Errorf("scanning volume: %w", err)
		}
		return &ScanResult{Filesystem: "ntfs", Files: files}, nil
	}

	f, err := os.Open(toRawVolumePath(sourcePath))
	if err != nil {
		return nil, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()

	fsType, partOffset, err := detectFilesystem(f)
	if err != nil {
		return nil, fmt.Errorf("detecting filesystem: %w", err)
	}

	var files []manifest.FileEntry
	switch fsType {
	case "ntfs":
		files, err = scanNTFS(f, partOffset)
	case "fat32":
		files, err = scanFAT32(sourcePath, partOffset)
	case "exfat":
		files, err = scanExFAT(f, partOffset)
	case "ext4":
		files, err = scanExt4(f, partOffset)
	default:
		return nil, fmt.Errorf("unsupported filesystem: %s", fsType)
	}
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", fsType, err)
	}

	return &ScanResult{
		Filesystem: fsType,
		Files:      files,
	}, nil
}

// readerAtFromFile wraps an *os.File as io.ReaderAt (it already implements it).
func readerAtFromFile(f *os.File) io.ReaderAt {
	return f
}

// ScanPartition (#151) scans a filesystem embedded at a byte offset inside
// a disk (machine-snapshot member catalogs) — GPT/MBR agnostic: the caller
// supplies the partition's offset/length from the parsed layout.
func ScanPartition(ctx context.Context, r io.ReaderAt, offset, length int64) (*ScanResult, error) {
	fsType, err := detectFSAt(r, offset)
	if err != nil {
		return nil, fmt.Errorf("detecting member filesystem: %w", err)
	}
	switch fsType {
	case "ntfs":
		files, err := scanNTFSAt(r, offset, 0)
		if err != nil {
			return nil, fmt.Errorf("scanning member ntfs: %w", err)
		}
		return &ScanResult{Filesystem: "ntfs", Files: files}, nil
	default:
		return nil, fmt.Errorf("unsupported member filesystem for cataloging: %s", fsType)
	}
}
