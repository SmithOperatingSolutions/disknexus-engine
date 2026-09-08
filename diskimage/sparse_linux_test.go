// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"os"
	"syscall"
	"testing"
)

// assertSparse proves the zero regions cost nothing: the blocks the
// filesystem allocated for the image are a small fraction of its size.
// Formats that carry their own allocation are small files already.
func assertSparse(t *testing.T, files []string) {
	t.Helper()
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if st.Size() < 64<<20 {
			continue // VHD dynamic, qcow2, VMDK descriptor: not sparse by nature
		}
		alloc := st.Sys().(*syscall.Stat_t).Blocks * 512
		if alloc > 32<<20 {
			t.Fatalf("%s: %d bytes allocated for ~1 MiB of data in a %d-byte file", f, alloc, st.Size())
		}
	}
}
