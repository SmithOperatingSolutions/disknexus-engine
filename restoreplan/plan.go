// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package restoreplan (#157) decides, per pack, HOW a restore fetches its
// chunks: dense packs (many needed chunks) download whole and amortize;
// sparse packs (cross-machine-dedup scatter: a couple of chunks out of
// thousands) fetch per-chunk via ranged reads. The restore holds its full
// entry list before fetching, so the plan is exact, not heuristic-per-miss.
package restoreplan

import (
	"context"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Plan classifies packs for one restore.
type Plan struct {
	neededChunks map[uint32]int
	dense        map[uint32]bool
}

// Dense reports whether the pack should be downloaded whole.
func (p *Plan) Dense(pack uint32) bool { return p.dense[pack] }

// Build classifies packs from an in-memory entry slice; BuildFrom is the
// streaming form and the one a restore should call (#506).
func Build(entries []manifest.Entry, packOf func([32]byte) (uint32, bool), packMax int64) *Plan {
	return BuildFrom(manifest.NewSliceEntryAccessor(entries), packOf, packMax)
}

// PackSource is the pack-fetch surface Wire drives — exactly the four
// methods, no more. Defining it HERE (rather than importing the concrete
// *cloudsync.S3Session) is what keeps restoreplan inside the engine set
// (#542): the cloud layer satisfies this from the outside, the engine
// never names it. cloudsync.S3Session implements it as-is.
type PackSource interface {
	FetchChunkFrame(ctx context.Context, packNum uint32, offset int64) ([]byte, error)
	TouchPack(n uint32)
	DownloadPack(ctx context.Context, packNum uint32) error
	EvictLRUPacks(window int)
}

// Wire installs the fetch policy on a ChunkStore backed by a pack source:
// sparse packs fetch single frames via ranged reads; dense packs decline
// to the whole-pack path, which downloads with LRU eviction (window) and
// refreshes recency on every local hit.
func Wire(ctx context.Context, cs *store.ChunkStore, sess PackSource, plan *Plan, lruWindow int) {
	cs.OnChunkFetch = func(packNum uint32, offset int64) ([]byte, error) {
		if plan.Dense(packNum) {
			return nil, store.ErrChunkFetchDecline
		}
		return sess.FetchChunkFrame(ctx, packNum, offset)
	}
	cs.OnPackAccess = sess.TouchPack
	cs.OnPackMissing = func(n uint32) error {
		if err := sess.DownloadPack(ctx, n); err != nil {
			return err
		}
		sess.TouchPack(n)
		sess.EvictLRUPacks(lruWindow)
		return nil
	}
}
