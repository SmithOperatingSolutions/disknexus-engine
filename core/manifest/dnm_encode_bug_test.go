// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"bytes"
	"testing"
	"time"
)

// TestFileEntryExtentCountOverflow proves that encodeFileEntry writes the
// extent count as uint16 while still writing every extent: at 65536
// extents the prefix wraps to 0, and decodeFileEntry silently drops all
// of them (misreading the first extent's bytes as the InlineData length).
// Heavily fragmented files on NTFS can exceed 65535 extents, so a backup
// catalog round-trip loses the file's entire extent map with no error.
func TestFileEntryExtentCountOverflow(t *testing.T) {
	const extentCount = 65536

	fe := FileEntry{
		Path: "vm/disk.vhdx",
		Size: int64(extentCount) * 4096,
	}
	fe.VolumeExtents = make([]VolumeExtent, extentCount)
	for i := range fe.VolumeExtents {
		fe.VolumeExtents[i] = VolumeExtent{
			FileOffset:   int64(i) * 4096,
			VolumeOffset: int64(i) * 8192,
			Length:       4096,
		}
	}

	decoded, err := decodeFileEntry(encodeFileEntry(fe))
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}

	if got := len(decoded.VolumeExtents); got != extentCount {
		t.Fatalf("extents lost in encode/decode round-trip: got %d, want %d", got, extentCount)
	}
}

// TestFileEntryInlineDataOverflow proves the same uint16 wrap for
// InlineData: a 64 KiB payload encodes with a length prefix of 0, so the
// decoder returns the entry with no inline content — the file's data is
// silently lost from the catalog and restore writes an empty file.
func TestFileEntryInlineDataOverflow(t *testing.T) {
	inline := bytes.Repeat([]byte{0xAB}, 65536)

	fe := FileEntry{
		Path:       "resident/file.bin",
		Size:       int64(len(inline)),
		InlineData: inline,
	}

	decoded, err := decodeFileEntry(encodeFileEntry(fe))
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}

	if !bytes.Equal(decoded.InlineData, inline) {
		t.Fatalf("inline data lost in encode/decode round-trip: got %d bytes, want %d", len(decoded.InlineData), len(inline))
	}
}

// TestFileEntryEpochModTimeRoundTrip proves that a legitimate mtime of
// exactly the Unix epoch (common on files extracted from archives or
// container images) does not survive a round-trip: the writer encodes it
// as 0 and the reader treats 0 as "no timestamp", leaving a zero
// time.Time, so restore stamps the wrong mtime.
func TestFileEntryEpochModTimeRoundTrip(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()

	fe := FileEntry{
		Path:    "etc/config",
		ModTime: epoch,
	}

	decoded, err := decodeFileEntry(encodeFileEntry(fe))
	if err != nil {
		t.Fatalf("decodeFileEntry: %v", err)
	}

	if !decoded.ModTime.Equal(epoch) {
		t.Fatalf("epoch mtime lost in round-trip: got %v (IsZero=%v), want %v", decoded.ModTime, decoded.ModTime.IsZero(), epoch)
	}
}
