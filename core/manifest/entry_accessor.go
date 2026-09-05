// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// entry_accessor.go — EntryAccessor interface and implementations for
// memory-efficient access to backup entry records.
//
// Two concrete implementations:
//   - sliceEntryAccessor: wraps []Entry (in-memory, no I/O)
//   - dnmEntryAccessor: wraps *DNMReader (O(1) seek per At, one read for Range)
//
// SearchEntries and SearchEntriesEnd provide binary-search helpers that work
// with any EntryAccessor, replacing sort.Search call sites.

import "fmt"

// EntryAccessor provides indexed access to Entry records without requiring
// all entries to be loaded into memory at once.
type EntryAccessor interface {
	// Count returns the total number of entries.
	Count() int64
	// At returns the entry at index i. O(1) seek for DNM, O(1) for slice.
	At(i int64) (Entry, error)
	// Range returns entries [start, end). One sequential read for DNM.
	Range(start, end int64) ([]Entry, error)
}

// ---------------------------------------------------------------------------
// sliceEntryAccessor
// ---------------------------------------------------------------------------

type sliceEntryAccessor struct {
	entries []Entry
}

// NewSliceEntryAccessor wraps an already-loaded []Entry slice. Range returns
// a sub-slice with no copy.
func NewSliceEntryAccessor(entries []Entry) EntryAccessor {
	return &sliceEntryAccessor{entries: entries}
}

func (s *sliceEntryAccessor) Count() int64 {
	return int64(len(s.entries))
}

func (s *sliceEntryAccessor) At(i int64) (Entry, error) {
	if i < 0 || i >= int64(len(s.entries)) {
		return Entry{}, fmt.Errorf("entry index %d out of range [0, %d)", i, len(s.entries))
	}
	return s.entries[i], nil
}

func (s *sliceEntryAccessor) Range(start, end int64) ([]Entry, error) {
	n := int64(len(s.entries))
	if start < 0 || end > n || start > end {
		return nil, fmt.Errorf("entry range [%d, %d) out of bounds [0, %d)", start, end, n)
	}
	return s.entries[start:end], nil
}

// ---------------------------------------------------------------------------
// dnmEntryAccessor
// ---------------------------------------------------------------------------

type dnmEntryAccessor struct {
	r *DNMReader
}

// NewDNMEntryAccessor wraps an open DNMReader. At() seeks to the record;
// Range() does a single seek then reads sequentially.
func NewDNMEntryAccessor(r *DNMReader) EntryAccessor {
	return &dnmEntryAccessor{r: r}
}

func (d *dnmEntryAccessor) Count() int64 {
	return d.r.EntriesCount()
}

func (d *dnmEntryAccessor) At(i int64) (Entry, error) {
	return d.r.EntryAt(uint64(i))
}

func (d *dnmEntryAccessor) Range(start, end int64) ([]Entry, error) {
	return d.r.EntriesRange(uint64(start), uint64(end))
}

// ---------------------------------------------------------------------------
// Binary search helpers
// ---------------------------------------------------------------------------

// SearchEntries returns the index of the first entry where
//
//	entry.VolumeOffset + int64(entry.ChunkLength) > target
//
// This identifies the first chunk that overlaps or starts after target — the
// start index for scanning chunks that cover a file region beginning at target.
// Returns ea.Count() if no such entry exists.
func SearchEntries(ea EntryAccessor, target int64) (int64, error) {
	n := ea.Count()
	lo, hi := int64(0), n
	for lo < hi {
		mid := (lo + hi) / 2
		e, err := ea.At(mid)
		if err != nil {
			return 0, fmt.Errorf("SearchEntries: accessing entry %d: %w", mid, err)
		}
		if e.VolumeOffset+int64(e.ChunkLength) <= target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// SearchEntriesEnd returns the index of the first entry where
//
//	entry.VolumeOffset >= target
//
// This is the exclusive upper bound for Range calls: all entries before this
// index have VolumeOffset < target and may overlap a file region ending at target.
// Returns ea.Count() if all entries have VolumeOffset < target.
func SearchEntriesEnd(ea EntryAccessor, target int64) (int64, error) {
	n := ea.Count()
	lo, hi := int64(0), n
	for lo < hi {
		mid := (lo + hi) / 2
		e, err := ea.At(mid)
		if err != nil {
			return 0, fmt.Errorf("SearchEntriesEnd: accessing entry %d: %w", mid, err)
		}
		if e.VolumeOffset < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}
