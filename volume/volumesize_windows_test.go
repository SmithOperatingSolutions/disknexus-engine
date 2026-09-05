//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"runtime"
	"testing"
)

// systemDrive returns the drive letter to probe (e.g. "C:"), from %SystemDrive%.
func systemDrive() string {
	d := os.Getenv("SystemDrive")
	if d == "" {
		d = "C:"
	}
	return d
}

// TestVolumeSizeReturnsPositive is a functional sanity check: the system volume
// must report a positive byte size.
func TestVolumeSizeReturnsPositive(t *testing.T) {
	sz, err := VolumeSize(systemDrive())
	if err != nil {
		t.Fatalf("VolumeSize(%s): %v", systemDrive(), err)
	}
	if sz <= 0 {
		t.Fatalf("VolumeSize(%s) = %d, want > 0", systemDrive(), sz)
	}
}

// TestVolumeSizeDoesNotDoubleCloseHandle guards issue #28: VolumeSize used to
// wrap its volume handle in an os.File *and* CloseHandle it, so the os.File
// finalizer closed the handle a second time. A recycled handle value later used
// by the runtime for an OS thread would then be invalidated, crashing with
// "runtime.preemptM: duplicatehandle failed".
//
// The double-close is a Windows-syscall-level, timing-dependent corruption that
// can't be asserted deterministically. This repeatedly calls VolumeSize and
// forces GC (running any os.File finalizers) between calls to exercise the exact
// path that used to double-close, asserting the results stay correct and the
// process does not crash. With the fix (single CloseHandle, no os.File wrapper)
// it is stable.
func TestVolumeSizeDoesNotDoubleCloseHandle(t *testing.T) {
	drive := systemDrive()
	first, err := VolumeSize(drive)
	if err != nil {
		t.Fatalf("VolumeSize(%s): %v", drive, err)
	}
	for i := 0; i < 500; i++ {
		sz, err := VolumeSize(drive)
		if err != nil {
			t.Fatalf("VolumeSize(%s) iter %d: %v", drive, i, err)
		}
		if sz != first {
			t.Fatalf("VolumeSize(%s) iter %d = %d, want stable %d", drive, i, sz, first)
		}
		// Run finalizers: under the old code this is where the wrapped
		// os.File's finalizer performed the second CloseHandle.
		runtime.GC()
	}
}
