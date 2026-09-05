// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package vss

import (
	"runtime"
	"strings"
	"testing"
)

// TestReleaseNilSafe verifies Release is safe on an un-created / nil snapshot
// (the code paths that never obtained a shadow copy set).
func TestReleaseNilSafe(t *testing.T) {
	var s *Snapshot
	if err := s.Release(); err != nil {
		t.Errorf("nil snapshot Release: %v", err)
	}
	if err := (&Snapshot{}).Release(); err != nil {
		t.Errorf("empty snapshot Release: %v", err)
	}
}

// TestCreateSnapshotNonWindows verifies the non-Windows guidance message so
// users on Linux/macOS are pointed at --input rather than a raw VSS error.
func TestCreateSnapshotNonWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("VSS creation requires an elevated Windows session; exercised in CI")
	}
	_, err := CreateSnapshot("C:")
	if err == nil {
		t.Fatal("expected an error creating a VSS snapshot off Windows")
	}
	if !strings.Contains(err.Error(), "require Windows") {
		t.Errorf("error should mention the Windows requirement, got: %v", err)
	}
}
