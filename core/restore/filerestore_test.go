// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func TestMatchFilePatternBasicGlob(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		{"src/main.go", "*.go", true},
		{"src/pkg/lib.go", "*.go", true},
		{"readme.md", "*.go", false},
		{"src/main.go", "main.go", true},
		{"src/main.go", "src/main.go", true},
	}

	for _, tt := range tests {
		got := MatchFilePattern(tt.path, tt.pattern)
		if got != tt.want {
			t.Errorf("MatchFilePattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
		}
	}
}

func TestMatchFilePatternDoubleGlob(t *testing.T) {
	tests := []struct {
		path    string
		pattern string
		want    bool
	}{
		{"src/main.go", "src/**/*.go", true},
		{"src/pkg/lib.go", "src/**/*.go", true},
		{"src/main.go", "**/*.go", true},
		{"main.go", "**/*.go", true},
		{"src/main.txt", "**/*.go", false},
	}

	for _, tt := range tests {
		got := MatchFilePattern(tt.path, tt.pattern)
		if got != tt.want {
			t.Errorf("MatchFilePattern(%q, %q) = %v, want %v", tt.path, tt.pattern, got, tt.want)
		}
	}
}

func TestFilterFiles(t *testing.T) {
	entries := []manifest.FileEntry{
		{Path: "src", IsDir: true},
		{Path: "src/main.go"},
		{Path: "src/util.go"},
		{Path: "docs", IsDir: true},
		{Path: "docs/readme.md"},
		{Path: "root.txt"},
	}

	result := FilterFiles(entries, []string{"*.go"})

	// Should include src/main.go, src/util.go, and parent dir "src"
	var goFiles, dirs int
	for _, f := range result {
		if f.IsDir {
			dirs++
		} else {
			goFiles++
			if filepath.Ext(f.Path) != ".go" {
				t.Errorf("unexpected non-.go file: %s", f.Path)
			}
		}
	}

	if goFiles != 2 {
		t.Errorf("got %d .go files, want 2", goFiles)
	}
	if dirs != 1 {
		t.Errorf("got %d dirs, want 1 (src)", dirs)
	}
}

// setupTestRepo creates a temp repo with a stored chunk and returns the components needed for testing.
func setupTestRepo(t *testing.T) (repoPath string, dedupIdx *index.DedupIndex, chunkStore *store.ChunkStore, chunkData []byte, chunkHash [32]byte) {
	t.Helper()
	dir := t.TempDir()
	repoPath = filepath.Join(dir, "repo")

	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	chunkData = make([]byte, 4096)
	rand.Read(chunkData)
	packNum, offset, _, err := chunkStore.Store(chunkData)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	chunkHash = sha256.Sum256(chunkData)

	dedupIdx, err = index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}

	dedupIdx.Insert(hasher.ChunkID{StrongHash: chunkHash}, packNum, uint64(offset), uint32(len(chunkData)))
	if err := dedupIdx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	return repoPath, dedupIdx, chunkStore, chunkData, chunkHash
}

func TestExtractFile(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	backup := &manifest.Backup{
		BackupID:   "test-extract",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "src", IsDir: true, Mode: 0755},
			{Path: "src/main.go", Size: 4096, Mode: 0644, StreamOffset: 0, StreamLength: 4096},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: 4096},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.go")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "src/main.go", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.RestoredFiles != 1 {
		t.Errorf("RestoredFiles: got %d, want 1", result.RestoredFiles)
	}
	if result.TotalFiles != 1 {
		t.Errorf("TotalFiles: got %d, want 1", result.TotalFiles)
	}
	if result.BytesWritten != 4096 {
		t.Errorf("BytesWritten: got %d, want 4096", result.BytesWritten)
	}

	// Verify file contents
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if len(data) != len(chunkData) {
		t.Fatalf("output size: got %d, want %d", len(data), len(chunkData))
	}
	for i := range data {
		if data[i] != chunkData[i] {
			t.Fatalf("byte mismatch at offset %d", i)
		}
	}

	// Verify the output is a flat file, not a directory tree
	if _, err := os.Stat(filepath.Dir(outputPath)); err != nil {
		t.Errorf("parent dir should exist: %v", err)
	}
}

func TestExtractFileNotFound(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	backup := &manifest.Backup{
		BackupID:   "test-extract-notfound",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "src/main.go", Size: 4096, Mode: 0644},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.go")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	_, err := restorer.ExtractFile(context.Background(), backup, "nonexistent.go", outputPath)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if got := err.Error(); got != `file "nonexistent.go" not found in backup catalog` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestExtractFileRejectsDirectory(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	backup := &manifest.Backup{
		BackupID:   "test-extract-dir",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "src", IsDir: true, Mode: 0755},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	_, err := restorer.ExtractFile(context.Background(), backup, "src", outputPath)
	if err == nil {
		t.Fatal("expected error for directory")
	}
	if got := err.Error(); got != `"src" is a directory, not a file` {
		t.Errorf("unexpected error: %s", got)
	}
}

func TestExtractFileRejectsNoFileCatalog(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	backup := &manifest.Backup{
		BackupID:   "test-extract-volume",
		BackupMode: "volume",
	}

	outputPath := filepath.Join(t.TempDir(), "out")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	_, err := restorer.ExtractFile(context.Background(), backup, "file.txt", outputPath)
	if err == nil {
		t.Fatal("expected error for backup without file catalog")
	}
}

func TestExtractVolumeFile(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	// Volume backup with file catalog (--capture-files)
	// The file is at bytes 100-2100 within the chunk
	backup := &manifest.Backup{
		BackupID:   "test-extract-volume-file",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "data/file.bin",
				Size: 2000,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 100, Length: 2000},
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.bin")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "data/file.bin", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.BytesWritten != 2000 {
		t.Errorf("BytesWritten: got %d, want 2000", result.BytesWritten)
	}

	// Verify the extracted data matches the slice of the chunk
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if len(data) != 2000 {
		t.Fatalf("output size: got %d, want 2000", len(data))
	}
	for i := range data {
		if data[i] != chunkData[100+i] {
			t.Fatalf("byte mismatch at offset %d: got %x, want %x", i, data[i], chunkData[100+i])
		}
	}
}

// TestExtractVolumeFileClusterSlack verifies that when VolumeExtent.Length is larger
// than FileEntry.Size (cluster-aligned NTFS/FAT slack), the output is truncated to
// the logical file size and BytesWritten reflects that size.
func TestExtractVolumeFileClusterSlack(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	// File is 2000 bytes logically, but NTFS allocated a full 4096-byte cluster.
	const logicalSize = 2000
	backup := &manifest.Backup{
		BackupID:   "test-cluster-slack",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "docs/DD214.pdf",
				Size: logicalSize,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 0, Length: 4096}, // cluster-aligned, 4096 > logicalSize
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "DD214.pdf")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "docs/DD214.pdf", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.BytesWritten != logicalSize {
		t.Errorf("BytesWritten: got %d, want %d (logical size)", result.BytesWritten, logicalSize)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() != logicalSize {
		t.Errorf("output file size: got %d, want %d (logical size, not cluster size)", info.Size(), logicalSize)
	}

	// Data should match the first logicalSize bytes of the chunk.
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	for i := range data {
		if data[i] != chunkData[i] {
			t.Fatalf("byte mismatch at offset %d: got %x, want %x", i, data[i], chunkData[i])
		}
	}
}

func TestExtractFileIncompleteCatalogEntryErrors(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	// Simulates a FileEntry produced by --capture-files on an NTFS volume
	// where VolumeExtents were not populated: Size > 0, StreamLength == 0, VolumeExtents == nil.
	backup := &manifest.Backup{
		BackupID:   "test-incomplete-entry",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{Path: "Users/Katrena/Documents/DD214.pdf", Size: 12345, Mode: 0644},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.pdf")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	_, err := restorer.ExtractFile(context.Background(), backup, "Users/Katrena/Documents/DD214.pdf", outputPath)
	if err == nil {
		t.Fatal("expected error for incomplete catalog entry, got nil (would have silently written 0 bytes)")
	}

	// Output file should not exist (or be empty) — we must not create a misleading 0-byte file
	if info, statErr := os.Stat(outputPath); statErr == nil && info.Size() == 0 {
		t.Error("0-byte output file was created; should not exist when restore fails")
	}
}

func TestExtractFileIncompleteCatalogEntryIgnoreErrors(t *testing.T) {
	repoPath, dedupIdx, chunkStore, _, _ := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	backup := &manifest.Backup{
		BackupID:   "test-incomplete-ignore",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{Path: "Users/Katrena/Documents/DD214.pdf", Size: 12345, Mode: 0644},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.pdf")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)
	restorer.IgnoreErrors = true

	result, err := restorer.ExtractFile(context.Background(), backup, "Users/Katrena/Documents/DD214.pdf", outputPath)
	if err != nil {
		t.Fatalf("expected no error with IgnoreErrors=true, got: %v", err)
	}
	if result.BytesWritten != 0 {
		t.Errorf("BytesWritten: got %d, want 0 (file was skipped)", result.BytesWritten)
	}
}

func TestExtractVolumeFileMultipleExtents(t *testing.T) {
	// Setup two chunks to simulate a fragmented file
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")

	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	// Create two chunks
	chunk1 := make([]byte, 4096)
	chunk2 := make([]byte, 4096)
	rand.Read(chunk1)
	rand.Read(chunk2)

	packNum1, offset1, _, err := chunkStore.Store(chunk1)
	if err != nil {
		t.Fatalf("Store chunk1: %v", err)
	}
	packNum2, offset2, _, err := chunkStore.Store(chunk2)
	if err != nil {
		t.Fatalf("Store chunk2: %v", err)
	}

	hash1 := sha256.Sum256(chunk1)
	hash2 := sha256.Sum256(chunk2)

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	dedupIdx.Insert(hasher.ChunkID{StrongHash: hash1}, packNum1, uint64(offset1), uint32(len(chunk1)))
	dedupIdx.Insert(hasher.ChunkID{StrongHash: hash2}, packNum2, uint64(offset2), uint32(len(chunk2)))
	if err := dedupIdx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Fragmented file: first 1000 bytes from chunk1, next 1500 from chunk2
	backup := &manifest.Backup{
		BackupID:   "test-fragmented",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "frag.bin",
				Size: 2500,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 500, Length: 1000},     // from chunk1 at offset 500
					{FileOffset: 1000, VolumeOffset: 4096, Length: 1500}, // from chunk2 at offset 0
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 4096},
			{VolumeOffset: 4096, ChunkHash: hash2, ChunkLength: 4096},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.bin")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "frag.bin", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.BytesWritten != 2500 {
		t.Errorf("BytesWritten: got %d, want 2500", result.BytesWritten)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if len(data) != 2500 {
		t.Fatalf("output size: got %d, want 2500", len(data))
	}

	// First 1000 bytes should come from chunk1 starting at offset 500
	for i := 0; i < 1000; i++ {
		if data[i] != chunk1[500+i] {
			t.Fatalf("byte mismatch at offset %d (extent 0)", i)
		}
	}
	// Next 1500 bytes should come from chunk2 starting at offset 0
	for i := 0; i < 1500; i++ {
		if data[1000+i] != chunk2[i] {
			t.Fatalf("byte mismatch at offset %d (extent 1)", 1000+i)
		}
	}
}

// TestExtractVolumeFileLeadingTrimAndClusterSlack exercises the combination of a chunk
// that starts before the extent (leading trim via sliceStart) AND cluster slack where
// ext.Length > f.Size (trailing truncation). Both corrections must apply together.
func TestExtractVolumeFileLeadingTrimAndClusterSlack(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	// Chunk covers volume bytes 0–4095.
	// Extent starts 200 bytes in (VolumeOffset=200), so sliceStart=200.
	// Extent length is 4096-200=3896 (cluster-aligned past end of file).
	// Logical file size is 2000 — smaller than extent, so truncation is required.
	const logicalSize = 2000
	backup := &manifest.Backup{
		BackupID:   "test-leadingtrim-slack",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "docs/file.pdf",
				Size: logicalSize,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 200, Length: 3896}, // chunk starts 200 bytes before extent; length > logicalSize
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "file.pdf")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "docs/file.pdf", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.BytesWritten != logicalSize {
		t.Errorf("BytesWritten: got %d, want %d", result.BytesWritten, logicalSize)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if int64(len(data)) != logicalSize {
		t.Fatalf("output size: got %d, want %d", len(data), logicalSize)
	}
	// Content should be chunkData[200 : 200+logicalSize]
	for i := range data {
		if data[i] != chunkData[200+i] {
			t.Fatalf("byte mismatch at offset %d: got %x, want %x", i, data[i], chunkData[200+i])
		}
	}
}

// TestExtractVolumeFileMultipleExtentsWithClusterSlack exercises a fragmented file
// (two extents on different disk regions) where the last extent is cluster-aligned
// and extends past the logical file size. The truncation must account for the total
// across all extents, not just the last one.
func TestExtractVolumeFileMultipleExtentsWithClusterSlack(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")

	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize,
		ChunkMaxSize: cfg.ChunkMaxSize, BuzhashMask: cfg.BuzhashMask,
		PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer chunkStore.Close()

	chunk1 := make([]byte, 4096)
	chunk2 := make([]byte, 4096)
	rand.Read(chunk1)
	rand.Read(chunk2)

	packNum1, offset1, _, err := chunkStore.Store(chunk1)
	if err != nil {
		t.Fatalf("Store chunk1: %v", err)
	}
	packNum2, offset2, _, err := chunkStore.Store(chunk2)
	if err != nil {
		t.Fatalf("Store chunk2: %v", err)
	}

	hash1 := sha256.Sum256(chunk1)
	hash2 := sha256.Sum256(chunk2)

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer dedupIdx.Close()

	dedupIdx.Insert(hasher.ChunkID{StrongHash: hash1}, packNum1, uint64(offset1), uint32(len(chunk1)))
	dedupIdx.Insert(hasher.ChunkID{StrongHash: hash2}, packNum2, uint64(offset2), uint32(len(chunk2)))
	if err := dedupIdx.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Fragmented file: 1000 bytes from chunk1, then a cluster-aligned second extent
	// (4096 bytes) from chunk2 — but logical file size is only 1000+1500=2500.
	// The last 2596 bytes of the second extent are cluster slack.
	const logicalSize = int64(2500)
	backup := &manifest.Backup{
		BackupID:   "test-multi-slack",
		BackupMode: "volume",
		FileCatalog: []manifest.FileEntry{
			{
				Path: "frag.bin",
				Size: logicalSize,
				Mode: 0644,
				VolumeExtents: []manifest.VolumeExtent{
					{FileOffset: 0, VolumeOffset: 500, Length: 1000},     // exact
					{FileOffset: 1000, VolumeOffset: 4096, Length: 4096}, // cluster-aligned; 2596 bytes of slack
				},
			},
		},
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: hash1, ChunkLength: 4096},
			{VolumeOffset: 4096, ChunkHash: hash2, ChunkLength: 4096},
		},
	}

	outputPath := filepath.Join(t.TempDir(), "out.bin")
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	restorer := NewFileRestorer(dedupIdx, chunkStore, repoPath, logger)

	result, err := restorer.ExtractFile(context.Background(), backup, "frag.bin", outputPath)
	if err != nil {
		t.Fatalf("ExtractFile: %v", err)
	}

	if result.BytesWritten != logicalSize {
		t.Errorf("BytesWritten: got %d, want %d", result.BytesWritten, logicalSize)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if int64(len(data)) != logicalSize {
		t.Fatalf("output size: got %d, want %d", len(data), logicalSize)
	}

	// First 1000 bytes: chunk1[500:1500]
	for i := 0; i < 1000; i++ {
		if data[i] != chunk1[500+i] {
			t.Fatalf("byte mismatch at offset %d (extent 0)", i)
		}
	}
	// Next 1500 bytes: chunk2[0:1500]
	for i := 0; i < 1500; i++ {
		if data[1000+i] != chunk2[i] {
			t.Fatalf("byte mismatch at offset %d (extent 1)", 1000+i)
		}
	}
}

func TestRestoreInlineFile(t *testing.T) {
	content := []byte("resident MFT data")
	entry := manifest.FileEntry{
		Path:       "mft.txt",
		Size:       int64(len(content)),
		Mode:       0644,
		InlineData: content,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewFileRestorer(nil, nil, t.TempDir(), logger)
	targetPath := filepath.Join(t.TempDir(), "out.txt")

	n, err := r.restoreInlineFile(entry, targetPath)
	if err != nil {
		t.Fatalf("restoreInlineFile: %v", err)
	}
	if n != int64(len(content)) {
		t.Errorf("returned n=%d, want %d", n, len(content))
	}

	got, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("file content mismatch: got %q, want %q", got, content)
	}
}

func TestRestoreInlineFileEmpty(t *testing.T) {
	entry := manifest.FileEntry{
		Path:       "empty.txt",
		Size:       0,
		Mode:       0644,
		InlineData: []byte{},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewFileRestorer(nil, nil, t.TempDir(), logger)
	targetPath := filepath.Join(t.TempDir(), "empty.txt")

	n, err := r.restoreInlineFile(entry, targetPath)
	if err != nil {
		t.Fatalf("restoreInlineFile empty: %v", err)
	}
	if n != 0 {
		t.Errorf("returned n=%d, want 0", n)
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatalf("stat output: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("file size=%d, want 0", info.Size())
	}
}
