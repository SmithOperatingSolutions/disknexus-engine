// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restoreplan

import (
	"context"
	"errors"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

type fakeSource struct {
	fetched  []uint32 // packs FetchChunkFrame was asked for
	touched  []uint32
	download []uint32
	evicted  []int
	frame    []byte
	dlErr    error
}

func (f *fakeSource) FetchChunkFrame(_ context.Context, packNum uint32, _ int64) ([]byte, error) {
	f.fetched = append(f.fetched, packNum)
	return f.frame, nil
}
func (f *fakeSource) TouchPack(n uint32) { f.touched = append(f.touched, n) }
func (f *fakeSource) DownloadPack(_ context.Context, n uint32) error {
	f.download = append(f.download, n)
	return f.dlErr
}
func (f *fakeSource) EvictLRUPacks(window int) { f.evicted = append(f.evicted, window) }

// Wire is the fetch policy every cloud restore runs under (#157): a chunk
// in a DENSE pack declines the ranged fetch so the whole pack downloads
// once and serves every chunk locally; a chunk in a SPARSE pack is fetched
// as a single frame. A missing pack is downloaded, touched (LRU recency)
// and the window enforced — in that order, so the pack just fetched is the
// newest and never the one evicted.
func TestWireRoutesDenseToPacksAndSparseToFrames(t *testing.T) {
	repo := t.TempDir()
	if err := store.InitRepo(repo, store.RepoConfig{}); err != nil {
		t.Fatal(err)
	}
	cs, err := store.NewChunkStore(repo, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	// Pack 7 is dense (many chunks needed), pack 8 sparse (one).
	entries := make([]manifest.Entry, 0, 101)
	for i := 0; i < 100; i++ {
		e := manifest.Entry{VolumeOffset: int64(i) * 4096, ChunkLength: 4096}
		e.ChunkHash[0], e.ChunkHash[1] = 7, byte(i)
		entries = append(entries, e)
	}
	sparse := manifest.Entry{VolumeOffset: 100 * 4096, ChunkLength: 4096}
	sparse.ChunkHash[0] = 8
	entries = append(entries, sparse)
	packOf := func(h [32]byte) (uint32, bool) { return uint32(h[0]), true }
	// Dense means "at least 1/64 of a pack's chunk capacity is needed": with
	// 4 KB chunks a 13 MB pack holds 3200, so the threshold is 50 — pack 7's
	// 100 chunks are dense, pack 8's one is not.
	plan := Build(entries, packOf, 4096*3200)
	if !plan.Dense(7) || plan.Dense(8) || plan.Dense(9) {
		t.Fatalf("plan: dense(7)=%v dense(8)=%v dense(9)=%v, want true/false/false", plan.Dense(7), plan.Dense(8), plan.Dense(9))
	}

	src := &fakeSource{frame: []byte("frame")}
	Wire(context.Background(), cs, src, plan, 3)

	if _, err := cs.OnChunkFetch(7, 0); !errors.Is(err, store.ErrChunkFetchDecline) {
		t.Fatalf("dense pack: OnChunkFetch returned %v, want ErrChunkFetchDecline (the whole-pack path)", err)
	}
	if len(src.fetched) != 0 {
		t.Fatal("dense pack: a ranged frame fetch was issued")
	}
	got, err := cs.OnChunkFetch(8, 4096)
	if err != nil || string(got) != "frame" || len(src.fetched) != 1 || src.fetched[0] != 8 {
		t.Fatalf("sparse pack: got %q err %v fetched %v, want the frame from one ranged fetch of pack 8", got, err, src.fetched)
	}

	cs.OnPackAccess(7)
	if len(src.touched) != 1 || src.touched[0] != 7 {
		t.Fatalf("OnPackAccess did not touch the pack: %v", src.touched)
	}
	src.touched = nil
	if err := cs.OnPackMissing(7); err != nil {
		t.Fatal(err)
	}
	if len(src.download) != 1 || src.download[0] != 7 || len(src.touched) != 1 || src.touched[0] != 7 || len(src.evicted) != 1 || src.evicted[0] != 3 {
		t.Fatalf("OnPackMissing: download %v touch %v evict %v — want download 7, touch 7, evict window 3", src.download, src.touched, src.evicted)
	}
	// A failed download is the caller's error, and nothing is touched or
	// evicted on its behalf.
	src.dlErr = errors.New("bucket unreachable")
	src.touched, src.evicted = nil, nil
	if err := cs.OnPackMissing(9); err == nil || len(src.touched) != 0 || len(src.evicted) != 0 {
		t.Fatalf("failed download: err=%v touched=%v evicted=%v", err, src.touched, src.evicted)
	}
}
