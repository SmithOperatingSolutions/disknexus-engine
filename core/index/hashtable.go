// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// errTableFull is returned by diskHashTable.Insert when the load factor
// exceeds 85%.
var errTableFull = errors.New("hash table full")

const htabHeaderSize = 24

// htabMagic identifies a valid .htab file.
var htabMagic = [8]byte{'D', 'N', 'H', 'T', 'A', 'B', 0x01, 0x00}

// diskHashTable is an open-addressed hash table with linear probing stored on
// disk. Entries are the 48-byte IndexEntry type. Empty slots are identified by
// an all-zero StrongHash (SHA-256 can never be all-zeros for real data).
//
// File layout:
//
//	[0:8]   magic "DNHTAB\x01\x00" (8 bytes)
//	[8:16]  numSlots (uint64 LE)
//	[16:24] count (uint64 LE, written at Close)
//	[24:]   slots — numSlots × EntrySize bytes
type diskHashTable struct {
	f        *os.File
	numSlots int64
	count    int64 // in-memory; flushed to header at Close
}

// createDiskHashTable creates a new hash table file at path with numSlots
// pre-allocated slots (all zeros = empty). The caller is responsible for
// removing the file on error after this call returns.
func createDiskHashTable(path string, numSlots int64) (*diskHashTable, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating hash table %s: %w", path, err)
	}
	hdr := make([]byte, htabHeaderSize)
	copy(hdr[0:8], htabMagic[:])
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(numSlots))
	binary.LittleEndian.PutUint64(hdr[16:24], 0)
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("writing hash table header: %w", err)
	}
	totalSize := int64(htabHeaderSize) + numSlots*EntrySize
	if err := f.Truncate(totalSize); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("truncating hash table to %d bytes: %w", totalSize, err)
	}
	return &diskHashTable{f: f, numSlots: numSlots}, nil
}

// openDiskHashTable opens an existing hash table file, validates the magic,
// and reads the header fields.
func openDiskHashTable(path string) (*diskHashTable, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening hash table: %w", err)
	}
	hdr := make([]byte, htabHeaderSize)
	if _, err := fileReadAt(f, hdr, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("reading hash table header: %w", err)
	}
	var magic [8]byte
	copy(magic[:], hdr[0:8])
	if magic != htabMagic {
		f.Close()
		return nil, fmt.Errorf("invalid hash table magic bytes")
	}
	numSlots := int64(binary.LittleEndian.Uint64(hdr[8:16]))
	count := int64(binary.LittleEndian.Uint64(hdr[16:24]))
	// A corrupt header with numSlots <= 0 would make Insert's modulo
	// (hash % numSlots) an integer divide-by-zero panic. Reject it at open so
	// neither Insert nor Lookup can be reached with a zero-slot table.
	if numSlots <= 0 {
		f.Close()
		return nil, fmt.Errorf("corrupt hash table: numSlots=%d", numSlots)
	}
	return &diskHashTable{f: f, numSlots: numSlots, count: count}, nil
}

func (ht *diskHashTable) slotOffset(slotIdx int64) int64 {
	return int64(htabHeaderSize) + slotIdx*EntrySize
}

// Lookup searches for strongHash using linear probing.
// Returns the entry and true if found; nil and false if the slot chain ends
// at an empty slot.
func (ht *diskHashTable) Lookup(strongHash [32]byte) (*IndexEntry, bool, error) {
	if ht.numSlots == 0 {
		return nil, false, nil
	}
	start := int64(hashPrefix8(strongHash) % uint64(ht.numSlots))
	buf := make([]byte, EntrySize)
	var zero [32]byte
	for probe := int64(0); probe < ht.numSlots; probe++ {
		slotIdx := (start + probe) % ht.numSlots
		if _, err := fileReadAt(ht.f, buf, ht.slotOffset(slotIdx)); err != nil {
			return nil, false, fmt.Errorf("reading slot %d: %w", slotIdx, err)
		}
		var e IndexEntry
		decodeEntry(buf, &e)
		if e.StrongHash == zero {
			// Empty slot — not in table.
			return nil, false, nil
		}
		if e.StrongHash == strongHash {
			return &e, true, nil
		}
	}
	return nil, false, nil
}

// Insert writes entry into the table using linear probing.
// Returns errTableFull when count exceeds 85% of numSlots.
// Inserting an already-present hash is a no-op (idempotent).
func (ht *diskHashTable) Insert(entry IndexEntry) error {
	if ht.numSlots <= 0 {
		return fmt.Errorf("corrupt hash table: numSlots=%d", ht.numSlots)
	}
	if ht.count > ht.numSlots*85/100 {
		return errTableFull
	}
	start := int64(hashPrefix8(entry.StrongHash) % uint64(ht.numSlots))
	buf := make([]byte, EntrySize)
	var zero [32]byte
	for probe := int64(0); probe < ht.numSlots; probe++ {
		slotIdx := (start + probe) % ht.numSlots
		if _, err := fileReadAt(ht.f, buf, ht.slotOffset(slotIdx)); err != nil {
			return fmt.Errorf("reading slot for insert: %w", err)
		}
		var e IndexEntry
		decodeEntry(buf, &e)
		if e.StrongHash == zero {
			// Empty slot — write here.
			encodeEntry(&entry, buf)
			if _, err := fileWriteAt(ht.f, buf, ht.slotOffset(slotIdx)); err != nil {
				return fmt.Errorf("writing slot: %w", err)
			}
			ht.count++
			return nil
		}
		if e.StrongHash == entry.StrongHash {
			// Already present — update metadata in case the chunk moved.
			if e.PackNumber != entry.PackNumber || e.StoreOffset != entry.StoreOffset || e.ChunkLength != entry.ChunkLength {
				encodeEntry(&entry, buf)
				if _, err := fileWriteAt(ht.f, buf, ht.slotOffset(slotIdx)); err != nil {
					return fmt.Errorf("updating slot: %w", err)
				}
			}
			return nil
		}
	}
	return errTableFull
}

// ReadAll scans all slots sequentially and returns non-empty entries.
func (ht *diskHashTable) ReadAll() ([]IndexEntry, error) {
	result := make([]IndexEntry, 0, ht.count)
	buf := make([]byte, EntrySize)
	var zero [32]byte
	for i := int64(0); i < ht.numSlots; i++ {
		if _, err := fileReadAt(ht.f, buf, ht.slotOffset(i)); err != nil {
			return nil, fmt.Errorf("reading slot %d: %w", i, err)
		}
		var e IndexEntry
		decodeEntry(buf, &e)
		if e.StrongHash != zero {
			result = append(result, e)
		}
	}
	return result, nil
}

// Close writes the in-memory count to the file header and closes the file.
func (ht *diskHashTable) Close() error {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(ht.count))
	if _, err := fileWriteAt(ht.f, buf[:], 16); err != nil {
		ht.f.Close()
		return fmt.Errorf("writing count to hash table header: %w", err)
	}
	return ht.f.Close()
}
