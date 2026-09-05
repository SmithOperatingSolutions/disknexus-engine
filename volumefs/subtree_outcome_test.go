// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

//go:build filesystem

package volumefs

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// ExcludeSubtree tells "found and excluded" from "not on this volume" from
// "not NTFS" (#468). AddSubtreeExclusionRanges collapses all three into a
// nil error, which is right for the repo-directory case and wrong for an
// operator's exclusion — that one is a promise, and each of the three is a
// different answer to "did you keep it".
func TestExcludeSubtreeReportsFoundMissingAndForeignFilesystem(t *testing.T) {
	ntfs := testdataPath(t, "ntfs.img")

	m := volume.NewExclusionMap()
	found, err := ExcludeSubtree(ntfs, "dir1", m)
	if err != nil {
		t.Fatal(err)
	}
	if !found.Found || found.Filesystem != "ntfs" || m.Len() == 0 {
		t.Fatalf("dir1 on the NTFS fixture: %+v with %d ranges, want found on ntfs with ranges", found, m.Len())
	}

	missing, err := ExcludeSubtree(ntfs, "no/such/dir", volume.NewExclusionMap())
	if err != nil {
		t.Fatal(err)
	}
	if missing.Found || missing.Filesystem != "ntfs" {
		t.Fatalf("absent subtree: %+v, want not found on ntfs", missing)
	}

	foreign, err := ExcludeSubtree(testdataPath(t, "ext4.img"), "dir1", volume.NewExclusionMap())
	if err != nil {
		t.Fatal(err)
	}
	if foreign.Found || foreign.Filesystem != "ext4" {
		t.Fatalf("ext4 image: %+v, want not found on ext4", foreign)
	}
}
