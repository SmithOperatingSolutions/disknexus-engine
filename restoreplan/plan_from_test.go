// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restoreplan

import (
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// TestBuildFrom_ChainedBatchesMatchTheSlicePlan: a plan built through an
// accessor in planBatch-sized reads, across part boundaries of a chain, is
// the plan Build makes over the flat slice. N is 3 batches plus a ragged
// tail so every batch edge — and the two part edges, placed off the batch
// edges — is crossed; a plan that drops or double-counts an entry at any
// of them changes a pack's chunk count and fails the deep comparison.
func TestBuildFrom_ChainedBatchesMatchTheSlicePlan(t *testing.T) {
	const n = 3*planBatch + 7
	entries := make([]manifest.Entry, n)
	for i := range entries {
		e := &entries[i]
		binary.LittleEndian.PutUint64(e.ChunkHash[:], uint64(i))
		e.ChunkHash[8] = byte(i % 251) // pack
		e.VolumeOffset = int64(i) * 4096
		e.ChunkLength = 4096 + i%3
		e.IsExcluded = i%97 == 0
	}
	packOf := func(h [32]byte) (uint32, bool) {
		if h[0] == 0xAA { // some chunks unknown to the index
			return 0, false
		}
		return uint32(h[8]), true
	}
	const packMax = 64 << 20

	want := Build(entries, packOf, packMax)
	if len(want.neededChunks) == 0 || len(want.dense) == 0 {
		t.Fatalf("fixture plan is vacuous: %d packs needed, %d dense", len(want.neededChunks), len(want.dense))
	}

	// Independent authority for the per-pack counts: Build delegates to
	// BuildFrom, so slice-vs-chain equality alone cannot see a batching
	// defect they would share (a dropped entry at every batch edge survived
	// exactly that comparison). Count the packs by hand over the flat slice.
	counted := map[uint32]int{}
	for _, e := range entries {
		if e.IsExcluded {
			continue
		}
		if pk, ok := packOf(e.ChunkHash); ok {
			counted[pk]++
		}
	}
	if !reflect.DeepEqual(want.neededChunks, counted) {
		for pk, c := range counted {
			if want.neededChunks[pk] != c {
				t.Fatalf("pack %d: plan counts %d chunks, a direct count finds %d", pk, want.neededChunks[pk], c)
			}
		}
		t.Fatalf("plan needs %d packs, a direct count finds %d", len(want.neededChunks), len(counted))
	}

	// Part edges at 1000 and 100_003: off every batch edge.
	chain := manifest.ChainEntryAccessor(
		manifest.NewSliceEntryAccessor(entries[:1000]),
		manifest.NewSliceEntryAccessor(entries[1000:100003]),
		manifest.NewSliceEntryAccessor(entries[100003:]),
	)
	got := BuildFrom(chain, packOf, packMax)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chained plan differs from the slice plan:\n needed got %d packs want %d\n dense got %d want %d",
			len(got.neededChunks), len(want.neededChunks), len(got.dense), len(want.dense))
	}

	// Anti-vacuity: the comparison can tell plans apart.
	entries[n-1].IsExcluded = !entries[n-1].IsExcluded
	if reflect.DeepEqual(Build(entries, packOf, packMax), want) {
		t.Fatal("flipping one entry left the plan equal — the deep comparison is blind")
	}
}
