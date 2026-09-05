// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestFileReadWriteAtRoundTrip writes data at several offsets and reads it back,
// verifying that fileReadAt/fileWriteAt produce byte-identical results.
func TestFileReadWriteAtRoundTrip(t *testing.T) {
	f := createTempIndexFile(t)

	// Write at offset 0.
	payload := []byte("hello, fileReadAt!")
	n, err := fileWriteAt(f, payload, 0)
	if err != nil {
		t.Fatalf("writeAt 0: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("writeAt 0: wrote %d, want %d", n, len(payload))
	}

	// Write at a gap (offset 1024); region [len(payload)..1024) is zero-filled by OS.
	payload2 := []byte("second region")
	if _, err := fileWriteAt(f, payload2, 1024); err != nil {
		t.Fatalf("writeAt 1024: %v", err)
	}

	// Read back offset 0.
	buf := make([]byte, len(payload))
	n, err = fileReadAt(f, buf, 0)
	if err != nil {
		t.Fatalf("readAt 0: %v", err)
	}
	if n != len(payload) || !bytes.Equal(buf, payload) {
		t.Errorf("readAt 0: got %q, want %q", buf[:n], payload)
	}

	// Read back offset 1024.
	buf2 := make([]byte, len(payload2))
	n, err = fileReadAt(f, buf2, 1024)
	if err != nil {
		t.Fatalf("readAt 1024: %v", err)
	}
	if !bytes.Equal(buf2, payload2) {
		t.Errorf("readAt 1024: got %q, want %q", buf2[:n], payload2)
	}

	// Read the zero gap — bytes between the two payloads must be zero.
	gapStart := int64(len(payload))
	gapLen := 1024 - gapStart
	gap := make([]byte, gapLen)
	if _, err := fileReadAt(f, gap, gapStart); err != nil {
		t.Fatalf("readAt gap: %v", err)
	}
	for i, b := range gap {
		if b != 0 {
			t.Errorf("gap[%d] = 0x%02x, want 0x00", i, b)
			break
		}
	}
}

// TestFileReadAtPastEOF verifies that reading beyond the end of the file
// returns an error (consistent with os.File.ReadAt behaviour).
func TestFileReadAtPastEOF(t *testing.T) {
	f := createTempIndexFile(t)
	if _, err := fileWriteAt(f, []byte("short"), 0); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 100) // much larger than the 5-byte file
	_, err := fileReadAt(f, buf, 0)
	if err == nil {
		t.Fatal("expected error when reading past EOF, got nil")
	}
}

// TestFileReadAtExactEOF reads exactly up to the last byte of the file.
func TestFileReadAtExactEOF(t *testing.T) {
	f := createTempIndexFile(t)
	data := []byte("exactly-this-much")
	if _, err := fileWriteAt(f, data, 0); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len(data))
	n, err := fileReadAt(f, buf, 0)
	if err != nil {
		t.Fatalf("readAt exact size: %v", err)
	}
	if n != len(data) || !bytes.Equal(buf, data) {
		t.Errorf("got %q (%d bytes), want %q", buf[:n], n, data)
	}
}

// TestFileReadWriteAtEmptyBuf verifies the zero-length edge case.
func TestFileReadWriteAtEmptyBuf(t *testing.T) {
	f := createTempIndexFile(t)

	n, err := fileReadAt(f, nil, 0)
	if err != nil {
		t.Errorf("readAt nil buf: %v", err)
	}
	if n != 0 {
		t.Errorf("readAt nil buf: n = %d, want 0", n)
	}

	n, err = fileWriteAt(f, nil, 0)
	if err != nil {
		t.Errorf("writeAt nil buf: %v", err)
	}
	if n != 0 {
		t.Errorf("writeAt nil buf: n = %d, want 0", n)
	}

	n, err = fileReadAt(f, []byte{}, 0)
	if err != nil {
		t.Errorf("readAt empty slice: %v", err)
	}
	if n != 0 {
		t.Errorf("readAt empty slice: n = %d, want 0", n)
	}
}

// TestFileWriteAtOverwrite writes data, then overwrites a sub-region and
// verifies both the overwritten and surrounding regions.
func TestFileWriteAtOverwrite(t *testing.T) {
	f := createTempIndexFile(t)

	original := bytes.Repeat([]byte{0xAA}, 256)
	if _, err := fileWriteAt(f, original, 0); err != nil {
		t.Fatal(err)
	}

	// Overwrite bytes [64..80) with 0xBB.
	patch := bytes.Repeat([]byte{0xBB}, 16)
	if _, err := fileWriteAt(f, patch, 64); err != nil {
		t.Fatal(err)
	}

	// Read the whole region.
	buf := make([]byte, 256)
	if _, err := fileReadAt(f, buf, 0); err != nil {
		t.Fatal(err)
	}

	// Bytes [0..64) should be 0xAA.
	for i := 0; i < 64; i++ {
		if buf[i] != 0xAA {
			t.Fatalf("buf[%d] = 0x%02x, want 0xAA", i, buf[i])
		}
	}
	// Bytes [64..80) should be 0xBB.
	for i := 64; i < 80; i++ {
		if buf[i] != 0xBB {
			t.Fatalf("buf[%d] = 0x%02x, want 0xBB", i, buf[i])
		}
	}
	// Bytes [80..256) should be 0xAA.
	for i := 80; i < 256; i++ {
		if buf[i] != 0xAA {
			t.Fatalf("buf[%d] = 0x%02x, want 0xAA", i, buf[i])
		}
	}
}

// TestFileReadWriteAtMultipleFiles verifies that interleaved I/O across two
// files does not mix up data (guards against handle/offset confusion).
func TestFileReadWriteAtMultipleFiles(t *testing.T) {
	f1 := createTempIndexFile(t)
	f2 := createTempIndexFile(t)

	d1 := []byte("file-one-data")
	d2 := []byte("file-two-data")
	if _, err := fileWriteAt(f1, d1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriteAt(f2, d2, 0); err != nil {
		t.Fatal(err)
	}

	buf1 := make([]byte, len(d1))
	buf2 := make([]byte, len(d2))
	if _, err := fileReadAt(f1, buf1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := fileReadAt(f2, buf2, 0); err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(buf1, d1) {
		t.Errorf("file1: got %q, want %q", buf1, d1)
	}
	if !bytes.Equal(buf2, d2) {
		t.Errorf("file2: got %q, want %q", buf2, d2)
	}
}

// createTempIndexFile returns an open *os.File in a test-scoped temp dir.
func createTempIndexFile(t *testing.T) *os.File {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dat")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
