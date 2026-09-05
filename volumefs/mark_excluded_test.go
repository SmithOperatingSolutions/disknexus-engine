// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestMarkExcludedFiles (#94): entries with any extent overlapping the
// exclusion map get IsExcluded; untouched entries and extent-less entries
// (dirs, resident files) do not.
func TestMarkExcludedFiles(t *testing.T) {
	m := volume.NewExclusionMap()
	m.AddRange(10_000, 4096) // covers part of "hit"

	files := []manifest.FileEntry{
		{Path: "hit", VolumeExtents: []manifest.VolumeExtent{{FileOffset: 0, VolumeOffset: 12_000, Length: 4096}}},
		{Path: "miss", VolumeExtents: []manifest.VolumeExtent{{FileOffset: 0, VolumeOffset: 50_000, Length: 4096}}},
		{Path: "dir/", IsDir: true},
		{Path: "resident", InlineData: []byte("tiny")},
	}
	n := MarkExcludedFiles(files, m)
	if n != 1 {
		t.Fatalf("marked %d, want 1", n)
	}
	if !files[0].IsExcluded {
		t.Fatal("overlapping entry not marked")
	}
	for _, i := range []int{1, 2, 3} {
		if files[i].IsExcluded {
			t.Fatalf("%s wrongly marked", files[i].Path)
		}
	}

	// Nil map = no-op.
	if MarkExcludedFiles(files, nil) != 0 {
		t.Fatal("nil map must mark nothing")
	}
}
