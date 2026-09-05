// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package retention

import (
	"testing"
	"time"
)

// #171 slice 1: PURE retention selection — the decision logic, fully
// separated from anything that can delete. Prime directive encoded here:
// the forget set may never include (a) the newest completed backup of any
// source, (b) any ancestor of a kept backup (incremental chains), (c)
// in-progress/suspended backups, (d) members of a machine snapshot that
// has any kept member. Selection is deterministic — dry-run parity is
// structural, not aspirational.

func ts(daysAgo int, hoursAgo int) time.Time {
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	return base.Add(-time.Duration(daysAgo)*24*time.Hour - time.Duration(hoursAgo)*time.Hour)
}

func stdPolicy() Policy {
	return Policy{
		WithinWeek: 15 * time.Minute, AfterWeek: 24 * time.Hour,
		After90Days: 7 * 24 * time.Hour, AfterYear: 30 * 24 * time.Hour,
	}
}

func TestSelectKeepsNewestPerSource(t *testing.T) {
	backups := []BackupMeta{
		{ID: "old", Source: "C:", Status: "completed", CompletedAt: ts(400, 0)},
		{ID: "new", Source: "C:", Status: "completed", CompletedAt: ts(399, 0)},
		{ID: "d-only", Source: "D:", Status: "completed", CompletedAt: ts(500, 0)},
	}
	res := Select(backups, stdPolicy(), ts(0, 0))
	if !res.Kept["new"] {
		t.Fatal("newest completed of C: must always be kept")
	}
	if !res.Kept["d-only"] {
		t.Fatal("sole backup of D: must be kept regardless of age")
	}
}

func TestSelectLadderThinning(t *testing.T) {
	// 8 backups of one source, 15 days old, 1 hour apart: the after-week
	// tier keeps one per 24h bucket — so exactly one survives the ladder
	// (plus nothing else from that day), and the newest overall is kept.
	var backups []BackupMeta
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for i, id := range ids {
		backups = append(backups, BackupMeta{
			ID: id, Source: "C:", Status: "completed", CompletedAt: ts(15, i)})
	}
	backups = append(backups, BackupMeta{ID: "fresh", Source: "C:", Status: "completed", CompletedAt: ts(0, 1)})
	res := Select(backups, stdPolicy(), ts(0, 0))
	keptOld := 0
	for _, id := range ids {
		if res.Kept[id] {
			keptOld++
		}
	}
	if keptOld != 1 {
		t.Fatalf("after-week tier must thin to one per day bucket, kept %d", keptOld)
	}
	if len(res.Forget) != len(ids)-1 {
		t.Fatalf("forget set = %d, want %d", len(res.Forget), len(ids)-1)
	}
}

func TestSelectNeverOrphansChains(t *testing.T) {
	// parent <- child(kept): the parent is ladder-forgettable but chain
	// integrity must keep it. grandparent too.
	backups := []BackupMeta{
		{ID: "gp", Source: "C:", Status: "completed", CompletedAt: ts(20, 2)},
		{ID: "p", Source: "C:", Status: "completed", CompletedAt: ts(20, 1), ParentID: "gp"},
		{ID: "c", Source: "C:", Status: "completed", CompletedAt: ts(0, 1), ParentID: "p"},
	}
	res := Select(backups, stdPolicy(), ts(0, 0))
	if !res.Kept["c"] || !res.Kept["p"] || !res.Kept["gp"] {
		t.Fatalf("chain must be kept whole: %+v", res.Kept)
	}
	if len(res.Forget) != 0 {
		t.Fatalf("nothing is forgettable here, got %v", res.Forget)
	}
}

func TestSelectProtectsActiveAndSnapshots(t *testing.T) {
	backups := []BackupMeta{
		{ID: "run", Source: "C:", Status: "in_progress", CompletedAt: time.Time{}, StartedAt: ts(30, 0)},
		{ID: "m1", Source: "C:", Status: "completed", CompletedAt: ts(30, 1), SnapshotID: "snap"},
		{ID: "m2", Source: "EFI", Status: "completed", CompletedAt: ts(30, 1), SnapshotID: "snap"},
		{ID: "m3", Source: "D:", Status: "completed", CompletedAt: ts(30, 1), SnapshotID: "snap"},
		// A newer snapshot so "snap" members are ladder-forgettable per source.
		{ID: "n1", Source: "C:", Status: "completed", CompletedAt: ts(0, 3), SnapshotID: "snap2"},
		{ID: "n2", Source: "EFI", Status: "completed", CompletedAt: ts(0, 3), SnapshotID: "snap2"},
		{ID: "n3", Source: "D:", Status: "completed", CompletedAt: ts(0, 3), SnapshotID: "snap2"},
	}
	res := Select(backups, stdPolicy(), ts(0, 0))
	if !res.Kept["run"] {
		t.Fatal("in-progress backups are untouchable")
	}
	// Old snapshot members: either the whole snapshot stays or the whole
	// snapshot goes — never a partial machine.
	full := res.Kept["m1"] && res.Kept["m2"] && res.Kept["m3"]
	none := !res.Kept["m1"] && !res.Kept["m2"] && !res.Kept["m3"]
	if !full && !none {
		t.Fatalf("partial machine snapshot survival: %+v", res.Kept)
	}
	if !res.Kept["n1"] || !res.Kept["n2"] || !res.Kept["n3"] {
		t.Fatal("newest snapshot members must be kept")
	}
}

func TestSelectSnapshotMemberKeptKeepsSiblings(t *testing.T) {
	// If any member is kept for ANY reason (here: newest of its source),
	// every sibling of that snapshot is kept too.
	backups := []BackupMeta{
		{ID: "s1", Source: "C:", Status: "completed", CompletedAt: ts(40, 0), SnapshotID: "snap"},
		{ID: "s2", Source: "RARE", Status: "completed", CompletedAt: ts(40, 0), SnapshotID: "snap"},
		{ID: "newerC", Source: "C:", Status: "completed", CompletedAt: ts(1, 0)},
	}
	res := Select(backups, stdPolicy(), ts(0, 0))
	if !res.Kept["s2"] {
		t.Fatal("s2 is the newest of source RARE — kept")
	}
	if !res.Kept["s1"] {
		t.Fatal("sibling of a kept snapshot member must be kept")
	}
}

// #208: two backups of one source that completed at the SAME instant (the
// controller used to store second-granularity timestamps, so sub-second
// backups tied routinely). Selection must not depend on the order the
// records happen to arrive in — "keep the newest" cannot mean "keep
// whichever one the store listed first".
func TestSelectTiedTimestampsIgnoreInputOrder(t *testing.T) {
	tie := ts(20, 0) // old enough that the ladder thins the pair to one
	mk := func(id string) BackupMeta {
		return BackupMeta{ID: id, Source: "C:", Status: "completed",
			StartedAt: tie, CompletedAt: tie}
	}
	forward := Select([]BackupMeta{mk("aaa"), mk("bbb")}, stdPolicy(), ts(0, 0))
	reverse := Select([]BackupMeta{mk("bbb"), mk("aaa")}, stdPolicy(), ts(0, 0))

	if len(forward.Forget) != len(reverse.Forget) {
		t.Fatalf("input order changed the forget set: %v vs %v", forward.Forget, reverse.Forget)
	}
	for i := range forward.Forget {
		if forward.Forget[i] != reverse.Forget[i] {
			t.Fatalf("input order changed the forget set: %v vs %v", forward.Forget, reverse.Forget)
		}
	}
	for id, why := range forward.Reasons {
		if reverse.Reasons[id] != why {
			t.Fatalf("input order changed why %q was kept: %q vs %q", id, why, reverse.Reasons[id])
		}
	}
	// And the tie must actually resolve: exactly one of the pair survives the
	// ladder bucket, or the tie-break did nothing.
	if len(forward.Forget) != 1 {
		t.Fatalf("tied pair in one ladder bucket must thin to one, forget=%v kept=%v",
			forward.Forget, forward.Reasons)
	}
}

// #208: when CompletedAt ties, StartedAt is real evidence of order — a repo
// admits one in-progress backup at a time, so the later start finished
// later. The tie-break must use it before falling back to the (arbitrary)
// ID, otherwise "newest" picks the lexicographically smaller ID.
func TestSelectTieBreakPrefersLaterStart(t *testing.T) {
	// Both completed inside the same second and were stored truncated.
	sameSecond := ts(20, 0)
	backups := []BackupMeta{
		// listed first, smaller ID, but started EARLIER: not the newest.
		{ID: "aaa", Source: "C:", Status: "completed",
			StartedAt: sameSecond.Add(-2 * time.Second), CompletedAt: sameSecond},
		{ID: "zzz", Source: "C:", Status: "completed",
			StartedAt: sameSecond.Add(-1 * time.Second), CompletedAt: sameSecond},
	}
	res := Select(backups, stdPolicy(), ts(0, 0))
	if res.Reasons["zzz"] != "newest-of-source" {
		t.Fatalf("later-started backup must be the newest of its source, reasons=%v", res.Reasons)
	}
	if res.Kept["aaa"] {
		t.Fatalf("earlier-started tie loser must not be kept, reasons=%v", res.Reasons)
	}
}

// The whole selection — not just the tie-break — must be a function of the
// backup SET, never of the order the store happened to list it in. Ties are
// where that used to leak, so this fixture is full of them, plus chains,
// snapshots and an active backup, and every permutation must agree (#208).
func TestSelectIsOrderIndependentWithTiesChainsAndSnapshots(t *testing.T) {
	// The tied group is the NEWEST of its source and sits in one ladder
	// bucket, so the tie is what decides both "newest-of-source" and the
	// bucket winner — exactly the spot where input order used to leak.
	tied := ts(0, 1)
	old := ts(30, 0)
	backups := []BackupMeta{
		{ID: "t1", Source: "C:", Status: "completed", StartedAt: tied, CompletedAt: tied},
		{ID: "t2", Source: "C:", Status: "completed", StartedAt: tied, CompletedAt: tied, ParentID: "anc"},
		{ID: "t3", Source: "C:", Status: "completed", StartedAt: tied, CompletedAt: tied},
		{ID: "anc", Source: "C:", Status: "completed", StartedAt: old, CompletedAt: old},
		// A tied pair inside a machine snapshot, on two other sources.
		{ID: "s1", Source: "D:", Status: "completed", StartedAt: tied, CompletedAt: tied, SnapshotID: "snap"},
		{ID: "s2", Source: "EFI", Status: "completed", StartedAt: tied, CompletedAt: tied, SnapshotID: "snap"},
		{ID: "s3", Source: "D:", Status: "completed", StartedAt: tied, CompletedAt: tied},
		{ID: "run", Source: "EFI", Status: "in_progress", StartedAt: ts(0, 0)},
	}
	perms := [][]BackupMeta{
		backups,
		reversed(backups),
		rotated(backups, 3),
		rotated(reversed(backups), 5),
	}
	want := Select(perms[0], stdPolicy(), ts(0, 0))
	for i, p := range perms {
		got := Select(p, stdPolicy(), ts(0, 0))
		if len(got.Forget) != len(want.Forget) {
			t.Fatalf("permutation %d changed the forget set: %v vs %v", i, got.Forget, want.Forget)
		}
		for j := range got.Forget {
			if got.Forget[j] != want.Forget[j] {
				t.Fatalf("permutation %d changed the forget set: %v vs %v", i, got.Forget, want.Forget)
			}
		}
		for id, why := range want.Reasons {
			if got.Reasons[id] != why {
				t.Fatalf("permutation %d changed why %q was kept: %q vs %q", i, id, why, got.Reasons[id])
			}
		}
		// Prime directive, re-checked on every permutation.
		if !got.Kept["run"] {
			t.Fatalf("permutation %d forgot an in-progress backup", i)
		}
		forgotten := map[string]bool{}
		for _, id := range got.Forget {
			forgotten[id] = true
		}
		for _, b := range p {
			if got.Kept[b.ID] && b.ParentID != "" && forgotten[b.ParentID] {
				t.Fatalf("permutation %d orphaned chain parent %q of kept %q", i, b.ParentID, b.ID)
			}
			if b.SnapshotID == "" || !got.Kept[b.ID] {
				continue
			}
			for _, sib := range p {
				if sib.SnapshotID == b.SnapshotID && forgotten[sib.ID] {
					t.Fatalf("permutation %d split machine snapshot %q", i, b.SnapshotID)
				}
			}
		}
		// Newest-per-source, stated honestly under ties: for every source,
		// SOME backup carrying that source's latest CompletedAt survives.
		latest := map[string]time.Time{}
		for _, b := range p {
			if b.Status == "completed" && b.CompletedAt.After(latest[b.Source]) {
				latest[b.Source] = b.CompletedAt
			}
		}
		keptLatest := map[string]bool{}
		for _, b := range p {
			if b.Status == "completed" && b.CompletedAt.Equal(latest[b.Source]) && got.Kept[b.ID] {
				keptLatest[b.Source] = true
			}
		}
		for src := range latest {
			if !keptLatest[src] {
				t.Fatalf("permutation %d forgot every newest backup of %q", i, src)
			}
		}
	}
}

func reversed(in []BackupMeta) []BackupMeta {
	out := make([]BackupMeta, 0, len(in))
	for i := len(in) - 1; i >= 0; i-- {
		out = append(out, in[i])
	}
	return out
}

func rotated(in []BackupMeta, n int) []BackupMeta {
	n %= len(in)
	return append(append([]BackupMeta{}, in[n:]...), in[:n]...)
}

func TestSelectDeterministic(t *testing.T) {
	var backups []BackupMeta
	for i := 0; i < 40; i++ {
		backups = append(backups, BackupMeta{
			ID: string(rune('A'+i%26)) + string(rune('a'+i/26)), Source: "C:",
			Status: "completed", CompletedAt: ts(i*3, i%5)})
	}
	a := Select(backups, stdPolicy(), ts(0, 0))
	b := Select(backups, stdPolicy(), ts(0, 0))
	if len(a.Forget) != len(b.Forget) {
		t.Fatal("selection must be deterministic")
	}
	for i := range a.Forget {
		if a.Forget[i] != b.Forget[i] {
			t.Fatal("forget order must be deterministic")
		}
	}
}
