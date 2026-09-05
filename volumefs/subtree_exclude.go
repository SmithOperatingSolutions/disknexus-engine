// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"os"

	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	parser "www.velocidex.com/golang/go-ntfs/parser"
)

// AddSubtreeExclusionRanges walks the NTFS directory subtree at relPath
// (volume-root-relative, slash-separated, e.g. "repo" or
// "Users/x/AppData/Local/Temp/disknexus-s3-123") on sourcePath — a volume
// device, VSS snapshot device, or image file — and adds the physical byte
// extents of every file under it to m.
//
// This is how a repo or cloud temp directory stored on the captured volume is
// kept out of its own backup stream. Unlike AddRepoExclusionRanges (which maps
// the LIVE files via FSCTL_GET_RETRIEVAL_POINTERS), the extents here come from
// the filesystem being captured — for a VSS device that is the snapshot's own
// MFT, so they are exactly the clusters the point-in-time image references,
// even if the live files moved since the snapshot.
//
// A missing subtree and non-NTFS sources are clean no-ops: the directory
// simply is not on this volume. Callers that must tell those apart — an
// operator-configured exclusion is a promise, and "not found" is the
// opposite outcome of "excluded" (#468) — use ExcludeSubtree.
func AddSubtreeExclusionRanges(sourcePath, relPath string, m *volume.ExclusionMap) error {
	_, err := ExcludeSubtree(sourcePath, relPath, m)
	return err
}

// SubtreeExclusion is what ExcludeSubtree found: the filesystem it looked at
// and whether the subtree exists on it. Found is false on a non-NTFS source
// too — the walk is NTFS-only — and Filesystem says which case it was.
type SubtreeExclusion struct {
	Filesystem string // "ntfs", "ext4", "fat32", ... as detectFilesystem names it
	Found      bool   // the subtree exists and its extents were added
}

// ExcludeSubtree is AddSubtreeExclusionRanges with the outcome reported.
func ExcludeSubtree(sourcePath, relPath string, m *volume.ExclusionMap) (SubtreeExclusion, error) {
	var out SubtreeExclusion
	f, err := os.Open(toRawVolumePath(sourcePath))
	if err != nil {
		return out, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()

	fsType, partOffset, err := detectFilesystem(f)
	if err != nil {
		return out, fmt.Errorf("detecting filesystem: %w", err)
	}
	out.Filesystem = fsType
	if fsType != "ntfs" {
		return out, nil
	}

	reader, err := parser.NewPagedReader(f, 1024, 10000)
	if err != nil {
		return out, fmt.Errorf("creating paged reader: %w", err)
	}
	ntfs, err := parser.GetNTFSContext(reader, partOffset)
	if err != nil {
		return out, fmt.Errorf("parsing NTFS: %w", err)
	}
	defer ntfs.Close()

	root, err := ntfs.GetMFT(5)
	if err != nil {
		return out, fmt.Errorf("getting root MFT entry: %w", err)
	}
	entry, err := root.Open(ntfs, strings.ReplaceAll(relPath, `\`, "/"))
	if err != nil {
		return out, nil // subtree not present on this volume
	}
	out.Found = true

	excludeEntryTree(ntfs, entry, partOffset, m, make(map[int64]struct{}))
	return out, nil
}

// excludeEntryTree excludes every file's $DATA extents under entry,
// recursively. Mirrors walkNTFSDir2's cycle guard so junction loops cannot
// recurse forever. Directory entries themselves carry no $DATA to exclude;
// files that fail to resolve are simply not excluded (best-effort, same
// contract as AddRepoExclusionRanges).
func excludeEntryTree(ntfs *parser.NTFSContext, entry *parser.MFT_ENTRY, partOffset int64, m *volume.ExclusionMap, visited map[int64]struct{}) {
	infos := parser.ListDir(ntfs, entry)
	if len(infos) == 0 {
		// Leaf: a plain file opened directly as the subtree root.
		addStreamRanges(ntfs, entry, "", partOffset, m)
		return
	}
	for _, info := range infos {
		if strings.HasPrefix(info.Name, "$") {
			continue
		}
		// MFTId is "entry-attr-id" (e.g. "65-144-2"); the entry number leads.
		var mftNum int64
		if _, err := fmt.Sscanf(info.MFTId, "%d", &mftNum); err != nil {
			continue
		}
		if _, seen := visited[mftNum]; seen {
			continue
		}
		visited[mftNum] = struct{}{}
		child, err := ntfs.GetMFT(mftNum)
		if err != nil {
			continue
		}
		if info.IsDir {
			excludeEntryTree(ntfs, child, partOffset, m, visited)
		} else {
			addStreamRanges(ntfs, child, "", partOffset, m)
		}
	}
}
