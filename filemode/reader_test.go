// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiFileReaderBasic(t *testing.T) {
	dir := t.TempDir()

	// Create files with known content
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Files are sorted: a.txt before b.txt
	expected := "helloworld"
	if string(data) != expected {
		t.Errorf("got %q, want %q", string(data), expected)
	}
}

func TestMultiFileReaderSize(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("defgh"), 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	if reader.Size() != 8 { // 3 + 5
		t.Errorf("Size: got %d, want 8", reader.Size())
	}
}

func TestMultiFileReaderSmallReads(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("abc"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("def"), 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	// Read one byte at a time
	var result []byte
	buf := make([]byte, 1)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			result = append(result, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	expected := "abcdef"
	if string(result) != expected {
		t.Errorf("got %q, want %q", string(result), expected)
	}
}

func TestMultiFileReaderEmpty(t *testing.T) {
	dir := t.TempDir()

	// No files in directory
	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	if reader.Size() != 0 {
		t.Errorf("Size: got %d, want 0", reader.Size())
	}

	buf := make([]byte, 10)
	n, err := reader.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("expected (0, EOF), got (%d, %v)", n, err)
	}
}

func TestMultiFileReaderWithDirs(t *testing.T) {
	dir := t.TempDir()

	// Create a directory and a file
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	os.WriteFile(filepath.Join(dir, "subdir", "file.txt"), []byte("content"), 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Only file data should be in the stream (dirs contribute nothing)
	if string(data) != "content" {
		t.Errorf("got %q, want %q", string(data), "content")
	}
}

func TestMultiFileReaderLargeRead(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("ab"), 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("cd"), 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	// Read with buffer larger than total content
	buf := make([]byte, 1024)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if string(buf[:n]) != "abcd" {
		t.Errorf("got %q, want %q", string(buf[:n]), "abcd")
	}
}
