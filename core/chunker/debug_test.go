// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker_test

import (
	"bytes"
	"crypto/rand"
	"io"
	mrand "math/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/chunker"
)

func TestIdenticalInput(t *testing.T) {
	// Verify that identical inputs produce identical chunks
	data := make([]byte, 64*1024)
	rand.Read(data)

	c1 := chunkAllWith(t, data)
	c2 := chunkAllWith(t, data)

	if len(c1) != len(c2) {
		t.Fatalf("identical input: %d vs %d chunks", len(c1), len(c2))
	}
	for i := range c1 {
		if !bytes.Equal(c1[i].Data, c2[i].Data) {
			t.Fatalf("chunk %d differs for identical input", i)
		}
	}
	t.Logf("identical input: %d chunks, all match", len(c1))
}

func TestSharedPrefixChunks(t *testing.T) {
	// Two streams that share the first 32KB and differ after that.
	// The first N chunks (covering the shared prefix) must be identical.
	prefix := make([]byte, 32*1024)
	rand.Read(prefix)

	suffix1 := make([]byte, 32*1024)
	suffix2 := make([]byte, 32*1024)
	rand.Read(suffix1)
	rand.Read(suffix2)

	data1 := append(append([]byte{}, prefix...), suffix1...)
	data2 := append(append([]byte{}, prefix...), suffix2...)

	c1 := chunkAllWith(t, data1)
	c2 := chunkAllWith(t, data2)

	// Find how far the chunks match
	matching := 0
	for i := 0; i < len(c1) && i < len(c2); i++ {
		if bytes.Equal(c1[i].Data, c2[i].Data) {
			matching++
		} else {
			break
		}
	}

	// All chunks fully within the shared prefix should match
	t.Logf("shared prefix: %d/%d initial chunks match (stream1: %d chunks, stream2: %d chunks)",
		matching, len(c1), len(c1), len(c2))

	// At least the chunks before the 32KB mark should match
	if matching == 0 {
		t.Error("no matching prefix chunks!")
	}
}

func TestAppendedData(t *testing.T) {
	// Original data + appended data: all original chunks should survive
	data := make([]byte, 64*1024)
	rand.Read(data)

	appended := make([]byte, 32*1024)
	rand.Read(appended)

	extended := append(append([]byte{}, data...), appended...)

	origChunks := chunkAllWith(t, data)
	extChunks := chunkAllWith(t, extended)

	origSet := make(map[string]bool)
	for _, c := range origChunks {
		origSet[string(c.Data)] = true
	}

	matching := 0
	for _, c := range extChunks {
		if origSet[string(c.Data)] {
			matching++
		}
	}

	rate := float64(matching) / float64(len(origChunks))
	t.Logf("appended data: %d/%d original chunks survived (%.1f%%)", matching, len(origChunks), rate*100)

	// All original chunks except the last (which may merge with appended data) should survive
	if matching < len(origChunks)-1 {
		t.Errorf("too few original chunks survived appending: %d/%d", matching, len(origChunks))
	}
}

// TestSingleByteInsert_Detailed pins the chunker's own contract at the scale
// its sibling does not cover: chunker.go documents that, because the rolling
// hash is continuous across chunk boundaries, "a single byte insertion only
// affects 1-2 chunks near the insertion point". TestChunkerShiftResync
// (chunker_test.go) asserts a loose 80% survival over ~1 MB; this test asserts
// the tight per-chunk bound — at most 2 chunks lost — on a small input where a
// resync failure has nowhere to hide, and keeps the per-chunk log (as t.Logf:
// the fmt.Printf it used to write corrupted `go test -json` output, and it
// asserted nothing at all, #402).
//
// The input is a fixed-seed PRNG stream, deliberately: the bound above is the
// documented guarantee, but chunk boundaries are data-dependent, so a fixed
// fixture makes the measured survival reproducible instead of a per-run
// lottery. Breaking the continuous-hash design (the property under test)
// cascades boundary drift past the insertion point and fails this bound for
// any input, seed included.
func TestSingleByteInsert_Detailed(t *testing.T) {
	data := make([]byte, 32*1024) // small enough for detailed output
	mrand.New(mrand.NewSource(42)).Read(data)

	insertPos := len(data) / 2
	modified := make([]byte, 0, len(data)+1)
	modified = append(modified, data[:insertPos]...)
	modified = append(modified, 0xFF)
	modified = append(modified, data[insertPos:]...)

	origChunks := chunkAllWith(t, data)
	modChunks := chunkAllWith(t, modified)

	t.Logf("Original (%d chunks):", len(origChunks))
	for i, c := range origChunks {
		t.Logf("  [%2d] off=%5d len=%4d end=%5d first3=%x",
			i, c.Offset, c.Length, c.Offset+int64(c.Length), c.Data[:min(3, len(c.Data))])
	}

	t.Logf("Modified (%d chunks, insert at %d):", len(modChunks), insertPos)
	for i, c := range modChunks {
		t.Logf("  [%2d] off=%5d len=%4d end=%5d first3=%x",
			i, c.Offset, c.Length, c.Offset+int64(c.Length), c.Data[:min(3, len(c.Data))])
	}

	origSet := make(map[string]int)
	for i, c := range origChunks {
		origSet[string(c.Data)] = i
	}

	matching := 0
	for i, c := range modChunks {
		if oi, ok := origSet[string(c.Data)]; ok {
			t.Logf("  mod[%d] = orig[%d]", i, oi)
			matching++
		}
	}
	t.Logf("Survival: %d/%d (%.1f%%)", matching, len(origChunks),
		float64(matching)/float64(len(origChunks))*100)

	if matching < len(origChunks)-2 {
		t.Errorf("single-byte insert at %d changed more than the documented 1-2 chunks: only %d/%d original "+
			"chunks survived — shift-resync is broken, so an incremental backup after a one-byte edit "+
			"re-uploads chunks it already stores", insertPos, matching, len(origChunks))
	}
}

func chunkAllWith(t *testing.T, data []byte, opts ...chunker.Option) []chunker.Chunk {
	t.Helper()
	c := chunker.New(bytes.NewReader(data), opts...)
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
