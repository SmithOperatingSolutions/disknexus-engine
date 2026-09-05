// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker_test

import (
	"bytes"
	"io"
	"math/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/chunker"
)

// TestReset_ContiguousCoverageFromBoundary validates the core assumption behind
// resumable backups (#42 §3.4): re-chunking from a prior chunk boundary B tiles
// the source with no gap and no overlap, even though the first post-B boundary
// may differ from the uninterrupted run (Reset re-seeds a zero rolling-hash
// window). Restore reassembles by VolumeOffset, so contiguous coverage — not
// identical boundaries — is what guarantees a byte-identical restore.
func TestReset_ContiguousCoverageFromBoundary(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	data := make([]byte, 2<<20) // 2 MiB
	rng.Read(data)

	// Full uninterrupted chunking.
	c := chunker.New(bytes.NewReader(data), chunker.WithMinSize(2048), chunker.WithMaxSize(64*1024))
	var full []chunker.Chunk
	for {
		ch, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		full = append(full, ch)
	}
	if len(full) < 4 {
		t.Fatalf("need several chunks to pick an interior boundary, got %d", len(full))
	}

	// Pick an interior boundary B = end of the 2nd chunk.
	B := full[1].Offset + int64(full[1].Length)

	// Prefix = chunks fully before B.
	var prefixEnd int64
	for _, ch := range full {
		if ch.Offset >= B {
			break
		}
		if ch.Offset != prefixEnd {
			t.Fatalf("prefix gap: chunk at %d, expected %d", ch.Offset, prefixEnd)
		}
		prefixEnd = ch.Offset + int64(ch.Length)
	}
	if prefixEnd != B {
		t.Fatalf("prefix does not end exactly at B: prefixEnd=%d B=%d", prefixEnd, B)
	}

	// Resume: re-chunk from B.
	c.Reset(bytes.NewReader(data[B:]), B)
	var suffix []chunker.Chunk
	for {
		ch, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		suffix = append(suffix, ch)
	}
	if len(suffix) == 0 {
		t.Fatal("no suffix chunks")
	}
	if suffix[0].Offset != B {
		t.Fatalf("first resumed chunk Offset = %d, want B=%d", suffix[0].Offset, B)
	}

	// Suffix must tile [B, len(data)) contiguously and reconstruct the tail bytes.
	cur := B
	var rebuilt []byte
	for i, ch := range suffix {
		if ch.Offset != cur {
			t.Fatalf("suffix gap/overlap at chunk %d: Offset %d, expected %d", i, ch.Offset, cur)
		}
		rebuilt = append(rebuilt, ch.Data...)
		cur += int64(ch.Length)
	}
	if cur != int64(len(data)) {
		t.Fatalf("suffix ends at %d, want %d", cur, len(data))
	}
	if !bytes.Equal(rebuilt, data[B:]) {
		t.Fatal("resumed suffix bytes do not reconstruct the source tail")
	}
}
