// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// TestRestoreFileShortEntriesFailsLoud guards issue #16: restoreFile never
// checked that it wrote StreamLength bytes, so a file whose covering entries are
// empty or gapped restored "successfully" as a short/empty file (silent data
// loss). Here the entry accessor is empty while the catalog entry claims 1000
// bytes; restoreFile must return an error rather than a 0-byte file.
func TestRestoreFileShortEntriesFailsLoud(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := NewFileRestorer(nil, nil, "", logger)

	empty := manifest.NewSliceEntryAccessor(nil)
	target := filepath.Join(t.TempDir(), "out.dat")
	f := manifest.FileEntry{Path: "out.dat", Size: 1000, StreamOffset: 0, StreamLength: 1000}

	written, err := r.restoreFile(f, target, empty, nil)
	if err == nil {
		t.Fatalf("restoreFile returned success (wrote %d of %d) for a file with no covering entries; should fail loud", written, f.StreamLength)
	}
}
