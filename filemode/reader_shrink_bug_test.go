// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestMultiFileReaderShrunkFileKeepsStreamAligned proves that when a source
// file shrinks between the catalog walk and the read, the stream must not
// lose the shortfall: every entry's StreamOffset was fixed at walk time, so
// a shorter-than-catalogued file would shift all subsequent files and make
// restore reconstruct them from the wrong bytes. The shortfall is padded
// with zeros instead.
func TestMultiFileReaderShrunkFileKeepsStreamAligned(t *testing.T) {
	dir := t.TempDir()

	aContent := bytes.Repeat([]byte("A"), 100)
	bContent := bytes.Repeat([]byte("B"), 50)
	os.WriteFile(filepath.Join(dir, "a.txt"), aContent, 0644)
	os.WriteFile(filepath.Join(dir, "b.txt"), bContent, 0644)

	cat, err := Walk(context.Background(), []string{dir}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// a.txt shrinks after the walk (e.g. concurrent modification).
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), aContent[:60], 0644); err != nil {
		t.Fatalf("truncating a.txt: %v", err)
	}

	reader := NewMultiFileReader(cat)
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	if int64(len(data)) != cat.TotalSize {
		t.Fatalf("stream length %d != catalogued total %d: downstream StreamOffsets are desynchronized", len(data), cat.TotalSize)
	}

	// a.txt occupies stream [0,100): 60 real bytes + 40 zeros.
	want := append(append(append([]byte{}, aContent[:60]...), make([]byte, 40)...), bContent...)
	if !bytes.Equal(data, want) {
		t.Fatalf("stream content mismatch: b.txt no longer starts at its catalogued StreamOffset")
	}
}
