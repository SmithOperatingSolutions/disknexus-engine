// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

// #204: batching must not be bought with memory. The prefetcher fetches in
// windows and holds AT MOST one window of frames, so a huge file (or a huge
// pack set) never lands in the heap all at once.

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func TestPrefetchWindowBoundsMemory(t *testing.T) {
	cfg := config.Default()
	cs, err := store.NewChunkStore(filepath.Join(t.TempDir(), "repo"), cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	const (
		chunks    = 400
		rawLen    = 8 * 1024
		budget    = 64 * 1024 // tiny window: forces many rounds
		frameSize = 128       // stand-in frame payload; only its size matters here
	)

	var batchSizes []int
	var batchBytes []int64
	cs.OnChunkFetchBatch = func(refs []store.ChunkRef) (map[store.ChunkLoc][]byte, error) {
		var est int64
		out := make(map[store.ChunkLoc][]byte, len(refs))
		for _, r := range refs {
			est += int64(r.RawLen) + prefetchFrameSlack
			out[r.ChunkLoc] = make([]byte, frameSize)
		}
		batchSizes = append(batchSizes, len(refs))
		batchBytes = append(batchBytes, est)
		return out, nil
	}

	pf := newFramePrefetcher(cs, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if pf == nil {
		t.Fatal("newFramePrefetcher returned nil with a batch fetcher wired")
	}
	pf.maxBytes = budget

	res := make([]chunkResolution, chunks)
	for i := range res {
		res[i] = chunkResolution{
			ref:   store.ChunkRef{ChunkLoc: store.ChunkLoc{PackNum: uint32(i / 50), StoreOffset: int64(i) * rawLen}, RawLen: rawLen},
			found: true,
		}
	}

	// Walk the chunks the way the restore loop does.
	for i := range res {
		pf.ensure(res, i)
		if _, ok := pf.frame(res[i].ref.ChunkLoc); !ok {
			t.Fatalf("chunk %d was not supplied by the prefetcher", i)
		}
		if held := len(pf.frames) * frameSize; int64(held) > budget {
			t.Fatalf("prefetcher holds %d bytes of frames at chunk %d; window budget is %d", held, i, budget)
		}
	}

	if len(batchSizes) < 2 {
		t.Fatalf("expected several windows for %d chunks at a %d-byte budget, got %d batch(es)", chunks, budget, len(batchSizes))
	}
	for i, b := range batchBytes {
		if b > budget && batchSizes[i] > 1 {
			t.Errorf("window %d estimated %d bytes, over the %d budget (%d chunks)", i, b, budget, batchSizes[i])
		}
	}
	// Batching must still be real: far fewer round trips than chunks.
	if len(batchSizes) >= chunks {
		t.Errorf("%d batches for %d chunks — no batching happened", len(batchSizes), chunks)
	}
	t.Logf("%d windows for %d chunks (budget %d bytes)", len(batchSizes), chunks, budget)
}
