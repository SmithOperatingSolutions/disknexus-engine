// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func TestExclusionMapLen(t *testing.T) {
	m := volume.NewExclusionMap()
	if m.Len() != 0 {
		t.Fatalf("empty map Len() = %d, want 0", m.Len())
	}

	m.AddRange(100, 50)
	if m.Len() != 1 {
		t.Fatalf("after 1 add Len() = %d, want 1", m.Len())
	}

	m.AddRange(200, 50)
	m.AddRange(300, 50)
	if m.Len() != 3 {
		t.Fatalf("after 3 adds Len() = %d, want 3", m.Len())
	}
}

func TestExcludedReaderZerosRegions(t *testing.T) {
	// Create a buffer filled with 0xFF
	data := bytes.Repeat([]byte{0xFF}, 1024)
	inner := bytes.NewReader(data)

	m := volume.NewExclusionMap()
	m.AddRange(100, 50) // bytes [100, 150)

	r := volume.NewExcludedReader(inner, m)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1024 {
		t.Fatalf("read %d bytes, want 1024", len(out))
	}

	// Bytes before the exclusion should be 0xFF
	for i := 0; i < 100; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}

	// Excluded region should be zeros
	for i := 100; i < 150; i++ {
		if out[i] != 0 {
			t.Fatalf("excluded byte %d = %#x, want 0", i, out[i])
		}
	}

	// Bytes after the exclusion should be 0xFF
	for i := 150; i < 1024; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}
}

func TestExcludedReaderNoExclusions(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 512)
	inner := bytes.NewReader(data)

	m := volume.NewExclusionMap()
	r := volume.NewExcludedReader(inner, m)

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(out, data) {
		t.Fatal("empty exclusion map should pass data through unchanged")
	}
}

func TestExcludedReaderMultipleRanges(t *testing.T) {
	data := bytes.Repeat([]byte{0xFF}, 1024)
	inner := bytes.NewReader(data)

	m := volume.NewExclusionMap()
	// Add ranges out of order to test sorting
	m.AddRange(500, 100) // [500, 600)
	m.AddRange(100, 50)  // [100, 150)
	m.AddRange(148, 12)  // [148, 160) — overlaps with previous

	r := volume.NewExcludedReader(inner, m)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	// Check non-excluded regions
	for i := 0; i < 100; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}

	// First exclusion [100,150) + overlap [148,160) → [100,160) zeroed
	for i := 100; i < 160; i++ {
		if out[i] != 0 {
			t.Fatalf("byte %d = %#x, want 0", i, out[i])
		}
	}

	// Gap between exclusions
	for i := 160; i < 500; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}

	// Second exclusion [500, 600)
	for i := 500; i < 600; i++ {
		if out[i] != 0 {
			t.Fatalf("byte %d = %#x, want 0", i, out[i])
		}
	}

	// After all exclusions
	for i := 600; i < 1024; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}
}

func TestReaderNonDevice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.dat")

	// Write known data
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i & 0xFF)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	r, err := volume.NewReader(path, 1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	if r.Size() != 4096 {
		t.Fatalf("Size() = %d, want 4096", r.Size())
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("data mismatch")
	}
	if r.Offset() != 4096 {
		t.Errorf("Offset() = %d, want 4096", r.Offset())
	}
}

func TestReaderSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.dat")

	data := []byte("hello")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	r, err := volume.NewReader(path, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Errorf("got %q, want %q", out, data)
	}
}

// TestIsDevicePath lives in isdevicepath_test.go: isDevicePath is unexported,
// so the table has to be exercised from an in-package test file — the copy
// that used to sit here (package volume_test) called nothing and asserted
// nothing (#402).

func TestExcludedReaderSmallReads(t *testing.T) {
	// Test that exclusion works correctly across multiple small reads
	data := bytes.Repeat([]byte{0xFF}, 256)
	inner := bytes.NewReader(data)

	m := volume.NewExclusionMap()
	m.AddRange(50, 100) // [50, 150)

	r := volume.NewExcludedReader(inner, m)

	// Read in 32-byte chunks to test cross-read-boundary behavior
	var out []byte
	buf := make([]byte, 32)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(out) != 256 {
		t.Fatalf("total read %d bytes, want 256", len(out))
	}

	for i := 0; i < 50; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}
	for i := 50; i < 150; i++ {
		if out[i] != 0 {
			t.Fatalf("byte %d = %#x, want 0", i, out[i])
		}
	}
	for i := 150; i < 256; i++ {
		if out[i] != 0xFF {
			t.Fatalf("byte %d = %#x, want 0xFF", i, out[i])
		}
	}
}
