// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker

import (
	"testing"
)

// TestBuzhashResync verifies that the Buzhash rolling hash resyncs after
// WindowSize bytes following a single byte insertion. This is the fundamental
// property that enables CDC's shift-resync behavior.
func TestBuzhashResync(t *testing.T) {
	original := make([]byte, 512)
	for i := range original {
		original[i] = byte((i*7 + 13) & 0xFF)
	}

	insertPos := 200
	modified := make([]byte, 0, len(original)+1)
	modified = append(modified, original[:insertPos]...)
	modified = append(modified, 0xFF)
	modified = append(modified, original[insertPos:]...)

	origHashes := computeAllHashes(original)
	modHashes := computeAllHashes(modified)

	// Before insertion: hashes must be identical
	for i := 0; i < insertPos; i++ {
		if origHashes[i] != modHashes[i] {
			t.Fatalf("pre-insert hash mismatch at position %d", i)
		}
	}
	t.Logf("pre-insert: all %d hashes match", insertPos)

	// After insertion + WindowSize: hashes should match (shifted by 1)
	resyncStart := insertPos + WindowSize
	mismatches := 0

	for i := resyncStart; i < len(original); i++ {
		modIdx := i + 1
		if modIdx >= len(modHashes) {
			break
		}
		if origHashes[i] != modHashes[modIdx] {
			mismatches++
			if mismatches <= 3 {
				t.Errorf("post-resync hash mismatch at orig[%d] vs mod[%d]: %016x vs %016x",
					i, modIdx, origHashes[i], modHashes[modIdx])
			}
		}
	}

	if mismatches > 0 {
		t.Errorf("%d hash mismatches after expected resync point", mismatches)
	} else {
		t.Logf("Buzhash resyncs correctly after WindowSize=%d bytes past insertion", WindowSize)
	}
}

// TestBuzhashSameWindow verifies that two streams with identical last WindowSize
// bytes produce the same hash, and that the incremental hash matches direct computation.
func TestBuzhashSameWindow(t *testing.T) {
	tail := make([]byte, WindowSize)
	for i := range tail {
		tail[i] = byte(i*13 + 7)
	}

	stream1 := append([]byte{10, 20, 30, 40, 50}, tail...)
	stream2 := append([]byte{99, 88, 77, 66, 55, 44, 33}, tail...)

	h1 := computeAllHashes(stream1)
	h2 := computeAllHashes(stream2)

	last1 := h1[len(h1)-1]
	last2 := h2[len(h2)-1]

	if last1 != last2 {
		t.Errorf("same last %d bytes but different hash: %016x vs %016x",
			WindowSize, last1, last2)
	}

	directHash := computeHashDirect(tail)
	if last1 != directHash {
		t.Errorf("incremental hash %016x != direct hash %016x", last1, directHash)
	}
}

// computeHashDirect computes the Buzhash for a window without incremental updates.
func computeHashDirect(window []byte) uint64 {
	n := len(window)
	var hash uint64
	for i := 0; i < n; i++ {
		hash ^= rotateLeft(buzhashTable[window[i]], n-1-i)
	}
	return hash
}

func computeAllHashes(data []byte) []uint64 {
	var window [WindowSize]byte
	var windowPos int
	hash := zeroWindowHash

	hashes := make([]uint64, len(data))
	for i, b := range data {
		outByte := window[windowPos]
		window[windowPos] = b
		windowPos = (windowPos + 1) % WindowSize
		hash = rotateLeft(hash, 1) ^
			rotateLeft(buzhashTable[outByte], WindowSize) ^
			buzhashTable[b]
		hashes[i] = hash
	}
	return hashes
}
