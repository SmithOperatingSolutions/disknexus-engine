// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Filesystem detection must not leak a handle to the source it probes.
//
// findFirstPartition calls diskfs.Open(f.Name()) — a SECOND, independent open
// of the file the caller already handed it — and nothing closed the returned
// disk. Every capture that probes a source (which is every volume/image
// backup, since the volatile-file exclusion map runs first) leaked one handle
// for the life of the process.
//
// Linux hides it: an unlinked-but-open file just lingers as "(deleted)". The
// windows-latest runner does not, which is where this surfaced — PR #359's
// tests failed with "The process cannot access the file because it is being
// used by another process" while every assertion passed.
func TestDetectFilesystemDoesNotLeakAHandle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts open handles through /proc/self/fd")
	}
	dir := t.TempDir()
	img := filepath.Join(dir, "probe.img")
	// 1 MB of non-filesystem bytes: detection fails, which is the path a
	// plain image takes and the one the leak lived on.
	if err := os.WriteFile(img, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	before := openHandlesTo(t, img)
	f, err := os.Open(img)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = detectFilesystem(f)
	f.Close()
	after := openHandlesTo(t, img)

	if after != before {
		t.Errorf("detectFilesystem left %d open handle(s) to %s after the caller closed its own — "+
			"findFirstPartition opens the path a second time and never closes it; every capture leaks one, "+
			"and Windows then refuses to delete the source", after-before, img)
	}
}

func openHandlesTo(t *testing.T, path string) int {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, fd := range fds {
		tgt, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if err == nil && strings.HasPrefix(tgt, path) {
			n++
		}
	}
	return n
}
