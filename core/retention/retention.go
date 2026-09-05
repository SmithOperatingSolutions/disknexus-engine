// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package retention holds the PURE retention-selection logic for #171 —
// the decision layer, deliberately incapable of deleting anything. The
// mark/sweep engine consumes its output; dry-run and real runs share this
// exact code path, so their answers cannot diverge.
//
// Prime directive (enforced here, tested exhaustively): the forget set
// never contains the newest completed backup of any source, any ancestor
// of a kept backup, an in-progress/suspended backup, or a member of a
// machine snapshot that has any kept member.
package retention

import (
	"sort"
	"time"
)

// BackupMeta is the selection view of one backup.
type BackupMeta struct {
	ID          string
	Source      string // source volume/path — the per-source ladder key
	Status      string // completed | in_progress | suspended | failed...
	StartedAt   time.Time
	CompletedAt time.Time
	ParentID    string // incremental parent ("" = full)
	SnapshotID  string // machine snapshot membership ("" = standalone)
}

// Policy is the keep-a-version ladder: within each age tier, one backup
// per cadence bucket survives (the newest in the bucket). Zero cadence in
// a tier means "keep everything in that tier".
type Policy struct {
	WithinWeek  time.Duration // age < 7d
	AfterWeek   time.Duration // 7d <= age < 90d
	After90Days time.Duration // 90d <= age < 365d
	AfterYear   time.Duration // age >= 365d
}

// Result is the deterministic selection outcome. Kept ∪ Forget covers
// every input backup exactly once; Forget is sorted for stable output.
type Result struct {
	Kept   map[string]bool
	Forget []string
	// Reasons maps kept IDs to why they were kept (ladder, newest-of-source,
	// chain-ancestor, active, snapshot-sibling) — surfaced by dry-run.
	Reasons map[string]string
}

// newer is the TOTAL order on backups used everywhere "newest" is decided
// (#208). Equal CompletedAt values used to leave the winner up to map/slice
// order — the controller stored second-granularity timestamps, so two fast
// backups of one source tied routinely and "keep the newest" silently
// became "keep an arbitrary one of the tied group".
//
// The key, in order:
//  1. CompletedAt — the real signal.
//  2. StartedAt — real evidence too: a repo admits one in-progress backup
//     at a time, so of two backups that finished in the same instant the
//     one that STARTED later is the later one.
//  3. ID, lexicographically SMALLEST first. IDs are UUIDs and carry no
//     time meaning; this last step exists only to make the answer
//     reproducible. It keeps the pre-#208 ladder ordering unchanged.
//
// Both callers use this one comparator, so newest-of-source and the ladder
// can never disagree about which member of a tied group is the newest.
func newer(a, b BackupMeta) bool {
	if !a.CompletedAt.Equal(b.CompletedAt) {
		return a.CompletedAt.After(b.CompletedAt)
	}
	if !a.StartedAt.Equal(b.StartedAt) {
		return a.StartedAt.After(b.StartedAt)
	}
	return a.ID < b.ID
}

func (p Policy) cadenceFor(age time.Duration) time.Duration {
	switch {
	case age < 7*24*time.Hour:
		return p.WithinWeek
	case age < 90*24*time.Hour:
		return p.AfterWeek
	case age < 365*24*time.Hour:
		return p.After90Days
	default:
		return p.AfterYear
	}
}

// Select computes the keep/forget partition at the given evaluation time.
func Select(backups []BackupMeta, policy Policy, now time.Time) Result {
	res := Result{Kept: map[string]bool{}, Reasons: map[string]string{}}
	byID := make(map[string]BackupMeta, len(backups))
	for _, b := range backups {
		byID[b.ID] = b
	}
	keep := func(id, reason string) {
		if !res.Kept[id] {
			res.Kept[id] = true
			res.Reasons[id] = reason
		}
	}

	// 1. Untouchables: anything not terminally completed.
	for _, b := range backups {
		if b.Status != "completed" {
			keep(b.ID, "active")
		}
	}

	// 2. Newest completed per source: always kept.
	newest := map[string]BackupMeta{}
	for _, b := range backups {
		if b.Status != "completed" {
			continue
		}
		if cur, ok := newest[b.Source]; !ok || newer(b, cur) {
			newest[b.Source] = b
		}
	}
	for _, b := range newest {
		keep(b.ID, "newest-of-source")
	}

	// 3. Ladder per source: within each tier, keep the newest per cadence
	// bucket. Deterministic: sorted newest-first by the total order above
	// (so ties resolve the same way step 2 resolved them), bucket index by
	// floor division of age.
	bySource := map[string][]BackupMeta{}
	for _, b := range backups {
		if b.Status == "completed" {
			bySource[b.Source] = append(bySource[b.Source], b)
		}
	}
	for _, list := range bySource {
		sort.Slice(list, func(i, j int) bool { return newer(list[i], list[j]) })
		type bucketKey struct {
			cadence time.Duration
			idx     int64
		}
		seen := map[bucketKey]bool{}
		for _, b := range list {
			age := now.Sub(b.CompletedAt)
			cad := policy.cadenceFor(age)
			if cad <= 0 {
				keep(b.ID, "ladder")
				continue
			}
			k := bucketKey{cad, int64(age / cad)}
			if !seen[k] {
				seen[k] = true
				keep(b.ID, "ladder")
			}
		}
	}

	// 4. Chain integrity: every ancestor of a kept backup is kept. Iterate
	// to fixpoint (chains are short; snapshots below can re-trigger).
	// 5. Snapshot atomicity: any kept member keeps every sibling.
	snapMembers := map[string][]string{}
	for _, b := range backups {
		if b.SnapshotID != "" {
			snapMembers[b.SnapshotID] = append(snapMembers[b.SnapshotID], b.ID)
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range res.Kept {
			for pid := byID[id].ParentID; pid != ""; pid = byID[pid].ParentID {
				if !res.Kept[pid] {
					keep(pid, "chain-ancestor")
					changed = true
				}
			}
			if sid := byID[id].SnapshotID; sid != "" {
				for _, sib := range snapMembers[sid] {
					if !res.Kept[sib] {
						keep(sib, "snapshot-sibling")
						changed = true
					}
				}
			}
		}
	}

	// Everything else is forgettable — sorted for determinism.
	for _, b := range backups {
		if !res.Kept[b.ID] {
			res.Forget = append(res.Forget, b.ID)
		}
	}
	sort.Strings(res.Forget)
	return res
}
