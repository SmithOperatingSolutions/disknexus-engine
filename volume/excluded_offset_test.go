// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"bytes"
	"io"
	"testing"
)

func TestZeroExcluded(t *testing.T) {
	m := NewExclusionMap()
	m.AddRange(4, 3) // zero bytes [4,7)
	buf := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	m.ZeroExcluded(buf, 0)
	want := []byte{1, 2, 3, 4, 0, 0, 0, 8, 9, 10}
	if !bytes.Equal(buf, want) {
		t.Fatalf("got %v, want %v", buf, want)
	}

	// With a buffer starting at offset 2, the same absolute range [4,7) maps to
	// buffer indices [2,5).
	m2 := NewExclusionMap()
	m2.AddRange(4, 3)
	buf2 := []byte{3, 4, 5, 6, 7} // represents source [2,7)
	m2.ZeroExcluded(buf2, 2)
	want2 := []byte{3, 4, 0, 0, 0}
	if !bytes.Equal(buf2, want2) {
		t.Fatalf("offset: got %v, want %v", buf2, want2)
	}
}

// TestExcludedReaderAt: an offset-started excluded reader zeros the same
// absolute ranges as a from-zero reader over the same tail (#54).
func TestExcludedReaderAt(t *testing.T) {
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i + 1)
	}
	m := NewExclusionMap()
	m.AddRange(60, 10) // absolute [60,70)

	// Read the tail [50,100) via an offset-aware excluded reader.
	r := NewExcludedReaderAt(bytes.NewReader(data[50:]), m, 50)
	got, _ := io.ReadAll(r)

	want := make([]byte, 50)
	copy(want, data[50:])
	// zero absolute [60,70) => buffer indices [10,20)
	for i := 10; i < 20; i++ {
		want[i] = 0
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("offset excluded read mismatch:\n got=%v\nwant=%v", got, want)
	}
}
