//go:build filesystem

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// testdataPath returns the absolute path to a file in engine/volumefs/testdata/.
// It skips the test if the file does not exist (images not yet generated).
func testdataPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join("testdata", name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		t.Skipf("testdata file not found: %s (run scripts/create-testdata.sh first)", p)
	}
	return p
}

// findFile returns a pointer to the first FileEntry whose Path matches,
// or nil if not found. Leading "./" is stripped before comparing so that
// NTFS paths (which may be prefixed with "./") match the same way as
// ext4/exFAT/FAT32 paths.
func findFile(files []manifest.FileEntry, path string) *manifest.FileEntry {
	for i := range files {
		if strings.TrimPrefix(files[i].Path, "./") == path {
			return &files[i]
		}
	}
	return nil
}
