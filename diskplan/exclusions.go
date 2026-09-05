// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

// buildCaptureExclusions builds the exclusion map for one volume capture:
// volatile NTFS files (pagefile.sys & co, plus $LogFile/$UsnJrnl), and the
// physical ranges of any localPaths (the local repo directory, cloud temp
// dirs) that live on the captured volume — so a backup never captures its own
// backend state ("don't back up the backup").
//
// scanSource must be the filesystem the stream is read from: the VSS snapshot
// device when one exists (extents then match the point-in-time image — see the
// $LogFile reallocation note at the volume-backup call site), otherwise the
// input image / live volume.
//
// localPaths on a different volume than volumeLetter are ignored: their
// extents are offsets on a different device, and excluding them would zero
// unrelated ranges of the captured stream.
//
// Best-effort by design: on error a warning is printed and the capture
// proceeds unexcluded. Returns nil when disabled or empty.
func BuildCaptureExclusions(cfg config.Config, scanSource, volumeLetter string, totalSize int64, localPaths ...string) *volume.ExclusionMap {
	if !cfg.ExcludeVolatileFiles {
		return nil
	}
	m, err := volumefs.BuildVolatileExclusionMap(scanSource, totalSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: volatile file exclusion failed: %v\n", err)
		return nil
	}
	for _, lp := range localPaths {
		if lp == "" {
			continue
		}
		rel, ok := volumeRelative(lp, volumeLetter)
		if !ok {
			continue
		}
		// Subtree walk of the capture source itself: excludes exactly the
		// clusters the captured image references for these files.
		if err := volumefs.AddSubtreeExclusionRanges(scanSource, rel, m); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: exclusion of %s failed: %v\n", lp, err)
		}
		// Live-extent pass (FSCTL, best-effort): also covers blocks the
		// directory gained after the snapshot. On the snapshot those clusters
		// were free, so zeroing them only drops stale deleted-data garbage
		// from the image.
		volumefs.AddRepoExclusionRanges(lp, m)
	}
	if m.Len() == 0 {
		return nil
	}
	return m
}

// StaleCloudTempDirs returns leftover disknexus-s3-* work dirs from crashed
// cloud runs. A live run's temp dir is created after the snapshot, so only
// stale ones can hold pre-snapshot bytes (downloaded index, sidecars, packs)
// that would otherwise be captured into the image.
//
// bases are the scratch bases to scan — exactly those, and nothing else.
// The engine does not know where the PRODUCT stages its work (the ambient
// temp, a DISKNEXUS_TEMP override, <stateDir>/tmp since #315): the caller
// does, and passes every base scratch can go to, or the #297 exclusion
// silently stops matching the agent's own crashed runs. The engine reads no
// environment (internal/arch pins that).
func StaleCloudTempDirs(bases ...string) []string {
	seen := map[string]bool{}
	var matches []string
	for _, b := range bases {
		if b == "" || seen[b] {
			continue
		}
		seen[b] = true
		m, _ := filepath.Glob(filepath.Join(b, "disknexus-s3-*"))
		matches = append(matches, m...)
	}
	return matches
}
