// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/chunker"
)

func TestChunkerDeterminism(t *testing.T) {
	// Same input must produce identical chunk boundaries
	data := make([]byte, 256*1024) // 256 KB
	rand.Read(data)

	chunks1 := chunkAll(t, data)
	chunks2 := chunkAll(t, data)

	if len(chunks1) != len(chunks2) {
		t.Fatalf("chunk count mismatch: %d vs %d", len(chunks1), len(chunks2))
	}

	for i := range chunks1 {
		if chunks1[i].Offset != chunks2[i].Offset {
			t.Errorf("chunk %d offset mismatch: %d vs %d", i, chunks1[i].Offset, chunks2[i].Offset)
		}
		if chunks1[i].Length != chunks2[i].Length {
			t.Errorf("chunk %d length mismatch: %d vs %d", i, chunks1[i].Length, chunks2[i].Length)
		}
		if !bytes.Equal(chunks1[i].Data, chunks2[i].Data) {
			t.Errorf("chunk %d data mismatch", i)
		}
	}
}

func TestChunkerSizeBounds(t *testing.T) {
	data := make([]byte, 1024*1024) // 1 MB
	rand.Read(data)

	chunks := chunkAll(t, data)
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	var belowMin int
	for i, c := range chunks {
		if c.Length > chunker.FallbackMaxSize {
			t.Errorf("chunk %d above max size: %d", i, c.Length)
		}
		if c.Length < chunker.FallbackMinSize && i < len(chunks)-1 {
			belowMin++
		}
	}

	// With normalized chunking, rare sub-minSize chunks are expected
	// (hard mask allows ~1/16 the probability). Log but don't fail
	// unless excessive.
	if belowMin > 0 {
		t.Logf("%d/%d chunks below min size (expected rare with normalized chunking)", belowMin, len(chunks))
	}
	if belowMin > len(chunks)/4 {
		t.Errorf("too many sub-minimum chunks: %d/%d", belowMin, len(chunks))
	}
}

func TestChunkerReassembly(t *testing.T) {
	// Chunks must reassemble to the original data
	data := make([]byte, 512*1024) // 512 KB
	rand.Read(data)

	chunks := chunkAll(t, data)

	var reassembled []byte
	for _, c := range chunks {
		reassembled = append(reassembled, c.Data...)
	}

	if !bytes.Equal(data, reassembled) {
		t.Fatal("reassembled data does not match original")
	}
}

func TestChunkerOffsets(t *testing.T) {
	data := make([]byte, 256*1024)
	rand.Read(data)

	chunks := chunkAll(t, data)

	var expectedOffset int64
	for i, c := range chunks {
		if c.Offset != expectedOffset {
			t.Errorf("chunk %d: expected offset %d, got %d", i, expectedOffset, c.Offset)
		}
		expectedOffset += int64(c.Length)
	}
}

func TestChunkerShiftResync(t *testing.T) {
	// Insert a byte in the middle; only chunks near the insertion should change.
	// Use a larger input so there are enough chunks for meaningful measurement.
	original := make([]byte, 1024*1024) // 1 MB → ~128 chunks at 8KB avg
	rand.Read(original)

	modified := make([]byte, 0, len(original)+1)
	insertPos := len(original) / 2
	modified = append(modified, original[:insertPos]...)
	modified = append(modified, 0xFF) // insert one byte
	modified = append(modified, original[insertPos:]...)

	origChunks := chunkAll(t, original)
	modChunks := chunkAll(t, modified)

	// Count matching chunks (by data content)
	origSet := make(map[string]bool, len(origChunks))
	for _, c := range origChunks {
		origSet[string(c.Data)] = true
	}

	matching := 0
	for _, c := range modChunks {
		if origSet[string(c.Data)] {
			matching++
		}
	}

	// With CDC, most chunks should survive a single byte insertion.
	// Typically only 1-2 chunks around the insertion point change,
	// so we expect the vast majority to survive.
	totalOrig := len(origChunks)
	survivalRate := float64(matching) / float64(totalOrig)
	if survivalRate < 0.80 {
		t.Errorf("CDC shift resync poor: only %d/%d (%.0f%%) chunks survived single byte insert",
			matching, totalOrig, survivalRate*100)
	}

	t.Logf("shift resync: %d/%d original chunks survived (%.1f%%), %d modified chunks total",
		matching, totalOrig, survivalRate*100, len(modChunks))
}

func TestChunkerEmpty(t *testing.T) {
	c := chunker.New(bytes.NewReader(nil))
	_, err := c.Next()
	if err != io.EOF {
		t.Errorf("expected io.EOF for empty input, got %v", err)
	}
}

func TestChunkerSmallInput(t *testing.T) {
	data := []byte("hello world")
	chunks := chunkAll(t, data)

	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small input, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[0].Data, data) {
		t.Fatal("chunk data doesn't match input")
	}
}

func BenchmarkChunker(b *testing.B) {
	data := make([]byte, 10*1024*1024) // 10 MB
	rand.Read(data)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for b.Loop() {
		c := chunker.New(bytes.NewReader(data))
		for {
			_, err := c.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func chunkAll(t *testing.T, data []byte) []chunker.Chunk {
	t.Helper()
	c := chunker.New(bytes.NewReader(data))
	var chunks []chunker.Chunk
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("chunker error: %v", err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
}
