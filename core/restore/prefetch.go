// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"log/slog"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Prefetch window bounds (#204). A file restore used to fetch every chunk on
// its own — with the S3 backend that is TWO presigned range requests per chunk
// (header, then payload), so a few hundred chunks meant a thousand round trips.
// Chunks are now fetched in windows: one pack-grouped, range-coalesced batch
// covers many chunks at once.
//
// The window is what bounds memory. A restore holds AT MOST one window of raw
// frames at a time (the previous window is dropped before a new batch is
// issued), so the extra footprint is capped at MaxPrefetchBytes regardless of
// how large the file, the pack, or the backup is — nothing here buffers a whole
// file or a whole pack set. Streaming is otherwise unchanged: bytes are still
// written out chunk by chunk as the loop walks the entries.
const (
	// MaxPrefetchBytes caps the estimated frame bytes held by one window.
	MaxPrefetchBytes = 16 << 20
	// MaxPrefetchChunks caps the chunks in one window, so pathologically small
	// chunks cannot turn the byte budget into a huge request list.
	MaxPrefetchChunks = 2048
	// prefetchFrameSlack estimates per-frame overhead (8-byte header, zstd
	// expansion on incompressible data, AES-GCM nonce+tag) when budgeting.
	prefetchFrameSlack = 512
)

// chunkResolution is one manifest entry resolved to its storage location.
// Lookups are hoisted out of the restore loop so a window can be planned
// before the first byte is fetched; failures are carried, not raised, so the
// loop still reports them at exactly the chunk (and therefore the file) where
// it used to.
type chunkResolution struct {
	ref   store.ChunkRef
	found bool
	err   error
}

// resolveChunks looks up every entry's pack location.
func (r *FileRestorer) resolveChunks(entries []manifest.Entry) []chunkResolution {
	out := make([]chunkResolution, len(entries))
	for i, e := range entries {
		ie, found, err := r.index.LookupDirect(e.ChunkHash)
		// A miss yields no entry to read (LookupDirect may hand back nothing at
		// all), so only a hit carries a location.
		out[i] = chunkResolution{found: found, err: err}
		if err == nil && found && ie != nil {
			out[i].ref = store.ChunkRef{
				ChunkLoc: store.ChunkLoc{PackNum: ie.PackNumber, StoreOffset: int64(ie.StoreOffset)},
				RawLen:   int(ie.ChunkLength),
			}
		}
	}
	return out
}

// framePrefetcher fetches chunk frames a window at a time and hands them to
// the restore loop. It is strictly an optimization: every frame it fails to
// supply is read individually by the caller, so error handling, integrity
// checking and --ignore-error granularity are unchanged.
type framePrefetcher struct {
	st        *store.ChunkStore
	logger    *slog.Logger
	frames    map[store.ChunkLoc][]byte
	maxBytes  int64 // window budget; tests shrink it to exercise multiple windows
	maxChunks int
	// lookahead optionally supplies chunks from FOLLOWING files, so a restore
	// of many small files batches across file boundaries instead of paying a
	// round trip per file. It may return fewer refs than the budget allows.
	lookahead func(maxChunks int, maxBytes int64) []store.ChunkRef
}

// newFramePrefetcher returns nil when batching is not wired, which makes every
// method a no-op and leaves the per-chunk path exactly as it was.
func newFramePrefetcher(st *store.ChunkStore, logger *slog.Logger) *framePrefetcher {
	if st == nil || !st.CanBatchFetch() {
		return nil
	}
	return &framePrefetcher{st: st, logger: logger, maxBytes: MaxPrefetchBytes, maxChunks: MaxPrefetchChunks}
}

// ensure makes the frame for res[i] available if it can, fetching a window
// that starts there. Failures are logged and left to the per-chunk path.
func (p *framePrefetcher) ensure(res []chunkResolution, i int) {
	if p == nil || i >= len(res) || !res[i].found {
		return
	}
	if _, ok := p.frames[res[i].ref.ChunkLoc]; ok {
		return
	}

	seen := make(map[store.ChunkLoc]bool)
	var batch []store.ChunkRef
	var est int64
	add := func(ref store.ChunkRef) bool {
		if seen[ref.ChunkLoc] || p.st.HasFrame(ref.PackNum, ref.StoreOffset) {
			return true // already covered; keep planning
		}
		size := int64(ref.RawLen) + prefetchFrameSlack
		if len(batch) > 0 && (est+size > p.maxBytes || len(batch) >= p.maxChunks) {
			return false
		}
		seen[ref.ChunkLoc] = true
		batch = append(batch, ref)
		est += size
		return true
	}

	full := false
	for j := i; j < len(res); j++ {
		if !res[j].found || res[j].err != nil {
			continue
		}
		if !add(res[j].ref) {
			full = true
			break
		}
	}
	if !full && p.lookahead != nil {
		for _, ref := range p.lookahead(p.maxChunks-len(batch), p.maxBytes-est) {
			if !add(ref) {
				break
			}
		}
	}
	if len(batch) == 0 {
		return
	}

	// Drop the previous window BEFORE fetching: at most one window's frames
	// are ever live, which is the memory bound this design promises.
	p.frames = make(map[store.ChunkLoc][]byte, len(batch))
	frames, err := p.st.FetchBatch(batch)
	if err != nil {
		// Partial results are still usable; whatever is missing falls back to
		// individual reads, which surface the real error per chunk.
		p.logger.Warn("batched chunk fetch failed; falling back to per-chunk reads",
			"chunks", len(batch), "fetched", len(frames), "error", err)
	}
	for loc, f := range frames {
		p.frames[loc] = f
	}
}

// frame returns a pre-fetched frame, if the window holds it.
func (p *framePrefetcher) frame(loc store.ChunkLoc) ([]byte, bool) {
	if p == nil {
		return nil, false
	}
	f, ok := p.frames[loc]
	return f, ok
}

// release drops the window (end of a file restore).
func (p *framePrefetcher) release() {
	if p != nil {
		p.frames = nil
	}
}

// fileLookahead walks the files a restore has not reached yet and yields their
// chunk locations, so one batch can cover several small files. It only handles
// plain stream files resolved against the backup's own entry accessor: files
// that redirect to another backup (unchanged-file chains), volume-extent files,
// inline files and excluded files are skipped and batch on their own terms.
type fileLookahead struct {
	files   []manifest.FileEntry
	entries manifest.EntryAccessor
	idx     *index.DedupIndex
	next    int
}

// advanceTo marks files up to and including i as reached.
func (l *fileLookahead) advanceTo(i int) {
	if l != nil && l.next <= i {
		l.next = i + 1
	}
}

// refs yields chunk refs from following files, within the given budget.
func (l *fileLookahead) refs(maxChunks int, maxBytes int64) []store.ChunkRef {
	if l == nil || maxChunks <= 0 || maxBytes <= 0 {
		return nil
	}
	var out []store.ChunkRef
	var est int64
	for l.next < len(l.files) && len(out) < maxChunks && est < maxBytes {
		f := l.files[l.next]
		l.next++
		if f.IsExcluded || f.Unchanged || f.InlineData != nil || f.VolumeExtents != nil || f.StreamLength == 0 {
			continue
		}
		startIdx, err := manifest.SearchEntries(l.entries, f.StreamOffset)
		if err != nil {
			continue
		}
		endIdx, err := manifest.SearchEntriesEnd(l.entries, f.StreamOffset+f.StreamLength)
		if err != nil {
			continue
		}
		chunk, err := l.entries.Range(startIdx, endIdx)
		if err != nil {
			continue
		}
		for _, e := range chunk {
			if len(out) >= maxChunks || est >= maxBytes {
				break
			}
			ie, ok, lerr := l.idx.LookupDirect(e.ChunkHash)
			if lerr != nil || !ok {
				continue
			}
			out = append(out, store.ChunkRef{
				ChunkLoc: store.ChunkLoc{PackNum: ie.PackNumber, StoreOffset: int64(ie.StoreOffset)},
				RawLen:   int(ie.ChunkLength),
			})
			est += int64(ie.ChunkLength) + prefetchFrameSlack
		}
	}
	return out
}
