//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"os"
	"testing"
)

// TestVolatileExclusionUsesPhysicalOffsets proves the volatile-exclusion map
// excludes the physical device clusters of volatile streams (e.g. $LogFile),
// not go-ntfs's file-logical offsets. With file-logical offsets the map
// starts at 0 and would zero the boot sector and $MFT (corrupting the
// backup) instead of the volatile file's real clusters.
func TestVolatileExclusionUsesPhysicalOffsets(t *testing.T) {
	imgPath := testdataPath(t, "ntfs.img")
	fi, err := os.Stat(imgPath)
	if err != nil {
		t.Fatal(err)
	}

	m, err := BuildVolatileExclusionMap(imgPath, fi.Size())
	if err != nil {
		t.Fatalf("BuildVolatileExclusionMap: %v", err)
	}
	if m.Len() == 0 {
		// FIXTURE DRIFT IS A FAILURE (#402): a BuildVolatileExclusionMap
		// that stops excluding anything made this test SKIP — the exact
		// state in which the boot-sector assertion below is needed most.
		t.Fatal("the exclusion map is empty — the fixture lost its volatile streams or the builder stopped excluding; either way the boot-sector guard below would be vacuous, and a vacuous skip is a deleted test")
	}

	// The boot sector (offset 0) must never be excluded.
	if m.IsExcluded(0, 512) {
		t.Fatal("volatile exclusion map excludes the boot sector at offset 0 (file-logical offsets used instead of physical)")
	}
}
