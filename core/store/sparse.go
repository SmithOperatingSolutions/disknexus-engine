// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// ExtractFrames rewrites the pack file at packPath so it holds ONLY the
// frames starting at the given offsets, each at its ORIGINAL offset — a
// sparse file whose readable layout is identical to the full pack for
// exactly the frames a caller declared it needs (#522). Retrieve reads it
// unchanged; the unreferenced regions become holes and cost no disk.
//
// offsets must be ascending and name frame starts; keptBytes is the sum of
// the surviving frames (header + payload), which is what the extraction
// actually costs on disk up to filesystem block rounding.
//
// The rewrite goes through a temp file + rename, so a crash mid-extraction
// leaves either the full pack or the finished sparse one — never a pack
// with half a frame, which a later Retrieve would read as corrupt data
// rather than a missing file.
func ExtractFrames(packPath string, offsets []int64) (keptBytes int64, err error) {
	src, err := os.Open(packPath)
	if err != nil {
		return 0, fmt.Errorf("opening pack for extraction: %w", err)
	}
	// Closed EXPLICITLY before the rename below: Windows refuses a rename
	// onto a path with an open handle, and a deferred close runs too late —
	// the deterministic cross-OS trap §6 of docs/TESTING.md names.
	defer func() {
		if src != nil {
			src.Close()
		}
	}()

	tmpPath := packPath + ".extract-tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, fmt.Errorf("creating sparse pack: %w", err)
	}
	defer func() {
		if dst != nil {
			dst.Close()
			os.Remove(tmpPath)
		}
	}()

	var header [8]byte
	var prevEnd int64 = -1
	for _, off := range offsets {
		if off < prevEnd {
			return 0, fmt.Errorf("extraction offsets overlap or are unsorted (offset %d inside the frame ending at %d)", off, prevEnd)
		}
		if _, err := src.ReadAt(header[:], off); err != nil {
			return 0, fmt.Errorf("reading frame header at offset %d: %w", off, err)
		}
		compressedLen := binary.LittleEndian.Uint32(header[0:4])
		if compressedLen > maxChunkFrameLen {
			return 0, fmt.Errorf("frame at offset %d claims %d bytes, exceeds the %d-byte bound: corrupt pack", off, compressedLen, maxChunkFrameLen)
		}
		frameLen := int64(8 + compressedLen)
		if _, err := dst.Seek(off, io.SeekStart); err != nil {
			return 0, fmt.Errorf("seeking sparse pack to %d: %w", off, err)
		}
		if _, err := io.CopyN(dst, io.NewSectionReader(src, off, frameLen), frameLen); err != nil {
			return 0, fmt.Errorf("copying frame at offset %d: %w", off, err)
		}
		keptBytes += frameLen
		prevEnd = off + frameLen
	}
	// Preserve the pack's logical size so a reader that stats the file sees
	// the same extent the full pack had.
	if st, serr := src.Stat(); serr == nil {
		if terr := dst.Truncate(st.Size()); terr != nil {
			return 0, fmt.Errorf("sizing sparse pack: %w", terr)
		}
	}
	if err := dst.Sync(); err != nil {
		return 0, fmt.Errorf("syncing sparse pack: %w", err)
	}
	if err := dst.Close(); err != nil {
		dst = nil
		os.Remove(tmpPath)
		return 0, fmt.Errorf("closing sparse pack: %w", err)
	}
	dst = nil
	if err := src.Close(); err != nil {
		src = nil
		os.Remove(tmpPath)
		return 0, fmt.Errorf("closing pack after extraction: %w", err)
	}
	src = nil
	if err := os.Rename(tmpPath, packPath); err != nil {
		os.Remove(tmpPath)
		return 0, fmt.Errorf("publishing sparse pack: %w", err)
	}
	return keptBytes, nil
}
