// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// TestExtractVolumeFileTrailingSparseHole proves that restoreVolumeFile
// never extends the output to the logical file size: it truncates only
// downward (cluster slack). The NTFS scanner skips sparse runs without
// emitting extents (volumefs/ntfs.go: `if r.IsSparse { ... continue }`),
// so a sparse file whose tail is a hole is catalogued with extents that
// end before f.Size. On restore, the file is left at the end of the last
// extent instead of Size — readers get EOF where the original file had
// zeros, silently truncating the restored file.
func TestExtractVolumeFileTrailingSparseHole(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	// Sparse file: 8192 bytes logical size, but only the first 2048 bytes
	// are allocated — the trailing 6144 bytes are a sparse hole (zeros),
	// so the scanner emitted no extent for them.
	const logicalSize = 8192
	const allocated = 2048
	backup := &manifest.Backup{
		BackupID:   "test-sparse-tail",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "db/sparse.dat",
				Size: logicalSize,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 0, Length: allocated},
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "sparse.dat")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	if _, err := restorer.ExtractFile(context.Background(), backup, "db/sparse.dat", outputPath); err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() != logicalSize {
		t.Fatalf("restored file truncated: got %d bytes, want %d (trailing sparse hole lost)", info.Size(), logicalSize)
	}

	// Allocated head must match; the sparse tail must read as zeros.
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	for i := 0; i < allocated; i++ {
		if data[i] != chunkData[i] {
			t.Fatalf("byte mismatch at offset %d: got %x, want %x", i, data[i], chunkData[i])
		}
	}
	for i := allocated; i < logicalSize; i++ {
		if data[i] != 0 {
			t.Fatalf("sparse tail not zero at offset %d: got %x", i, data[i])
		}
	}
}
