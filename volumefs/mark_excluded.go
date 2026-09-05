// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// MarkExcludedFiles sets IsExcluded on every catalog entry with any physical
// extent overlapping the capture exclusion map, and returns how many were
// marked. Those files' clusters were deliberately zeroed in the block stream
// (volatile files, repo/temp dirs), so restore-files must refuse them rather
// than silently produce zero-filled content (#94). Any overlap marks the
// whole file: a partially-clipped file is just as unrestorable.
func MarkExcludedFiles(files []manifest.FileEntry, m *volume.ExclusionMap) int {
	if m == nil || m.Len() == 0 {
		return 0
	}
	marked := 0
	for i := range files {
		for _, ext := range files[i].VolumeExtents {
			if ext.Length > 0 && m.IsExcluded(ext.VolumeOffset, ext.Length) {
				files[i].IsExcluded = true
				marked++
				break
			}
		}
	}
	return marked
}
