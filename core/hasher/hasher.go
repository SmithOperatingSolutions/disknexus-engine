// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package hasher

import (
	"crypto/sha256"

	"github.com/cespare/xxhash/v2"
)

// ChunkID uniquely identifies a chunk via dual hashing.
type ChunkID struct {
	WeakHash   uint64   // xxHash of (normalized) data — for bloom filter
	StrongHash [32]byte // SHA-256 of (normalized) data — authoritative identity
}

// Sum computes both xxHash and SHA-256 for the given data.
func Sum(data []byte) ChunkID {
	return ChunkID{
		WeakHash:   xxhash.Sum64(data),
		StrongHash: sha256.Sum256(data),
	}
}

// StrongHashBytes returns the strong hash as a byte slice.
func (id ChunkID) StrongHashBytes() []byte {
	return id.StrongHash[:]
}

// Prefix8 returns the first 8 bytes of the strong hash for index sorting.
func (id ChunkID) Prefix8() uint64 {
	return uint64(id.StrongHash[0])<<56 |
		uint64(id.StrongHash[1])<<48 |
		uint64(id.StrongHash[2])<<40 |
		uint64(id.StrongHash[3])<<32 |
		uint64(id.StrongHash[4])<<24 |
		uint64(id.StrongHash[5])<<16 |
		uint64(id.StrongHash[6])<<8 |
		uint64(id.StrongHash[7])
}
