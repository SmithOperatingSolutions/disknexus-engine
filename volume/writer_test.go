// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func TestWriterWriteAtAndReadBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.img")

	w, err := volume.NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Write at specific offsets
	data1 := []byte("hello")
	data2 := []byte("world")

	if _, err := w.WriteAt(data1, 0); err != nil {
		t.Fatalf("WriteAt 0: %v", err)
	}
	if _, err := w.WriteAt(data2, 100); err != nil {
		t.Fatalf("WriteAt 100: %v", err)
	}

	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Read back and verify
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(content[0:5], data1) {
		t.Errorf("data at offset 0: got %q, want %q", content[0:5], data1)
	}
	if !bytes.Equal(content[100:105], data2) {
		t.Errorf("data at offset 100: got %q, want %q", content[100:105], data2)
	}

	// Gap should be zeros
	gap := content[5:100]
	for i, b := range gap {
		if b != 0 {
			t.Errorf("gap byte %d: got %d, want 0", i+5, b)
			break
		}
	}
}

func TestWriterTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.img")

	w, err := volume.NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	if err := w.Truncate(4096); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != 4096 {
		t.Errorf("size: got %d, want 4096", info.Size())
	}
}
