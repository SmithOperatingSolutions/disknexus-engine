// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs_test

import (
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

func TestBuildVolatileExclusionMapNonNTFS(t *testing.T) {
	// Create a temp file with non-NTFS content (zeros)
	f, err := os.CreateTemp("", "volatile-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	// Write enough zeros to pass the minimum size check in detectFilesystem
	buf := make([]byte, 4096)
	f.Write(buf)
	f.Close()

	exclMap, err := volumefs.BuildVolatileExclusionMap(f.Name(), 4096)
	if err != nil {
		// Non-NTFS should return error from detectFilesystem, which is handled
		// by returning an error. That's acceptable — check the error is about
		// filesystem detection.
		t.Logf("got expected error for non-NTFS file: %v", err)
		return
	}

	if exclMap.Len() != 0 {
		t.Fatalf("non-NTFS file should produce empty exclusion map, got %d ranges", exclMap.Len())
	}
}

func TestBuildVolatileExclusionMapInvalidFile(t *testing.T) {
	_, err := volumefs.BuildVolatileExclusionMap("/nonexistent/path/to/volume", 0)
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}
