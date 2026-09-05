// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package preprocess_test

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
)

// TestPENormalizerFalsePositiveCollision proves that the embedded PE\0\0
// scan in PENormalizer.Normalize zeroes bytes of the hash input with no
// validation that the surrounding bytes are actually a PE header: any
// 50 45 00 00 sequence in ordinary data (expected roughly once per 4 GiB
// of random bytes, so routine at backup scale) triggers it.
//
// Because the pipeline computes chunk identity from the NORMALIZED bytes
// but stores the ORIGINAL bytes (pipeline.SetNormalizer), two genuinely
// different chunks that differ only at the falsely-zeroed offsets get the
// same ChunkID: dedup stores the first chunk's raw data for both, and
// restoring the second file silently yields the first file's bytes.
func TestPENormalizerFalsePositiveCollision(t *testing.T) {
	const size = 4096
	const sigOff = 100 // "PE\0\0" occurs here, embedded in non-PE data

	// Deterministic non-PE payload (does not start with MZ; the only
	// PE\0\0 sequence is the one we embed).
	data1 := make([]byte, size)
	for i := range data1 {
		data1[i] = byte(i*7 + 13)
	}
	copy(data1[sigOff:], []byte{'P', 'E', 0, 0})

	// data2 differs ONLY at the fake "TimeDateStamp" position
	// (sigOff+8 .. sigOff+11) — real, meaningful data in a non-PE file.
	data2 := make([]byte, size)
	copy(data2, data1)
	data2[sigOff+8] ^= 0xFF
	data2[sigOff+9] ^= 0xFF
	data2[sigOff+10] ^= 0xFF
	data2[sigOff+11] ^= 0xFF

	n := &preprocess.PENormalizer{}
	id1 := hasher.Sum(n.Normalize(data1))
	id2 := hasher.Sum(n.Normalize(data2))

	if id1 == id2 {
		t.Fatalf("distinct non-PE chunks produced identical ChunkIDs: dedup will store one chunk's bytes for both, silently corrupting the other file on restore")
	}
}
