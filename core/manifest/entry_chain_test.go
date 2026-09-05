// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"testing"
)

// chainFixture is three parts of unequal size (one empty, one nil) whose
// entries are distinguishable by VolumeOffset = global index × 4096 — so a
// wrong part or a wrong local index reads as the wrong offset, never as an
// accidental match.
func chainFixture(t *testing.T) (EntryAccessor, []Entry) {
	t.Helper()
	sizes := []int{5, 0, 7, 1}
	var all []Entry
	var parts []EntryAccessor
	for _, n := range sizes {
		var es []Entry
		for j := 0; j < n; j++ {
			i := int64(len(all))
			e := Entry{VolumeOffset: i * 4096, ChunkLength: 4096}
			e.ChunkHash[0] = byte(i)
			es = append(es, e)
			all = append(all, e)
		}
		parts = append(parts, NewSliceEntryAccessor(es))
	}
	parts = append(parts, nil) // a nil part is skipped, not dereferenced
	if len(all) != 13 {
		t.Fatalf("fixture builds %d entries, want 13", len(all))
	}
	return ChainEntryAccessor(parts...), all
}

func TestChainEntryAccessor_CountIsTheSum(t *testing.T) {
	ea, all := chainFixture(t)
	if got := ea.Count(); got != int64(len(all)) {
		t.Fatalf("Count = %d, want %d", got, len(all))
	}
	if got := ChainEntryAccessor().Count(); got != 0 {
		t.Fatalf("empty chain Count = %d, want 0", got)
	}
}

func TestChainEntryAccessor_AtCrossesEveryBoundary(t *testing.T) {
	ea, all := chainFixture(t)
	for i := range all {
		got, err := ea.At(int64(i))
		if err != nil {
			t.Fatalf("At(%d): %v", i, err)
		}
		if got != all[i] {
			t.Fatalf("At(%d) = offset %d, want %d", i, got.VolumeOffset, all[i].VolumeOffset)
		}
	}
	for _, bad := range []int64{-1, int64(len(all)), 1 << 40} {
		if _, err := ea.At(bad); err == nil {
			t.Fatalf("At(%d) returned no error", bad)
		}
	}
}

func TestChainEntryAccessor_RangeEqualsTheConcatenation(t *testing.T) {
	ea, all := chainFixture(t)
	n := int64(len(all))
	for start := int64(0); start <= n; start++ {
		for end := start; end <= n; end++ {
			got, err := ea.Range(start, end)
			if err != nil {
				t.Fatalf("Range(%d,%d): %v", start, end, err)
			}
			want := all[start:end]
			if len(got) != len(want) {
				t.Fatalf("Range(%d,%d) has %d entries, want %d", start, end, len(got), len(want))
			}
			for k := range want {
				if got[k] != want[k] {
					t.Fatalf("Range(%d,%d)[%d] = offset %d, want %d", start, end, k, got[k].VolumeOffset, want[k].VolumeOffset)
				}
			}
		}
	}
	for _, bad := range [][2]int64{{-1, 2}, {0, n + 1}, {5, 4}} {
		if _, err := ea.Range(bad[0], bad[1]); err == nil {
			t.Fatalf("Range(%d,%d) returned no error", bad[0], bad[1])
		}
	}
	if got, err := ChainEntryAccessor().Range(0, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty chain Range(0,0) = %v, %v", got, err)
	}
}
