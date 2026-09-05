// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"path/filepath"
	"testing"
)

// SourceSize on a regular file (the image-file capture case) must report the
// byte length. The raw-device branch is Windows-only and proven there; this
// pins the file branch every platform exercises.
func TestSourceSizeRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img")
	if err := os.WriteFile(p, make([]byte, 4096+123), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n, err := SourceSize(f)
	if err != nil {
		t.Fatalf("SourceSize: %v", err)
	}
	if n != 4096+123 {
		t.Fatalf("SourceSize = %d, want %d", n, 4096+123)
	}
	// Offset-neutral: callers size mid-stream (the restore path sizes an
	// O_RDWR handle it is about to write through) — SourceSize must not
	// move the cursor.
	pos, err := f.Seek(0, 1) // io.SeekCurrent
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("SourceSize moved the offset to %d, want 0", pos)
	}
}
