// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"encoding/binary"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// buildExtentLeaf encodes ext4 extent leaf entries (12 bytes each):
// ee_block(u32) ee_len(u16) ee_start_hi(u16) ee_start_lo(u32).
func buildExtentLeaf(entries [][3]uint64) []byte {
	buf := make([]byte, len(entries)*12)
	for i, e := range entries {
		off := i * 12
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(e[0]))   // ee_block
		binary.LittleEndian.PutUint16(buf[off+4:off+6], uint16(e[1])) // ee_len
		binary.LittleEndian.PutUint16(buf[off+6:off+8], uint16(e[2]>>32))
		binary.LittleEndian.PutUint32(buf[off+8:off+12], uint32(e[2]))
	}
	return buf
}

// TestParseExtentLeavesSkipsUninitialized proves that an uninitialized
// (fallocated) extent — ee_len > 32768 — is omitted rather than emitted with
// its raw block count as the length. The raw count (e.g. 33024) would (a)
// map preallocated space to stale on-disk bytes (an info leak) and (b) claim
// a huge length that drops the real extents that follow. Omitting it leaves
// the fallocated region as zeros on restore, which is correct (fallocate'd
// but unwritten data reads as zeros).
func TestParseExtentLeavesSkipsUninitialized(t *testing.T) {
	const bs = 4096
	// entry1 is uninitialized: ee_len = 32768 + 256 = 33024.
	leaf := buildExtentLeaf([][3]uint64{
		{0, 1, 100},     // initialized, 1 block @ phys 100
		{1, 33024, 200}, // uninitialized (real 256 blocks) @ phys 200
		{257, 2, 500},   // initialized, 2 blocks @ phys 500
	})

	got := parseExtentLeaves(leaf, 3, bs, 0)

	if len(got) != 2 {
		t.Fatalf("got %d extents, want 2 (uninitialized extent must be omitted): %+v", len(got), got)
	}
	want := []manifest.VolumeExtent{
		{FileOffset: 0, VolumeOffset: 100 * bs, Length: 1 * bs},
		{FileOffset: 257 * bs, VolumeOffset: 500 * bs, Length: 2 * bs},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("extent %d = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestTrimExtentsToSizeWithHole proves that extents are trimmed to the file's
// logical size using each extent's own FileOffset, not a running total. With
// a hole (a gap in FileOffset, e.g. from an omitted uninitialized extent) the
// old cumulative-remaining logic let a trailing extent claim volume bytes
// past EOF.
func TestTrimExtentsToSizeWithHole(t *testing.T) {
	const fileSize = 13000
	extents := []manifest.VolumeExtent{
		{FileOffset: 0, VolumeOffset: 4096, Length: 4096},     // [0,4096)
		{FileOffset: 8192, VolumeOffset: 40960, Length: 8192}, // starts after a hole
	}

	got := trimExtentsToSize(extents, fileSize)

	if len(got) != 2 {
		t.Fatalf("got %d extents, want 2", len(got))
	}
	// The second extent starts at 8192; only 13000-8192 = 4808 bytes are within
	// the file. The old logic (fileSize - sum-of-kept) left it at 8192, past EOF.
	if got[1].Length != fileSize-8192 {
		t.Fatalf("trailing extent length = %d, want %d (must not extend past EOF)", got[1].Length, fileSize-8192)
	}
}
