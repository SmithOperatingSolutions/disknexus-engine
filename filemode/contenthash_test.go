// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func TestComputeContentHashesBasic(t *testing.T) {
	hash1 := sha256.Sum256([]byte("chunk1"))
	hash2 := sha256.Sum256([]byte("chunk2"))
	hash3 := sha256.Sum256([]byte("chunk3"))

	backup := &manifest.Backup{
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 100},
			{VolumeOffset: 100, ChunkHash: hash2, ChunkLength: 100},
			{VolumeOffset: 200, ChunkHash: hash3, ChunkLength: 100},
		},
		FileCatalog: []manifest.FileEntry{
			{Path: "a.txt", StreamOffset: 0, StreamLength: 100, ModTime: time.Now()},
			{Path: "b.txt", StreamOffset: 100, StreamLength: 200, ModTime: time.Now()},
		},
	}

	ComputeContentHashes(backup)

	// a.txt covers only chunk1
	var zeroHash [32]byte
	if backup.FileCatalog[0].ContentHash == zeroHash {
		t.Error("a.txt ContentHash should not be zero")
	}

	// b.txt covers chunk2 and chunk3
	if backup.FileCatalog[1].ContentHash == zeroHash {
		t.Error("b.txt ContentHash should not be zero")
	}

	// Different files covering different chunks should have different hashes
	if backup.FileCatalog[0].ContentHash == backup.FileCatalog[1].ContentHash {
		t.Error("different files should have different content hashes")
	}
}

func TestComputeContentHashesDirs(t *testing.T) {
	backup := &manifest.Backup{
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: sha256.Sum256([]byte("data")), ChunkLength: 100},
		},
		FileCatalog: []manifest.FileEntry{
			{Path: "dir", IsDir: true, StreamLength: 0, ModTime: time.Now()},
			{Path: "file.txt", StreamOffset: 0, StreamLength: 100, ModTime: time.Now()},
		},
	}

	ComputeContentHashes(backup)

	// Directory should have zero hash
	var zeroHash [32]byte
	if backup.FileCatalog[0].ContentHash != zeroHash {
		t.Error("directory ContentHash should be zero")
	}

	// File should have non-zero hash
	if backup.FileCatalog[1].ContentHash == zeroHash {
		t.Error("file ContentHash should not be zero")
	}
}

func TestComputeContentHashesEmpty(t *testing.T) {
	// Should not panic on empty catalog
	backup := &manifest.Backup{}
	ComputeContentHashes(backup)

	backup2 := &manifest.Backup{
		Entries:     []manifest.Entry{{VolumeOffset: 0, ChunkLength: 100}},
		FileCatalog: nil,
	}
	ComputeContentHashes(backup2)
}

func TestComputeContentHashesVolumeExtents(t *testing.T) {
	hash1 := sha256.Sum256([]byte("chunk1"))
	hash2 := sha256.Sum256([]byte("chunk2"))
	hash3 := sha256.Sum256([]byte("chunk3"))

	backup := &manifest.Backup{
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 1000},
			{VolumeOffset: 1000, ChunkHash: hash2, ChunkLength: 1000},
			{VolumeOffset: 2000, ChunkHash: hash3, ChunkLength: 1000},
		},
		FileCatalog: []manifest.FileEntry{
			{
				Path: "file1.txt",
				Size: 500,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 100, Length: 500},
				},
			},
			{
				Path: "file2.txt",
				Size: 1500,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 500, Length: 700},
					{FileOffset: 700, VolumeOffset: 2000, Length: 800},
				},
			},
		},
	}

	ComputeContentHashes(backup)

	var zeroHash [32]byte
	// file1.txt overlaps chunk1 → should have non-zero hash
	if backup.FileCatalog[0].ContentHash == zeroHash {
		t.Error("file1.txt ContentHash should not be zero")
	}

	// file2.txt overlaps chunk1, chunk3 → should have non-zero hash
	if backup.FileCatalog[1].ContentHash == zeroHash {
		t.Error("file2.txt ContentHash should not be zero")
	}

	// Different extents covering different chunks → different hashes
	if backup.FileCatalog[0].ContentHash == backup.FileCatalog[1].ContentHash {
		t.Error("files covering different chunk sets should have different hashes")
	}
}

func TestComputeContentHashesVolumeExtentNoExtents(t *testing.T) {
	backup := &manifest.Backup{
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: sha256.Sum256([]byte("data")), ChunkLength: 100},
		},
		FileCatalog: []manifest.FileEntry{
			{
				Path:          "empty.txt",
				Size:          0,
				VolumeExtents: []manifest.VolumeExtent{},
			},
		},
	}

	ComputeContentHashes(backup)

	var zeroHash [32]byte
	if backup.FileCatalog[0].ContentHash != zeroHash {
		t.Error("file with empty VolumeExtents should have zero hash")
	}
}

func TestComputeContentHashesSameChunks(t *testing.T) {
	hash1 := sha256.Sum256([]byte("chunk1"))

	backup := &manifest.Backup{
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 100},
		},
		FileCatalog: []manifest.FileEntry{
			{Path: "a.txt", StreamOffset: 0, StreamLength: 100, ModTime: time.Now()},
			// Same bytes would map to same chunks = same content hash
		},
	}

	ComputeContentHashes(backup)

	var zeroHash [32]byte
	if backup.FileCatalog[0].ContentHash == zeroHash {
		t.Error("ContentHash should not be zero")
	}
}
