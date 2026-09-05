// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "testing"

// CoveredBytes is what "excluding C:\x (12.3 MB)" reports to an operator
// (#468). Three passes can add the same clusters (volatile, subtree, live
// extents); a number that counted them twice would tell the operator their
// exclusion is bigger than the file.
func TestCoveredBytesCountsOverlapsOnce(t *testing.T) {
	m := NewExclusionMap()
	m.AddRange(0, 4096)
	m.AddRange(2048, 4096)  // overlaps the first by 2048
	m.AddRange(6144, 1024)  // adjacent to the merged [0,6144)
	m.AddRange(10240, 4096) // separate
	if got := m.CoveredBytes(); got != 7168+4096 {
		t.Fatalf("CoveredBytes = %d, want %d — overlapping passes must be counted once", got, 7168+4096)
	}
	// Positive control: the same ranges without overlap sum plainly.
	n := NewExclusionMap()
	n.AddRange(0, 100)
	n.AddRange(1000, 100)
	if got := n.CoveredBytes(); got != 200 {
		t.Fatalf("disjoint CoveredBytes = %d, want 200", got)
	}
	if NewExclusionMap().CoveredBytes() != 0 {
		t.Fatal("an empty map covers bytes")
	}
}
