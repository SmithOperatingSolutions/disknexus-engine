// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// EntrySize is the on-disk size of a single index entry.
// 32 (SHA-256) + 8 (pack offset) + 4 (chunk length) + 4 (pack number) = 48 bytes
const EntrySize = 48

// IndexEntry maps a chunk's strong hash to its location in the pack files.
type IndexEntry struct {
	StrongHash  [32]byte
	PackNumber  uint32
	StoreOffset uint64
	ChunkLength uint32
}

// HashIndex is a sorted on-disk hash index with a disk-backed hash table
// (.htab) for O(1) dedup lookups during a backup session.
//
// At startup, the hash table is built from the sorted index file via a
// sequential read. During a backup, all Lookup and FlushDelta operations hit
// the hash table (1–2 random reads per lookup). At the final Flush, all
// entries are collected from the hash table, sorted, and written back to the
// sorted index file; the hash table is then rebuilt from the new sorted file.
// The .htab file is ephemeral — always rebuilt at the start of each backup.
type HashIndex struct {
	mu sync.RWMutex

	path    string
	entries map[[32]byte]IndexEntry // in-memory write buffer (current window)
	dirty   bool                    // true if entries were added since last Flush
	noHtab  bool                    // true to skip building/using the .htab file

	htab     *diskHashTable
	htabPath string

	file     *os.File
	diskSize int64

	// cache is accepted for API compatibility but unused for lookups.
	cache *pageCache
}

// NewHashIndex creates or opens a hash index at the given path.
// cacheMB is accepted for API compatibility but is not used for lookups.
// When skipHtab is true the .htab file is not built, saving significant I/O.
// Use skipHtab for callers that only need ReadAll (e.g. prune), not per-chunk
// Lookup or FlushDelta.
func NewHashIndex(path string, cacheMB int, skipHtab bool) (*HashIndex, error) {
	idx := &HashIndex{
		path:     path,
		entries:  make(map[[32]byte]IndexEntry),
		htabPath: path + ".htab",
		noHtab:   skipHtab,
	}
	if cacheMB > 0 {
		idx.cache = newPageCache(cacheMB)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("opening hash index: %w", err)
	}
	idx.file = f

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat hash index: %w", err)
	}
	idx.diskSize = info.Size()

	// Remove stale temp/working files from previous interrupted runs.
	os.Remove(path + ".tmp")
	os.Remove(path + ".session")
	os.Remove(path + ".session.tmp")
	os.Remove(path + ".htab.tmp")
	for i := 0; ; i++ {
		dp := fmt.Sprintf("%s.delta.%06d", path, i)
		if err := os.Remove(dp); err != nil {
			break
		}
	}

	if skipHtab {
		os.Remove(idx.htabPath)
	} else {
		// Always rebuild the hash table fresh from the sorted file.
		// This ensures correctness even if a previous backup was interrupted.
		os.Remove(idx.htabPath)
		if err := idx.buildHashTable(); err != nil {
			f.Close()
			return nil, fmt.Errorf("building hash table: %w", err)
		}
	}

	return idx, nil
}

// htabBuildK is the MINIMUM number of bucket segments used when building the
// hash table. The actual K scales with the index so the per-segment
// residency stays CONSTANT: a fixed K=8 made the build's peak grow with
// repo size — ~440 MB at a 22M-entry index by design, which OOM-killed the
// 512Mi controller in 4 seconds opening a 264.9 GB repo's ~16M-entry index
// (#498), before any request-level budget could refuse. Higher K costs
// temp-file count and sequential I/O, both cheap; an unbounded burst on a
// memory-limited host is not.
const htabBuildK = 8

// htabBuildSegTarget bounds one segment's resident bytes. A segment holds
// its slot array (EntrySize per slot), its bucket's entries (~EntrySize/2
// per slot at the 2x slot ratio), and two 8-byte sort arrays over those
// entries — ~80 bytes per slot all told, so 24 MB of slots keeps the whole
// build under ~64 MB with the phase-1 writer buffers included.
const htabBuildSegTarget = 24 << 20

// htabBuildWriterBudget bounds phase 1's bucket writer buffers as a POOL:
// per-writer sizing shrinks as K grows, or 64 segments of 1 MB buffers
// would quietly rebuild the very burst the segmentation removed.
const htabBuildWriterBudget = 16 << 20

// buildHashTable creates a new .htab file from the current sorted index file.
// Must be called with idx.file open and idx.diskSize set correctly.
// Must NOT be called while the read lock is held (it writes idx.htab).
//
// Algorithm (K-pass bucket build):
//
//	Phase 1 – one sequential read of the sorted file distributes every entry
//	into one of K small temp files, each covering 1/K of the slot range.
//
//	Phase 2 – for each segment k:
//	  • read bucket k into memory (~numEntries/K × 48 B)
//	  • sort entries by home_slot for sequential cache access during probing
//	  • build partSlots (numSlots/K × 48 B) via linear probing
//	  • entries that probe past the segment boundary are carried to segment k+1
//	  • stream partSlots to the output file and free it before the next segment
//
//	Wrap-around carry (entries from segment K-1 wrapping to slot 0) is handled
//	with a handful of random writes; at 50% load this is always empty.
func (idx *HashIndex) buildHashTable() error {
	numEntries := idx.diskSize / EntrySize
	numSlots := numEntries * 2
	if numSlots < 1024 {
		numSlots = 1024
	}

	K := int64(htabBuildK)
	if maxSegSlots := int64(htabBuildSegTarget / EntrySize); numSlots > maxSegSlots*K {
		K = (numSlots + maxSegSlots - 1) / maxSegSlots
	}
	segSize := (numSlots + K - 1) / K // ceiling so last segment is never larger

	// --- Phase 1: distribute sorted-file entries into K bucket temp files ---

	type bkt struct {
		f    *os.File
		path string
		n    int64
	}
	bkts := make([]bkt, K)

	closeBuckets := func() {
		for i := range bkts {
			if bkts[i].f != nil {
				bkts[i].f.Close()
				os.Remove(bkts[i].path)
				bkts[i].f = nil
			}
		}
	}
	defer closeBuckets()

	for i := int64(0); i < K; i++ {
		p := fmt.Sprintf("%s.b%d.tmp", idx.path, i)
		f, err := os.Create(p)
		if err != nil {
			return fmt.Errorf("creating bucket file: %w", err)
		}
		bkts[i] = bkt{f: f, path: p}
	}

	writerBuf := htabBuildWriterBudget / int(K)
	if writerBuf > 1<<20 {
		writerBuf = 1 << 20
	}
	if writerBuf < 64<<10 {
		writerBuf = 64 << 10
	}
	bws := make([]*bufio.Writer, K)
	for i := range bws {
		bws[i] = bufio.NewWriterSize(bkts[i].f, writerBuf)
	}

	if idx.diskSize > 0 {
		if _, err := idx.file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seeking sorted file: %w", err)
		}
		br := bufio.NewReaderSize(idx.file, 4<<20)
		buf := make([]byte, EntrySize)
		for {
			if _, err := io.ReadFull(br, buf); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					break
				}
				return fmt.Errorf("reading sorted file: %w", err)
			}
			// hashPrefix8 = BE uint64 of first 8 bytes of StrongHash.
			homeSlot := int64(binary.BigEndian.Uint64(buf[:8]) % uint64(numSlots))
			bi := homeSlot / segSize
			if bi >= K {
				bi = K - 1
			}
			if _, err := bws[bi].Write(buf); err != nil {
				return fmt.Errorf("writing bucket %d: %w", bi, err)
			}
			bkts[bi].n++
		}
	}

	for i, bw := range bws {
		if err := bw.Flush(); err != nil {
			return fmt.Errorf("flushing bucket %d: %w", i, err)
		}
	}
	// No seek needed here: Phase 2 uses ReadAt(buf, 0) which specifies an
	// absolute offset and bypasses the file pointer entirely.  On Windows,
	// Seek on a newly-created empty file can return ERROR_INVALID_HANDLE
	// (SetFilePointerEx fails before the first write touches the handle).

	// --- Phase 2: build each segment and stream to the htab output file ---

	outF, err := os.Create(idx.htabPath)
	if err != nil {
		return fmt.Errorf("creating htab: %w", err)
	}
	abortOut := func() { outF.Close(); os.Remove(idx.htabPath) }

	// Write header with placeholder count; patched to final value at the end.
	hdr := make([]byte, htabHeaderSize)
	copy(hdr[:8], htabMagic[:])
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(numSlots))
	if _, err := outF.Write(hdr); err != nil {
		abortOut()
		return fmt.Errorf("writing htab header: %w", err)
	}

	outBW := bufio.NewWriterSize(outF, 4<<20)
	var totalCount int64
	var carry [][]byte // entries that probed past end of the previous segment
	var zero [32]byte

	// One allocation each, REUSED across all K segments. Per-segment makes
	// (the first shape) kept live residency bounded but let dead segments
	// pile up between GC cycles — on a slow runner HeapAlloc peaked at the
	// SUM of several segments, which is the pre-#498 number wearing a GC
	// pacing hat. Reuse makes the peak a property of the code, not of the
	// collector's mood.
	segBuf := make([]byte, segSize*EntrySize)
	var bucketBuf []byte
	var homeBuf []uint64
	var idxBuf []int

	for k := int64(0); k < K; k++ {
		segStart := k * segSize
		segEnd := segStart + segSize
		if segEnd > numSlots {
			segEnd = numSlots
		}
		thisSeg := segEnd - segStart

		partSlots := segBuf[:thisSeg*EntrySize]
		for i := range partSlots {
			partSlots[i] = 0
		}

		// Place carry entries from the previous segment. They arrive here
		// because all slots from their home_slot to the prior segment's end
		// were occupied; they probe forward from slot 0 of this segment.
		var nextCarry [][]byte
		nf := int64(0) // next free position to try for carry placement
		for _, e := range carry {
			placed := false
			for nf < thisSeg {
				off := nf * EntrySize
				var h [32]byte
				copy(h[:], partSlots[off:off+32])
				nf++
				if h == zero {
					copy(partSlots[(nf-1)*EntrySize:nf*EntrySize], e)
					totalCount++
					placed = true
					break
				}
			}
			if !placed {
				nextCarry = append(nextCarry, e)
			}
		}
		carry = nextCarry

		// Read this bucket's entries into a flat buffer.
		// ReadAt uses an absolute offset so no prior Seek is required.
		n := int(bkts[k].n)
		if need := int64(n) * EntrySize; int64(cap(bucketBuf)) < need {
			bucketBuf = make([]byte, need)
		}
		bucketData := bucketBuf[:int64(n)*EntrySize]
		if n > 0 {
			if _, err := fileReadAt(bkts[k].f, bucketData, 0); err != nil {
				abortOut()
				return fmt.Errorf("reading bucket %d: %w", k, err)
			}
		}

		// Precompute home_slot for each entry (avoid repeated hash work during sort).
		if cap(homeBuf) < n {
			homeBuf = make([]uint64, n)
		}
		homeSlots := homeBuf[:n]
		for i := 0; i < n; i++ {
			homeSlots[i] = binary.BigEndian.Uint64(bucketData[i*EntrySize:]) % uint64(numSlots)
		}

		// Sort entry indices by home_slot so probes advance sequentially through
		// partSlots, improving cache behaviour during insertion.
		if cap(idxBuf) < n {
			idxBuf = make([]int, n)
		}
		sortIdx := idxBuf[:n]
		for i := range sortIdx {
			sortIdx[i] = i
		}
		sort.Slice(sortIdx, func(a, b int) bool {
			return homeSlots[sortIdx[a]] < homeSlots[sortIdx[b]]
		})

		// Linear-probe each entry into partSlots.
		for _, ei := range sortIdx {
			e := bucketData[ei*EntrySize : (ei+1)*EntrySize]
			relSlot := int64(homeSlots[ei]) - segStart
			var eh [32]byte
			copy(eh[:], e[:32])

			placed := false
			for probe := relSlot; probe < thisSeg; probe++ {
				off := probe * EntrySize
				var sh [32]byte
				copy(sh[:], partSlots[off:off+32])
				if sh == zero {
					copy(partSlots[off:off+EntrySize], e)
					totalCount++
					placed = true
					break
				}
				if sh == eh {
					placed = true // duplicate; skip
					break
				}
			}
			if !placed {
				ec := make([]byte, EntrySize)
				copy(ec, e)
				carry = append(carry, ec)
			}
		}

		if _, err := outBW.Write(partSlots); err != nil {
			abortOut()
			return fmt.Errorf("writing htab segment %d: %w", k, err)
		}
	}

	if err := outBW.Flush(); err != nil {
		abortOut()
		return fmt.Errorf("flushing htab: %w", err)
	}

	// Wrap-around carry: entries from the last segment that wrap to slot 0+.
	// At 50% load this is always empty; included for correctness.
	for _, e := range carry {
		var eh [32]byte
		copy(eh[:], e[:32])
		placed := false
		for probe := int64(0); probe < numSlots; probe++ {
			slotOff := int64(htabHeaderSize) + probe*EntrySize
			var slotBuf [EntrySize]byte
			if _, err := fileReadAt(outF, slotBuf[:], slotOff); err != nil {
				abortOut()
				return fmt.Errorf("reading slot for wrap-around: %w", err)
			}
			var sh [32]byte
			copy(sh[:], slotBuf[:32])
			if sh == zero {
				if _, err := fileWriteAt(outF, e, slotOff); err != nil {
					abortOut()
					return fmt.Errorf("writing wrap-around slot: %w", err)
				}
				totalCount++
				placed = true
				break
			}
			if sh == eh {
				placed = true // duplicate
				break
			}
		}
		if !placed {
			abortOut()
			return fmt.Errorf("hash table full during wrap-around build (numSlots=%d)", numSlots)
		}
	}

	// Patch the final count into the header.
	var countBuf [8]byte
	binary.LittleEndian.PutUint64(countBuf[:], uint64(totalCount))
	if _, err := fileWriteAt(outF, countBuf[:], 16); err != nil {
		abortOut()
		return fmt.Errorf("writing htab count: %w", err)
	}
	if err := outF.Close(); err != nil {
		os.Remove(idx.htabPath)
		return fmt.Errorf("closing htab: %w", err)
	}

	ht, err := openDiskHashTable(idx.htabPath)
	if err != nil {
		return err
	}
	idx.htab = ht
	return nil
}

// Lookup searches for a chunk by its ChunkID.
// Returns the entry and true if found, nil and false otherwise.
//
// Search order: in-memory buffer → hash table (covers sorted file + all
// FlushDelta'd entries).
func (idx *HashIndex) Lookup(id hasher.ChunkID) (*IndexEntry, bool, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if entry, ok := idx.entries[id.StrongHash]; ok {
		return &entry, true, nil
	}
	if idx.htab == nil {
		return idx.diskLookup(id.StrongHash)
	}
	return idx.htab.Lookup(id.StrongHash)
}

// diskLookup binary-searches the sorted index file directly (#183). With no
// hash table — every read-only open of a downloaded index, where only the
// .db and bloom are published — Lookup used to return not-found without
// ever reading the disk. The file is sorted by the 8-byte hash prefix, so
// search on the prefix, then scan the equal-prefix run for the full hash.
// Caller must hold idx.mu (read or write).
func (idx *HashIndex) diskLookup(strong [32]byte) (*IndexEntry, bool, error) {
	n := idx.diskSize / EntrySize
	if n == 0 || idx.file == nil {
		return nil, false, nil
	}
	want := hashPrefix8(strong)
	buf := make([]byte, EntrySize)
	readAt := func(i int64) (*IndexEntry, error) {
		if _, err := idx.file.ReadAt(buf, i*EntrySize); err != nil {
			return nil, fmt.Errorf("disk lookup read at %d: %w", i, err)
		}
		var e IndexEntry
		decodeEntry(buf, &e)
		return &e, nil
	}
	lo, hi := int64(0), n
	for lo < hi {
		mid := (lo + hi) / 2
		e, err := readAt(mid)
		if err != nil {
			return nil, false, err
		}
		if hashPrefix8(e.StrongHash) < want {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	for i := lo; i < n; i++ {
		e, err := readAt(i)
		if err != nil {
			return nil, false, err
		}
		if hashPrefix8(e.StrongHash) != want {
			break
		}
		if e.StrongHash == strong {
			return e, true, nil
		}
	}
	return nil, false, nil
}

// SetMemFlushed is a no-op retained for API compatibility.
// The hash table provides O(1) lookups for all flushed entries; the separate
// in-memory flushed set is no longer needed.
func (idx *HashIndex) SetMemFlushed(_ bool) {}

// Insert adds a new entry to the in-memory buffer.
// Call FlushDelta periodically and Flush at the end to persist to disk.
func (idx *HashIndex) Insert(id hasher.ChunkID, packNumber uint32, storeOffset uint64, chunkLength uint32) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	idx.entries[id.StrongHash] = IndexEntry{
		StrongHash:  id.StrongHash,
		PackNumber:  packNumber,
		StoreOffset: storeOffset,
		ChunkLength: chunkLength,
	}
	idx.dirty = true
}

// FlushDelta moves the in-memory buffer into the hash table and clears it.
// Growing the hash table if needed is handled automatically.
func (idx *HashIndex) FlushDelta() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if len(idx.entries) == 0 {
		return nil
	}

	if idx.htab == nil {
		return fmt.Errorf("FlushDelta requires the hash table (index was opened with skipHtab; use Flush instead)")
	}

	for _, entry := range idx.entries {
		if err := idx.htab.Insert(entry); errors.Is(err, errTableFull) {
			if err2 := idx.growHashTable(); err2 != nil {
				return fmt.Errorf("growing hash table: %w", err2)
			}
			if err3 := idx.htab.Insert(entry); err3 != nil {
				return fmt.Errorf("inserting after grow: %w", err3)
			}
		} else if err != nil {
			return fmt.Errorf("inserting into hash table: %w", err)
		}
	}
	clear(idx.entries)
	return nil
}

// growHashTable doubles the number of slots in the hash table by reading all
// existing entries, creating a new file with 2× slots, and atomically
// replacing the old file. Must be called with idx.mu held.
func (idx *HashIndex) growHashTable() error {
	entries, err := idx.htab.ReadAll()
	if err != nil {
		return fmt.Errorf("reading entries for grow: %w", err)
	}

	newSlots := idx.htab.numSlots * 2
	tmpPath := idx.htabPath + ".tmp"

	newHT, err := createDiskHashTable(tmpPath, newSlots)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if err := newHT.Insert(e); err != nil {
			newHT.f.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("inserting into grown table: %w", err)
		}
	}

	// Close the new table before rename so Windows can move the file
	// (Windows does not allow renaming a file with open handles).
	// This also flushes the in-memory count to the file header.
	if err := newHT.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing grown hash table: %w", err)
	}

	if err := idx.htab.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing old hash table: %w", err)
	}
	os.Remove(idx.htabPath)

	if err := os.Rename(tmpPath, idx.htabPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming grown hash table: %w", err)
	}

	// Reopen the renamed file so idx.htab has a valid handle to the new path.
	ht, err := openDiskHashTable(idx.htabPath)
	if err != nil {
		return fmt.Errorf("reopening grown hash table: %w", err)
	}
	idx.htab = ht
	return nil
}

// Flush collects all entries from the hash table and in-memory buffer, sorts
// them, writes a new sorted index file atomically, then rebuilds the hash
// table from the new file. All temporary files are cleaned up.
func (idx *HashIndex) Flush() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if !idx.dirty && len(idx.entries) == 0 {
		return nil
	}

	// Collect all entries.
	// Reopen the htab file to get a fresh handle. On some Windows
	// configurations (GitHub Actions), handles that have been idle between
	// the last FlushDelta write and this Flush read return
	// ERROR_INVALID_HANDLE from ReadFile, even with OVERLAPPED I/O.
	if idx.htab != nil {
		idx.htab.f.Close()
		ht, err := openDiskHashTable(idx.htabPath)
		if err != nil {
			return fmt.Errorf("reopening htab for flush: %w", err)
		}
		idx.htab = ht
	}
	var all []IndexEntry
	if idx.htab != nil {
		entries, err := idx.htab.ReadAll()
		if err != nil {
			return fmt.Errorf("reading hash table: %w", err)
		}
		all = entries
	} else {
		// Skip-htab mode has no hash table to collect from; merge with the
		// existing sorted file so previously flushed entries survive the
		// rewrite below.
		entries, err := idx.readSortedFile()
		if err != nil {
			return fmt.Errorf("reading sorted index: %w", err)
		}
		all = entries
	}
	// The in-memory buffer is newest: it wins over any same-hash entry
	// already collected (Insert may relocate an existing chunk).
	if len(idx.entries) > 0 {
		kept := all[:0]
		for _, e := range all {
			if _, ok := idx.entries[e.StrongHash]; !ok {
				kept = append(kept, e)
			}
		}
		all = kept
		for _, entry := range idx.entries {
			all = append(all, entry)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		return hashPrefix8(all[i].StrongHash) < hashPrefix8(all[j].StrongHash)
	})

	tmpPath := idx.path + ".tmp"
	tf, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating temp index: %w", err)
	}

	bw := bufio.NewWriterSize(tf, 1<<20)
	entryBuf := make([]byte, EntrySize)
	for i := range all {
		encodeEntry(&all[i], entryBuf)
		if _, err := bw.Write(entryBuf); err != nil {
			tf.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("writing temp index entry: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		tf.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("flushing temp index: %w", err)
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing temp index: %w", err)
	}
	tf.Close()

	// Close the current file descriptor before replacing it: on Windows the
	// rename fails while a handle to the destination is open.
	//
	// Best-effort: this handle sits idle for the whole backup while reads go
	// through the hash table, and on some Windows configurations (GitHub
	// Actions) an idle handle returns ERROR_INVALID_HANDLE from Close — the
	// same failure mode the htab reopen above works around. A Close error
	// here does not mean the handle still holds the file, so proceed to the
	// rename regardless; if the handle really were still open, the rename
	// would fail and be handled there. (The Close error is intentionally
	// ignored, mirroring the htab close above.)
	if idx.file != nil {
		_ = idx.file.Close()
		idx.file = nil
	}

	if err := os.Rename(tmpPath, idx.path); err != nil {
		os.Remove(tmpPath)
		// Best-effort reopen so the index remains usable.
		idx.file, _ = os.OpenFile(idx.path, os.O_RDWR, 0644)
		return fmt.Errorf("renaming temp index: %w", err)
	}

	idx.file, err = os.OpenFile(idx.path, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("reopening index after flush: %w", err)
	}

	idx.diskSize = int64(len(all)) * EntrySize
	clear(idx.entries)
	idx.dirty = false

	// Rebuild the hash table from the new sorted file so that post-Flush
	// lookups (e.g. in tests) continue to work.
	if idx.htab != nil {
		idx.htab.f.Close()
		os.Remove(idx.htabPath)
		idx.htab = nil
	}
	if !idx.noHtab {
		if err := idx.buildHashTable(); err != nil {
			return fmt.Errorf("rebuilding hash table after flush: %w", err)
		}
	}

	if idx.cache != nil {
		idx.cache.invalidate()
	}

	return nil
}

// readSortedFile reads every entry from the sorted index file.
// The caller must hold idx.mu (read or write).
func (idx *HashIndex) readSortedFile() ([]IndexEntry, error) {
	all := make([]IndexEntry, 0, idx.diskSize/EntrySize)
	if idx.diskSize == 0 {
		return all, nil
	}
	if _, err := idx.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking index start: %w", err)
	}
	buf := make([]byte, EntrySize)
	for {
		if _, err := io.ReadFull(idx.file, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, fmt.Errorf("reading index entry: %w", err)
		}
		var entry IndexEntry
		decodeEntry(buf, &entry)
		all = append(all, entry)
	}
	return all, nil
}

// ReadAll returns all index entries from the sorted file plus the in-memory
// buffer. Call Flush first to ensure FlushDelta'd entries are included.
func (idx *HashIndex) ReadAll() ([]IndexEntry, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	all, err := idx.readSortedFile()
	if err != nil {
		return nil, err
	}
	for _, entry := range idx.entries {
		all = append(all, entry)
	}
	return all, nil
}

// Count returns the total number of unique entries (hash table + in-memory
// buffer).
func (idx *HashIndex) Count() uint64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if idx.htab == nil {
		return uint64(len(idx.entries))
	}
	// The in-memory buffer can hold a hash already present in the hash table
	// (a re-Insert before the next FlushDelta). Adding htab.count + len(entries)
	// blindly would count such a hash twice, so only add buffered hashes that
	// are not already in the table. Count is called for stats/at-open only —
	// never on the hot path — so the per-entry probe is affordable.
	extra := uint64(0)
	for h := range idx.entries {
		if _, found, err := idx.htab.Lookup(h); err != nil || !found {
			extra++
		}
	}
	return uint64(idx.htab.count) + extra
}

// Close flushes and closes the index.
// CloseDiscard closes all handles without flushing: the in-memory buffer is
// dropped and the sorted index file is left exactly as the last successful
// Flush wrote it. The ephemeral .htab (which may hold FlushDelta'd session
// inserts) is removed — it is rebuilt from the sorted file at next open, so
// discarding it discards the session.
func (idx *HashIndex) CloseDiscard() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	clear(idx.entries)
	idx.dirty = false

	var closeErr error
	if idx.htab != nil {
		closeErr = idx.htab.f.Close()
		os.Remove(idx.htabPath)
		idx.htab = nil
	}
	if idx.file != nil {
		if err := idx.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		idx.file = nil
	}
	return closeErr
}

func (idx *HashIndex) Close() error {
	if err := idx.Flush(); err != nil {
		// Best-effort cleanup of open handles.
		if idx.htab != nil {
			idx.htab.f.Close()
			os.Remove(idx.htabPath)
			idx.htab = nil
		}
		if idx.file != nil {
			idx.file.Close()
			idx.file = nil
		}
		return err
	}

	var closeErr error
	if idx.htab != nil {
		closeErr = idx.htab.Close()
		idx.htab = nil
		os.Remove(idx.htabPath) // ephemeral: always rebuilt at next NewHashIndex
	}
	if idx.file != nil {
		if err := idx.file.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
		idx.file = nil
	}
	return closeErr
}

func hashPrefix8(h [32]byte) uint64 {
	return binary.BigEndian.Uint64(h[0:8])
}

func encodeEntry(e *IndexEntry, buf []byte) {
	copy(buf[0:32], e.StrongHash[:])
	binary.LittleEndian.PutUint32(buf[32:36], e.PackNumber)
	binary.LittleEndian.PutUint64(buf[36:44], e.StoreOffset)
	binary.LittleEndian.PutUint32(buf[44:48], e.ChunkLength)
}

func decodeEntry(buf []byte, e *IndexEntry) {
	copy(e.StrongHash[:], buf[0:32])
	e.PackNumber = binary.LittleEndian.Uint32(buf[32:36])
	e.StoreOffset = binary.LittleEndian.Uint64(buf[36:44])
	e.ChunkLength = binary.LittleEndian.Uint32(buf[44:48])
}
