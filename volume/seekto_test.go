// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestSeekTo_BufferedFile: SeekTo on a regular file positions the next Read at
// the requested byte (the --input resume path).
func TestSeekTo_BufferedFile(t *testing.T) {
	data := make([]byte, 200*1024)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	path := filepath.Join(t.TempDir(), "src.img")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, off := range []int64{0, 1, 511, 512, 513, 4096, 100_000, 199_999} {
		if err := r.SeekTo(off); err != nil {
			t.Fatalf("SeekTo(%d): %v", off, err)
		}
		if r.Offset() != off {
			t.Fatalf("Offset after SeekTo(%d) = %d", off, r.Offset())
		}
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("ReadAll after SeekTo(%d): %v", off, err)
		}
		if !bytes.Equal(got, data[off:]) {
			t.Fatalf("SeekTo(%d): read %d bytes, want tail of %d", off, len(got), len(data)-int(off))
		}
	}
}

// TestSeekTo_DirectIOUnaligned exercises the sector-alignment arithmetic of the
// direct-I/O path without a real device: a Reader with directIO fields backed
// by a regular file must still deliver the exact byte at an unaligned offset
// (seek to the sector floor, discard the sub-sector remainder).
func TestSeekTo_DirectIOUnaligned(t *testing.T) {
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}
	path := filepath.Join(t.TempDir(), "dev.img")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := &Reader{file: f, size: int64(len(data)), directIO: true, bufferSize: 4096}
	r.initDirectIO()
	defer r.Close()

	for _, off := range []int64{1, 100, 511, 513, 1000, 4097, 60_000} {
		if err := r.SeekTo(off); err != nil {
			t.Fatalf("SeekTo(%d): %v", off, err)
		}
		if r.Offset() != off {
			t.Fatalf("Offset after SeekTo(%d) = %d", off, r.Offset())
		}
		// Read a modest span and compare to the source at that offset.
		buf := make([]byte, 2000)
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Fatalf("read after SeekTo(%d): %v", off, err)
		}
		want := data[off:]
		if len(want) > n {
			want = want[:n]
		}
		if !bytes.Equal(buf[:n], want) {
			t.Fatalf("SeekTo(%d) directIO: bytes mismatch at first diff", off)
		}
	}
}
