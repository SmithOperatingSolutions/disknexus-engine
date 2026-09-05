// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package hasher_test

import (
	"crypto/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

func TestSumDeterministic(t *testing.T) {
	data := make([]byte, 8192)
	rand.Read(data)

	id1 := hasher.Sum(data)
	id2 := hasher.Sum(data)

	if id1.WeakHash != id2.WeakHash {
		t.Error("weak hash not deterministic")
	}
	if id1.StrongHash != id2.StrongHash {
		t.Error("strong hash not deterministic")
	}
}

func TestSumDifferentData(t *testing.T) {
	data1 := make([]byte, 8192)
	data2 := make([]byte, 8192)
	rand.Read(data1)
	rand.Read(data2)

	id1 := hasher.Sum(data1)
	id2 := hasher.Sum(data2)

	if id1.StrongHash == id2.StrongHash {
		t.Error("different data produced same strong hash")
	}
}

func TestPrefix8(t *testing.T) {
	data := []byte("test data for prefix")
	id := hasher.Sum(data)

	prefix := id.Prefix8()
	if prefix == 0 {
		t.Error("prefix8 should not be zero for non-trivial data")
	}

	// Verify it's built from the first 8 bytes of SHA-256
	expected := uint64(id.StrongHash[0])<<56 |
		uint64(id.StrongHash[1])<<48 |
		uint64(id.StrongHash[2])<<40 |
		uint64(id.StrongHash[3])<<32 |
		uint64(id.StrongHash[4])<<24 |
		uint64(id.StrongHash[5])<<16 |
		uint64(id.StrongHash[6])<<8 |
		uint64(id.StrongHash[7])

	if prefix != expected {
		t.Errorf("prefix8 mismatch: got %x, want %x", prefix, expected)
	}
}

func BenchmarkSum(b *testing.B) {
	data := make([]byte, 8192) // typical chunk size
	rand.Read(data)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		hasher.Sum(data)
	}
}
