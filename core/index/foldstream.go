// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

// Streaming delta fold (#504). Compaction used to fold by opening the
// staged index (which merges every pending delta into the in-memory write
// buffer, ~130 B/entry) and Closing it — whose Flush materializes the
// ENTIRE base sorted file as []IndexEntry. Both costs track the repo, not
// the work: at the fleet's 16M-entry index that is 1+ GB of heap to fold
// 190 MB of deltas. This fold holds only the DELTA records (48 bytes each,
// flat) plus per-prefix run buffers; the base streams through.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const foldRecSize = EntrySize + 8 // 48-byte index record + delta sequence

// FoldPassObserver, when set (tests only), receives each pass's ACTUAL slab
// bytes and the bloom's resident bytes. The burst guards first derived
// their verdict from concurrently-sampled HeapAlloc, and even with forced
// collection the sample includes whatever the fold allocated DURING the GC
// cycle — 60-90 MB of variance on a contended 2-core runner, which made
// the same code read 38 MB on linux and 127 on windows. These numbers are
// the ground truth the sampling was trying to estimate.
var FoldPassObserver func(slabBytes, bloomBytes int64)

// FoldDeltasStreamed folds every staged delta under dir/deltas into
// dir/hash-index.db and dir/bloom.bin, with the same semantics the
// open-and-flush fold had: deltas apply in sorted object-name order, later
// entries win on a duplicate hash (a delta beats the base; a later delta
// beats an earlier one), and every delta is FULLY parsed — checksum
// included — before anything is written. Returns the number of delta
// entries folded (duplicates counted, matching the staging report).
func FoldDeltasStreamed(dir string, expectedChunks uint64, fpRate float64) (int, error) {
	return FoldDeltasStreamedWithBudget(dir, expectedChunks, fpRate, DefaultFoldBatchBudget)
}

// FoldDeltasStreamedWithBudget is FoldDeltasStreamed with the caller sizing
// the per-pass slab; batchBudget <= 0 selects DefaultFoldBatchBudget.
func FoldDeltasStreamedWithBudget(dir string, expectedChunks uint64, fpRate float64, batchBudget int64) (int, error) {
	ddir := filepath.Join(dir, DeltaSubdir)
	names, err := os.ReadDir(ddir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var deltaFiles []string
	for _, e := range names {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".delta" {
			deltaFiles = append(deltaFiles, e.Name())
		}
	}
	sort.Strings(deltaFiles)

	bloomPath := filepath.Join(dir, "bloom.bin")
	var bloom *BloomFilter
	if _, serr := os.Stat(bloomPath); serr == nil {
		bloom, err = LoadBloomFilter(bloomPath)
		if err != nil {
			return 0, fmt.Errorf("loading bloom filter: %w", err)
		}
	} else {
		bloom = NewBloomFilter(expectedChunks, fpRate)
	}

	// SEGMENTED (#507 round 2): the first shape batched FILES, with "a
	// single over-budget delta still folds alone" as the escape hatch — and
	// a big machine's first backup publishes nearly its whole index as ONE
	// delta, so the hatch handed a 470 MB file to a whole-file read plus a
	// whole-file slab: prod telemetry showed 32 small deltas batching
	// perfectly for four seconds, then +471 MB in one second, doubled to
	// 987 by append-growth, OOM. The budget now bounds the PASS whatever
	// the file layout: every delta is first VALIDATED by streaming (header,
	// version, size, checksum — the full-parse-before-anything-is-written
	// contract, minus the buffer), then records stream into the slab and a
	// merge pass runs every time the slab reaches budget — mid-file if
	// that is where the budget lands. Later segments override earlier ones
	// exactly as later deltas override earlier: each pass rewrites the base
	// the previous pass produced.
	for _, name := range deltaFiles {
		if err := ValidateDeltaFile(filepath.Join(ddir, name)); err != nil {
			return 0, fmt.Errorf("index delta %s: %w", name, err)
		}
	}
	budget := batchBudget
	if budget <= 0 {
		budget = DefaultFoldBatchBudget
	}
	slab := make([]byte, 0, budget+foldRecSize)
	total := 0
	seq := uint64(0)
	flushPass := func() error {
		if len(slab) == 0 {
			return nil
		}
		if err := mergePass(dir, slab); err != nil {
			return err
		}
		if FoldPassObserver != nil {
			FoldPassObserver(int64(len(slab)), BloomBytes(bloom))
		}
		slab = slab[:0]
		return nil
	}
	for _, name := range deltaFiles {
		err := streamDeltaRecords(filepath.Join(ddir, name), func(e *DeltaEntry) error {
			bloom.Add(e.WeakHash)
			var rec [foldRecSize]byte
			ie := IndexEntry{StrongHash: e.StrongHash, PackNumber: e.PackNumber,
				StoreOffset: e.StoreOffset, ChunkLength: e.ChunkLength}
			encodeEntry(&ie, rec[:EntrySize])
			binary.LittleEndian.PutUint64(rec[EntrySize:], seq)
			if int64(len(slab))+foldRecSize > budget {
				if err := flushPass(); err != nil {
					return err
				}
			}
			slab = append(slab, rec[:]...)
			seq++
			total++
			return nil
		})
		if err != nil {
			return total, fmt.Errorf("index delta %s: %w", name, err)
		}
	}
	if err := flushPass(); err != nil {
		return total, err
	}
	if err := bloom.Save(bloomPath); err != nil {
		return total, fmt.Errorf("saving bloom filter: %w", err)
	}
	return total, nil
}

// DefaultFoldBatchBudget bounds one fold pass's slab when the caller does
// not size it (#507): it keeps a fold's live heap around
// batch + bloom + buffers on a 512Mi pod. The engine takes the budget as a
// PARAMETER (FoldDeltasStreamedWithBudget) and reads no environment; the
// product decides its budget from its own knobs and hands it in.
const DefaultFoldBatchBudget int64 = 32 << 20

// mergePass sorts one budgeted slab of records and stream-merges it with
// the sorted base — the fold's single-pass core, run once per budget-full
// slab (#507: mid-file when the budget lands there).
func mergePass(dir string, slab []byte) error {
	// 2. Sort by (hash, sequence): equal hashes adjacent, the winning —
	// latest — record last in its group. (Within one batch; across batches
	// the later pass rewrites the base the earlier one produced.)
	sort.Sort(foldRecs(slab))

	// 3. Stream-merge with the sorted base. The base is ordered by its
	// 8-byte hash prefix; equal-prefix runs are buffered (they are a
	// handful of entries) so the duplicate rule can match FULL hashes.
	basePath := filepath.Join(dir, "hash-index.db")
	tmpPath := basePath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	abort := func() { out.Close(); os.Remove(tmpPath) }

	br, closeBase, err := openBaseReader(basePath)
	if err != nil {
		abort()
		return err
	}
	defer closeBase()

	di := 0 // record index into slab
	nRecs := len(slab) / foldRecSize
	deltaPrefix := func(i int) uint64 {
		return binary.BigEndian.Uint64(slab[i*foldRecSize:])
	}
	baseRun, err := br.nextRun()
	if err != nil {
		abort()
		return err
	}
	writeEntry := func(rec []byte) error {
		_, werr := bw.Write(rec[:EntrySize])
		return werr
	}
	for baseRun != nil || di < nRecs {
		var p uint64
		switch {
		case baseRun == nil:
			p = deltaPrefix(di)
		case di >= nRecs:
			p = baseRun.prefix
		default:
			p = baseRun.prefix
			if dp := deltaPrefix(di); dp < p {
				p = dp
			}
		}
		// Collect this prefix's delta records, deduped to the LAST of each
		// full-hash group (they are hash-then-sequence sorted).
		type winRec struct{ rec []byte }
		var deltaWins [][]byte
		for di < nRecs && deltaPrefix(di) == p {
			start := di
			for di+1 < nRecs && bytes.Equal(slab[(di+1)*foldRecSize:(di+1)*foldRecSize+32], slab[start*foldRecSize:start*foldRecSize+32]) {
				di++
			}
			deltaWins = append(deltaWins, slab[di*foldRecSize:di*foldRecSize+EntrySize])
			di++
		}
		// Base entries of this prefix, minus the hashes the deltas override.
		if baseRun != nil && baseRun.prefix == p {
			for _, be := range baseRun.entries {
				overridden := false
				for _, dw := range deltaWins {
					if bytes.Equal(be[:32], dw[:32]) {
						overridden = true
						break
					}
				}
				if !overridden {
					if err := writeEntry(be); err != nil {
						abort()
						return err
					}
				}
			}
			baseRun, err = br.nextRun()
			if err != nil {
				abort()
				return err
			}
		}
		for _, dw := range deltaWins {
			if err := writeEntry(dw); err != nil {
				abort()
				return err
			}
		}
	}
	if err := bw.Flush(); err != nil {
		abort()
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	closeBase()
	if err := os.Rename(tmpPath, basePath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// foldRecs sorts the flat record slab by (full hash, sequence).
type foldRecs []byte

func (r foldRecs) Len() int { return len(r) / foldRecSize }
func (r foldRecs) Less(i, j int) bool {
	a, b := r[i*foldRecSize:], r[j*foldRecSize:]
	if c := bytes.Compare(a[:32], b[:32]); c != 0 {
		return c < 0
	}
	return binary.LittleEndian.Uint64(a[EntrySize:]) < binary.LittleEndian.Uint64(b[EntrySize:])
}
func (r foldRecs) Swap(i, j int) {
	var tmp [foldRecSize]byte
	copy(tmp[:], r[i*foldRecSize:(i+1)*foldRecSize])
	copy(r[i*foldRecSize:(i+1)*foldRecSize], r[j*foldRecSize:(j+1)*foldRecSize])
	copy(r[j*foldRecSize:(j+1)*foldRecSize], tmp[:])
}

// baseReader yields the sorted base file one equal-prefix run at a time.
// The run arena is REUSED between runs: a fresh 48-byte allocation per base
// entry produced ~96 MB of garbage across a 2M-entry base, and the burst
// test's peak sampler (rightly) counts garbage the collector has not
// reached yet — the #498 build had the same lesson.
type baseReader struct {
	br      *bufio.Reader
	peeked  [EntrySize]byte
	hasPeek bool
	arena   []byte
	views   [][]byte
}

type baseRun struct {
	prefix  uint64
	entries [][]byte
}

func openBaseReader(path string) (*baseReader, func(), error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &baseReader{}, func() {}, nil // never compacted: empty base
	}
	if err != nil {
		return nil, nil, err
	}
	r := &baseReader{br: bufio.NewReaderSize(f, 4<<20)}
	return r, func() { f.Close() }, nil
}

// readOne fills dst with the next entry; false at EOF.
func (r *baseReader) readOne(dst []byte) (bool, error) {
	if r.br == nil {
		return false, nil
	}
	if r.hasPeek {
		copy(dst, r.peeked[:])
		r.hasPeek = false
		return true, nil
	}
	if _, err := io.ReadFull(r.br, dst); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return false, fmt.Errorf("sorted index truncated mid-entry")
		}
		return false, err
	}
	return true, nil
}

// nextRun returns all consecutive entries sharing one hash prefix, or nil
// at EOF. The returned run's entries alias the reader's arena and are valid
// only until the next call.
func (r *baseReader) nextRun() (*baseRun, error) {
	r.arena = r.arena[:0]
	r.views = r.views[:0]
	var first [EntrySize]byte
	ok, err := r.readOne(first[:])
	if err != nil || !ok {
		return nil, err
	}
	prefix := binary.BigEndian.Uint64(first[:8])
	r.arena = append(r.arena, first[:]...)
	for {
		var e [EntrySize]byte
		ok, err := r.readOne(e[:])
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if binary.BigEndian.Uint64(e[:8]) != prefix {
			r.peeked = e
			r.hasPeek = true
			break
		}
		r.arena = append(r.arena, e[:]...)
	}
	for off := 0; off < len(r.arena); off += EntrySize {
		r.views = append(r.views, r.arena[off:off+EntrySize])
	}
	return &baseRun{prefix: prefix, entries: r.views}, nil
}

// FilterSortedIndex rewrites a sorted hash-index.db in place keeping only
// the entries whose pack number the predicate accepts — the GC rebuild's
// drop-these-packs pass, streaming (#507 stage 2: ReadAllEntries
// materialized the whole repo to do this).
func FilterSortedIndex(path string, keep func(packNumber uint32) bool) error {
	src, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing to filter
		}
		return err
	}
	tmpPath := path + ".tmp"
	dst, err := os.Create(tmpPath)
	if err != nil {
		src.Close()
		return err
	}
	br := bufio.NewReaderSize(src, 4<<20)
	bw := bufio.NewWriterSize(dst, 1<<20)
	buf := make([]byte, EntrySize)
	abort := func() { src.Close(); dst.Close(); os.Remove(tmpPath) }
	for {
		if _, rerr := io.ReadFull(br, buf); rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			abort()
			if errors.Is(rerr, io.ErrUnexpectedEOF) {
				return fmt.Errorf("sorted index truncated mid-entry")
			}
			return rerr
		}
		if keep(binary.LittleEndian.Uint32(buf[32:36])) {
			if _, werr := bw.Write(buf); werr != nil {
				abort()
				return werr
			}
		}
	}
	src.Close()
	if err := bw.Flush(); err != nil {
		abort()
		return err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
