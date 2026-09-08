// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
)

// A disk that is not a whole number of blocks or clusters: 3 MiB plus one
// page. The VHD's last block is partial, the qcow2's last cluster is
// partial, and the last page of the disk is written and read back. Reads
// past the end are short, never wrong.
func TestOddSizeEveryFormat(t *testing.T) {
	const size = int64(3<<20 + 4096)
	head := bytes.Repeat([]byte{0x11}, 512)
	tail := bytes.Repeat([]byte{0xEE}, 4096)
	mid := bytes.Repeat([]byte{0x77}, 4096)
	for _, tc := range []struct {
		name string
		f    Format
		opts Options
	}{{"raw", Raw, Options{}}, {"vhd", VHD, Options{}}, {"vhd-fixed", VHD, Options{Fixed: true}}, {"qcow2", QCOW2, Options{}}, {"vmdk", VMDK, Options{}}} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d"+tc.f.Extension())
			w, err := Create(tc.f, path, size, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			for _, wr := range []struct {
				off int64
				b   []byte
			}{{0, head}, {2<<20 + 512, mid}, {size - 4096, tail}} {
				if _, err := w.WriteAt(wr.b, wr.off); err != nil {
					t.Fatalf("WriteAt(%d): %v", wr.off, err)
				}
			}
			if _, err := w.WriteAt([]byte{1}, size-4095); err != nil {
				t.Fatalf("write inside the last page: %v", err)
			}
			if _, err := w.WriteAt([]byte{1, 2}, size-1); err == nil {
				t.Fatal("a write crossing the end was accepted")
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			r, err := Open(tc.f, path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if r.Size() != size {
				t.Fatalf("size %d, want %d", r.Size(), size)
			}
			got := make([]byte, size)
			if _, err := r.ReadAt(got, 0); err != nil && err != io.EOF {
				t.Fatal(err)
			}
			want := make([]byte, size)
			copy(want, head)
			copy(want[2<<20+512:], mid)
			copy(want[size-4096:], tail)
			want[size-4095] = 1
			if !bytes.Equal(got, want) {
				for i := range got {
					if got[i] != want[i] {
						t.Fatalf("byte %d: got %02x want %02x", i, got[i], want[i])
					}
				}
			}
			// A read straddling the end is short with EOF, and a read past it fails.
			buf := make([]byte, 8192)
			n, err := r.ReadAt(buf, size-4096)
			if n != 4096 || err != io.EOF {
				t.Fatalf("straddling read: n=%d err=%v", n, err)
			}
			if _, err := r.ReadAt(buf, size+1); err == nil {
				t.Fatal("a read past the end succeeded")
			}
			if tc.f == QCOW2 {
				if err := r.(bounded).Reader.(*qcow2Reader).checkRefcounts(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
