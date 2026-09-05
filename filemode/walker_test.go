// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func createTestTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create directory structure
	dirs := []string{
		"src",
		"src/pkg",
		"docs",
		"build",
	}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}

	// Create files with known content
	files := map[string]string{
		"src/main.go":    "package main\n",
		"src/pkg/lib.go": "package pkg\n",
		"docs/readme.md": "# README\n",
		"build/out.bin":  "binary",
		"root.txt":       "root file\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestWalkBasic(t *testing.T) {
	dir := createTestTree(t)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(cat.SourcePaths) != 1 {
		t.Errorf("SourcePaths: got %d, want 1", len(cat.SourcePaths))
	}

	// Should have 4 dirs + 5 files = 9 entries
	if len(cat.Files) != 9 {
		t.Errorf("Files: got %d, want 9", len(cat.Files))
		for _, f := range cat.Files {
			t.Logf("  %s (dir=%v, sym=%v, size=%d)", f.Path, f.IsDir, f.IsSymlink, f.Size)
		}
	}

	// Verify total size
	expectedSize := int64(len("package main\n") + len("package pkg\n") +
		len("# README\n") + len("binary") + len("root file\n"))
	if cat.TotalSize != expectedSize {
		t.Errorf("TotalSize: got %d, want %d", cat.TotalSize, expectedSize)
	}
}

func TestWalkSorted(t *testing.T) {
	dir := createTestTree(t)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Check that files are sorted by path
	for i := 1; i < len(cat.Files); i++ {
		if cat.Files[i].Path < cat.Files[i-1].Path {
			t.Errorf("not sorted at index %d: %q < %q",
				i, cat.Files[i].Path, cat.Files[i-1].Path)
		}
	}
}

func TestWalkWithExclusions(t *testing.T) {
	dir := createTestTree(t)

	matcher := NewMatcher([]string{"build/", "*.md"})
	cat, err := Walk(context.Background(), []string{dir}, matcher, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	for _, f := range cat.Files {
		if f.Path == "build" || f.Path == "build/out.bin" {
			t.Errorf("build should be excluded, found: %s", f.Path)
		}
		if filepath.Ext(f.Path) == ".md" {
			t.Errorf("*.md should be excluded, found: %s", f.Path)
		}
	}
}

func TestWalkStreamOffsets(t *testing.T) {
	dir := createTestTree(t)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Verify stream offsets are sequential
	var lastEnd int64
	for _, f := range cat.Files {
		if f.IsDir || f.IsSymlink || f.Size == 0 {
			continue
		}
		if f.StreamOffset != lastEnd {
			t.Errorf("file %s: offset %d, expected %d", f.Path, f.StreamOffset, lastEnd)
		}
		lastEnd = f.StreamOffset + f.StreamLength
	}

	if lastEnd != cat.TotalSize {
		t.Errorf("last offset %d != TotalSize %d", lastEnd, cat.TotalSize)
	}
}

func TestWalkMultipleSources(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	os.WriteFile(filepath.Join(dir1, "a.txt"), []byte("aaa"), 0644)
	os.WriteFile(filepath.Join(dir2, "b.txt"), []byte("bbb"), 0644)

	cat, err := Walk(context.Background(), []string{dir1, dir2}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(cat.SourcePaths) != 2 {
		t.Errorf("SourcePaths: got %d, want 2", len(cat.SourcePaths))
	}
	if len(cat.Files) != 2 {
		t.Errorf("Files: got %d, want 2", len(cat.Files))
	}

	// Source indexes should be correct
	for _, f := range cat.Files {
		if f.Path == "a.txt" && f.SourceIndex != 0 {
			t.Errorf("a.txt: SourceIndex=%d, want 0", f.SourceIndex)
		}
		if f.Path == "b.txt" && f.SourceIndex != 1 {
			t.Errorf("b.txt: SourceIndex=%d, want 1", f.SourceIndex)
		}
	}
}

func TestWalkNotADirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	os.WriteFile(file, []byte("hello"), 0644)

	_, err := Walk(context.Background(), []string{file}, nil, nil)
	if err == nil {
		t.Error("expected error for non-directory source")
	}
}

func TestCatalogFilterByPrefix(t *testing.T) {
	dir := createTestTree(t)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	filtered := cat.FilterByPrefix("src")
	for _, f := range filtered {
		if f.Path != "src" && !hasPrefix(f.Path, "src/") {
			t.Errorf("unexpected entry %q for prefix 'src'", f.Path)
		}
	}

	if len(filtered) == 0 {
		t.Error("expected some entries for prefix 'src'")
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestWalkSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	var linkEntry *manifest.FileEntry
	for i := range cat.Files {
		if cat.Files[i].Path == "link.txt" {
			e := cat.Files[i]
			linkEntry = &e
			break
		}
	}
	if linkEntry == nil {
		t.Fatal("link.txt not found in catalog")
	}
	if !linkEntry.IsSymlink {
		t.Error("link.txt: IsSymlink should be true")
	}
	if linkEntry.LinkTarget != "target.txt" {
		t.Errorf("link.txt: LinkTarget=%q, want 'target.txt'", linkEntry.LinkTarget)
	}
	if linkEntry.StreamLength != 0 {
		t.Errorf("link.txt: StreamLength=%d, want 0", linkEntry.StreamLength)
	}

	// TotalSize counts only regular file data; symlink should not contribute.
	expected := int64(len("content"))
	if cat.TotalSize != expected {
		t.Errorf("TotalSize=%d, want %d (symlink must not be counted)", cat.TotalSize, expected)
	}
}

func TestWalkDeepPath(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf.txt"), []byte("deep"), 0644); err != nil {
		t.Fatal(err)
	}

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	want := "a/b/c/d/e/f/g/h/i/j/leaf.txt"
	var leafEntry *manifest.FileEntry
	for i := range cat.Files {
		if cat.Files[i].Path == want {
			e := cat.Files[i]
			leafEntry = &e
			break
		}
	}
	if leafEntry == nil {
		t.Fatalf("entry %q not found in catalog", want)
	}
	if leafEntry.IsDir {
		t.Error("leaf.txt should not be a directory")
	}
	if leafEntry.StreamLength == 0 {
		t.Error("leaf.txt: StreamLength should be > 0")
	}
}
