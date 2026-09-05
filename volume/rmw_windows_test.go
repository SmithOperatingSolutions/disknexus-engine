//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAlignedWriteAtFailsOnPreReadError guards issue #16: the read-modify-write
// slow path discarded the boundary-sector pre-read error. On a write-only
// handle (or a device read failure) the buffer stayed zeroed and the write
// destroyed the neighboring bytes sharing the boundary sectors. The pre-read
// error must abort the write.
func TestAlignedWriteAtFailsOnPreReadError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.img")
	// Seed two sectors of 0xFF so a zero-filled RMW would visibly destroy data.
	seed := make([]byte, 2*sectorSize)
	for i := range seed {
		seed[i] = 0xFF
	}
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	// Write-only handle: ReadAt fails with access denied, exactly the
	// ignored-pre-read failure mode.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Unaligned write forces the RMW slow path.
	if _, err := alignedWriteAt(f, []byte{0x11}, 10); err == nil {
		t.Fatal("alignedWriteAt succeeded despite the pre-read failing; the RMW would have zeroed the rest of the boundary sectors")
	}

	// The seeded data must be intact.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range got {
		if b != 0xFF {
			t.Fatalf("byte %d corrupted to %#x; RMW proceeded after a failed pre-read", i, b)
		}
	}
}
