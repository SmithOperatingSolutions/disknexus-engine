// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The fold honors the batch budget it is HANDED (#542, #507). The budget
// used to come from DISKNEXUS_COMPACT_FOLD_BATCH_MB read inside the engine
// and was never tested; it is a parameter now, and this is the test the
// knob always needed: a tiny budget must cost many merge passes over the
// same deltas that the default folds in one. The observer reports each
// pass's slab, so the pass count is the authority (§3), and the fixture is
// interrogated (§2): enough records that "many passes" is not "two".
func foldFixture(t *testing.T, deltas, perDelta int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, DeltaSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	n := 0
	for d := 0; d < deltas; d++ {
		delta := &Delta{}
		for i := 0; i < perDelta; i++ {
			n++
			var h [32]byte
			binary.BigEndian.PutUint64(h[:], uint64(n))
			delta.Entries = append(delta.Entries, DeltaEntry{StrongHash: h, WeakHash: uint64(n) * 7919, PackNumber: uint32(d), StoreOffset: uint64(i) * 4096, ChunkLength: 4096})
		}
		name := filepath.Join(dir, DeltaSubdir, "0000000"+string(rune('a'+d))+".delta")
		if err := os.WriteFile(name, delta.Marshal(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func countPasses(t *testing.T, dir string, budget int64) (passes int, slabBytes int64) {
	t.Helper()
	FoldPassObserver = func(slab, _ int64) { passes++; slabBytes += slab }
	defer func() { FoldPassObserver = nil }()
	if _, err := FoldDeltasStreamedWithBudget(dir, 10_000, 0.001, budget); err != nil {
		t.Fatalf("fold with budget %d: %v", budget, err)
	}
	return passes, slabBytes
}

func TestFoldHonorsTheBatchBudgetItIsHanded(t *testing.T) {
	const deltas, perDelta = 4, 50
	records := int64(deltas * perDelta)

	// Default budget: everything fits one pass.
	one, bytesOne := countPasses(t, foldFixture(t, deltas, perDelta), DefaultFoldBatchBudget)
	if bytesOne < records*foldRecSize {
		t.Fatalf("fixture defect: the fold saw %d slab bytes, fewer than %d records' worth — the deltas were not folded (§2)", bytesOne, records)
	}
	if one != 1 {
		t.Fatalf("default budget folded %d records in %d passes, want 1 — the fixture is not small enough to be the control", records, one)
	}

	// A budget of three records: the same deltas must cost ~records/3 passes.
	tiny := int64(3 * foldRecSize)
	many, _ := countPasses(t, foldFixture(t, deltas, perDelta), tiny)
	if want := int(records / 3); many < want {
		t.Fatalf("a %d-byte batch budget folded %d records in %d passes, want at least %d — the engine is not honoring the budget it was handed, "+
			"so a pod that sized the fold for its memory gets the default slab regardless", tiny, records, many, want)
	}
}
