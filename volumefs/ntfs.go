// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	parser "www.velocidex.com/golang/go-ntfs/parser"
)

// scanNTFS enumerates all files on an NTFS filesystem and builds
// VolumeExtents from their physical disk locations (disk-absolute when the
// filesystem sits at a partition offset inside an image).
func scanNTFS(f *os.File, partOffset int64) ([]manifest.FileEntry, error) {
	return scanNTFSAt(f, partOffset, partOffset)
}

// scanNTFSAt is the ReaderAt form (#151): scan an NTFS filesystem at
// fsOffset inside r, emitting extents relative to extentBase (0 for
// member catalogs, where the backup stream IS the partition; fsOffset for
// whole-image scans, where extents must be disk-absolute).
//
// go-ntfs's GetNTFSContext offset parameter only reaches the boot sector —
// MFT reads ignore it (latent library limitation) — so the offset is
// applied with a SectionReader and the context always starts at 0.
func scanNTFSAt(f io.ReaderAt, fsOffset, extentBase int64) ([]manifest.FileEntry, error) {
	sec := io.NewSectionReader(f, fsOffset, int64(1)<<62-fsOffset)
	reader, err := parser.NewPagedReader(sec, 1024, 10000)
	if err != nil {
		return nil, fmt.Errorf("creating paged reader: %w", err)
	}

	ntfs, err := parser.GetNTFSContext(reader, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing NTFS: %w", err)
	}
	defer ntfs.Close()

	root, err := ntfs.GetMFT(5)
	if err != nil {
		return nil, fmt.Errorf("getting root MFT entry: %w", err)
	}

	var files []manifest.FileEntry
	err = walkNTFSDir(ntfs, root, "", &files, extentBase)
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Path < files[j].Path
	})
	return files, nil
}

// walkNTFSDir recursively walks an NTFS directory tree.
// visited tracks MFT entry numbers already processed to detect junction cycles.
func walkNTFSDir(ntfs *parser.NTFSContext, dir *parser.MFT_ENTRY, dirPath string, files *[]manifest.FileEntry, partOffset int64) error {
	return walkNTFSDir2(ntfs, dir, dirPath, files, partOffset, make(map[int64]struct{}))
}

func walkNTFSDir2(ntfs *parser.NTFSContext, dir *parser.MFT_ENTRY, dirPath string, files *[]manifest.FileEntry, partOffset int64, visited map[int64]struct{}) error {
	infos := parser.ListDir(ntfs, dir)

	for _, info := range infos {
		name := info.Name
		// Skip NTFS metadata files (starting with $)
		if strings.HasPrefix(name, "$") {
			continue
		}

		filePath := name
		if dirPath != "" {
			filePath = dirPath + "/" + name
		}

		if info.IsDir {
			*files = append(*files, manifest.FileEntry{
				Path:    filePath,
				IsDir:   true,
				Mode:    uint32(os.ModeDir | 0755),
				ModTime: info.Mtime,
			})

			// Recurse into subdirectory
			mftID := info.MFTId
			if mftID == "" {
				continue
			}
			var mftNum int64
			if _, err := fmt.Sscanf(mftID, "%d", &mftNum); err != nil {
				continue
			}
			// Skip already-visited MFT entries to break junction/symlink cycles.
			if _, seen := visited[mftNum]; seen {
				continue
			}
			visited[mftNum] = struct{}{}
			child, err := ntfs.GetMFT(mftNum)
			if err != nil {
				continue // skip inaccessible dirs
			}
			if err := walkNTFSDir2(ntfs, child, filePath, files, partOffset, visited); err != nil {
				continue
			}
		} else {
			var extents []manifest.VolumeExtent
			var inline []byte
			if mftID := info.MFTId; mftID != "" {
				var mftNum int64
				if _, err := fmt.Sscanf(mftID, "%d", &mftNum); err == nil {
					if mftEntry, err := ntfs.GetMFT(mftNum); err == nil {
						extents = getNTFSExtents(ntfs, mftEntry, partOffset, info.Size)
						if len(extents) == 0 {
							// Resident file: data lives inside the MFT record
							// (no cluster runs). Inline it — the block stream
							// covers the MFT but per-file restore needs the
							// bytes addressable (restoreInlineFile). Capped:
							// resident data cannot exceed an MFT record.
							inline = readResidentData(ntfs, mftEntry, info.Size)
						}
					}
				}
			}
			*files = append(*files, manifest.FileEntry{
				Path:          filePath,
				Size:          info.Size,
				Mode:          uint32(0644),
				ModTime:       info.Mtime,
				VolumeExtents: extents,
				InlineData:    inline,
			})
		}
	}
	return nil
}

// readResidentData reads a resident stream's content out of the MFT record
// (bounded — resident data cannot exceed an MFT record, but cap defensively).
func readResidentData(ntfs *parser.NTFSContext, entry *parser.MFT_ENTRY, size int64) []byte {
	const residentCap = 4096
	if size <= 0 || size > residentCap {
		return nil
	}
	stream, err := parser.OpenStream(ntfs, entry, 128, 0xffff, "")
	if err != nil {
		return nil
	}
	buf := make([]byte, size)
	nread, err := stream.ReadAt(buf, 0)
	if int64(nread) != size || (err != nil && err != io.EOF) {
		return nil
	}
	return buf
}

// getNTFSExtents extracts the physical byte extents for a file's $DATA
// attribute, capped to the file's logical size.
func getNTFSExtents(ntfs *parser.NTFSContext, entry *parser.MFT_ENTRY, partOffset, fileSize int64) []manifest.VolumeExtent {
	return physicalExtents(ntfs, entry, "", partOffset, fileSize)
}

// physicalExtents maps a file data stream's runs to absolute device byte
// extents.
//
// go-ntfs's Range.Offset (from Ranges()) is in the file's *logical* address
// space (FileOffset*ClusterSize), NOT a physical disk offset — storing it as
// VolumeOffset makes every extent point at the wrong place on the volume
// (the first run resolves to offset 0, the boot sector). The physical
// location lives in the leaf runs' TargetOffset. We walk DebugRuns to reach
// those leaves and compute VolumeOffset = partOffset + TargetOffset*ClusterSize.
//
// Resident streams (data stored inside the MFT record) and compressed streams
// (on-disk bytes are not the raw file data) return nil — they cannot be
// represented as raw device extents. fileSize caps the total coverage to the
// file's logical size (pass a large value to include full trailing clusters,
// e.g. for volatile-file exclusion).
func physicalExtents(ntfs *parser.NTFSContext, entry *parser.MFT_ENTRY, streamName string, partOffset, fileSize int64) []manifest.VolumeExtent {
	stream, err := parser.OpenStream(ntfs, entry, 128, 0xffff, streamName)
	if err != nil {
		return nil
	}

	// Non-resident streams are *RangeReader; resident data (a *MappedReader
	// over a bytes.Reader) has no on-disk extents.
	rr, ok := stream.(*parser.RangeReader)
	if !ok {
		return nil
	}

	runs := parser.DebugRuns(rr, 0)

	// A compressed stream's on-disk bytes are LZNT1-compressed, not the raw
	// file content, so it cannot be backed up as raw device extents.
	for _, ri := range runs {
		if ri.CompressedLength > 0 {
			return nil
		}
	}

	var extents []manifest.VolumeExtent
	var fileOffset int64
	for i, ri := range runs {
		// Only leaf runs map directly to the device. DebugRuns emits each run
		// followed by its children (deeper level), so a run is a leaf when the
		// next run is not deeper.
		isLeaf := i+1 >= len(runs) || runs[i+1].Level <= ri.Level
		if !isLeaf || ri.ClusterSize <= 0 || ri.Length <= 0 {
			continue
		}
		runBytes := ri.Length * ri.ClusterSize
		if ri.IsSparse {
			fileOffset += runBytes
			continue
		}
		length := runBytes
		if remaining := fileSize - fileOffset; remaining <= 0 {
			break
		} else if length > remaining {
			length = remaining
		}
		extents = append(extents, manifest.VolumeExtent{
			FileOffset:   fileOffset,
			VolumeOffset: partOffset + ri.ToOffset*ri.ClusterSize,
			Length:       length,
		})
		fileOffset += runBytes
	}

	return extents
}

// ntfsModTime returns a zero time if the time is invalid.
func ntfsModTime(t time.Time) time.Time {
	if t.IsZero() || t.Year() < 1980 {
		return time.Time{}
	}
	return t
}

// cleanPath normalizes a path to forward-slash separated, no leading slash.
func cleanPath(p string) string {
	p = path.Clean(p)
	p = strings.TrimPrefix(p, "/")
	return p
}
