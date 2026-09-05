// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport

import (
	"errors"
	"testing"
)

// failingWriter fails every Write, simulating an underlying sink (disk, pipe)
// that errors — e.g. ENOSPC.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}

// TestZipDirectoryToSurfacesCentralDirectoryError proves that a write failure
// when zip.Writer.Close flushes the central directory is surfaced, not
// swallowed. The source dir is empty, so the only write is the central
// directory during Close — exactly the path the old bare `defer w.Close()`
// discarded, letting Export return nil with a truncated, unreadable archive.
func TestZipDirectoryToSurfacesCentralDirectoryError(t *testing.T) {
	srcDir := t.TempDir() // empty: no file data written, only the central directory

	err := zipDirectoryTo(failingWriter{}, srcDir)
	if err == nil {
		t.Fatal("zipDirectoryTo returned nil despite the underlying writer failing on the central-directory flush")
	}
}
