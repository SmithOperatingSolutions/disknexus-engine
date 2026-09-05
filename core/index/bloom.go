// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sync"
)

const bloomNumHash = 7 // Number of hash functions

// BloomFilter is an in-memory probabilistic set for fast negative lookups.
// Thread-safe for concurrent Add and MayContain calls.
type BloomFilter struct {
	mu      sync.RWMutex
	bits    []uint64
	numBits uint64
	count   uint64
}

// NewBloomFilter creates a bloom filter sized for the expected number of
// elements at the given false positive rate.
// BloomSizeBytes is the resident size NewBloomFilter would allocate for the
// given expectation — the heap gate's projection input (#507).
func BloomSizeBytes(expectedItems uint64, fpRate float64) int64 {
	if expectedItems == 0 {
		expectedItems = 1
	}
	numBits := uint64(math.Ceil(-float64(expectedItems) * math.Log(fpRate) / (math.Ln2 * math.Ln2)))
	numBits = ((numBits + 63) / 64) * 64
	if numBits == 0 {
		numBits = 64
	}
	return int64(numBits / 8)
}

func NewBloomFilter(expectedItems uint64, fpRate float64) *BloomFilter {
	if expectedItems == 0 {
		expectedItems = 1
	}
	// m = -n * ln(p) / (ln(2)^2)
	numBits := uint64(math.Ceil(-float64(expectedItems) * math.Log(fpRate) / (math.Ln2 * math.Ln2)))
	// Round up to nearest 64 for word alignment
	numBits = ((numBits + 63) / 64) * 64
	if numBits == 0 {
		numBits = 64
	}

	return &BloomFilter{
		bits:    make([]uint64, numBits/64),
		numBits: numBits,
	}
}

// BloomBytes is a filter's resident bit-array size — the FoldPassObserver's
// second ground-truth number.
func BloomBytes(bf *BloomFilter) int64 { return int64(len(bf.bits)) * 8 }

// Add inserts a weak hash into the bloom filter.
func (bf *BloomFilter) Add(weakHash uint64) {
	bf.mu.Lock()
	defer bf.mu.Unlock()

	for i := 0; i < bloomNumHash; i++ {
		pos := bf.hashPosition(weakHash, i)
		bf.bits[pos/64] |= 1 << (pos % 64)
	}
	bf.count++
}

// MayContain returns true if the hash might be in the set.
// False means definitely not in the set.
func (bf *BloomFilter) MayContain(weakHash uint64) bool {
	bf.mu.RLock()
	defer bf.mu.RUnlock()

	for i := 0; i < bloomNumHash; i++ {
		pos := bf.hashPosition(weakHash, i)
		if bf.bits[pos/64]&(1<<(pos%64)) == 0 {
			return false
		}
	}
	return true
}

// Count returns the number of items added.
func (bf *BloomFilter) Count() uint64 {
	bf.mu.RLock()
	defer bf.mu.RUnlock()
	return bf.count
}

// SizeBytes returns the memory footprint of the bit array in bytes.
func (bf *BloomFilter) SizeBytes() uint64 {
	return uint64(len(bf.bits)) * 8
}

// hashPosition computes the bit position for the i-th hash function.
// Uses enhanced double hashing with a mix function for better distribution.
func (bf *BloomFilter) hashPosition(weakHash uint64, i int) uint64 {
	// Derive two independent hashes via mixing
	h1 := weakHash
	h2 := mix64(weakHash)
	// Enhanced double hashing: h(i) = (h1 + i*h2 + i*i) mod m
	return (h1 + uint64(i)*h2 + uint64(i)*uint64(i)) % bf.numBits
}

// mix64 is a bijective mixer (splitmix64 finalizer) for deriving h2 from h1.
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// Save serializes the bloom filter to a file.
func (bf *BloomFilter) Save(path string) error {
	bf.mu.RLock()
	defer bf.mu.RUnlock()

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating bloom file: %w", err)
	}
	defer f.Close()

	// Header: numBits (8 bytes) + count (8 bytes)
	header := make([]byte, 16)
	binary.LittleEndian.PutUint64(header[0:8], bf.numBits)
	binary.LittleEndian.PutUint64(header[8:16], bf.count)
	if _, err := f.Write(header); err != nil {
		return fmt.Errorf("writing bloom header: %w", err)
	}

	// Bit array
	buf := make([]byte, 8)
	for _, word := range bf.bits {
		binary.LittleEndian.PutUint64(buf, word)
		if _, err := f.Write(buf); err != nil {
			return fmt.Errorf("writing bloom data: %w", err)
		}
	}

	return nil
}

// LoadBloomFilter loads a previously saved bloom filter from disk.
func LoadBloomFilter(path string) (*BloomFilter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading bloom file: %w", err)
	}
	if len(data) < 16 {
		return nil, fmt.Errorf("bloom file too small: %d bytes", len(data))
	}

	numBits := binary.LittleEndian.Uint64(data[0:8])
	count := binary.LittleEndian.Uint64(data[8:16])

	// numBits must be a positive multiple of 64: the filter stores one uint64
	// word per 64 bits, and MayContain/Add index by (hash % numBits). A zero or
	// non-multiple value would pass the floor-division size check below yet make
	// those operations panic (divide-by-zero or out-of-range word index).
	if numBits == 0 || numBits%64 != 0 {
		return nil, fmt.Errorf("corrupt bloom file: numBits=%d is not a positive multiple of 64", numBits)
	}

	expectedSize := 16 + (numBits/64)*8
	if uint64(len(data)) != expectedSize {
		return nil, fmt.Errorf("bloom file size mismatch: got %d, expected %d", len(data), expectedSize)
	}

	bits := make([]uint64, numBits/64)
	for i := range bits {
		offset := 16 + i*8
		bits[i] = binary.LittleEndian.Uint64(data[offset : offset+8])
	}

	return &BloomFilter{
		bits:    bits,
		numBits: numBits,
		count:   count,
	}, nil
}
