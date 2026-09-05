// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package forget implements retention policy for disknexus: selecting which
// backups to keep per a restic-style keep-policy and, crucially, protecting any
// backup that a kept backup still references (incremental parents, watcher
// unchanged-file data pointers) so retention can never make a surviving backup
// unrestorable.
//
// forget.go is pure: it operates only on []manifest.Backup values and an
// injected catalog loader, with no I/O and no encryption. It is the unit-test
// seam for the policy and reference-promotion algorithms. Orchestration
// (enumeration, deletion, prune) lives in run.go.
package forget

import (
	"fmt"
	"sort"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// CalDur is a calendar duration (restic --keep-within semantics): months and
// years are calendar-relative, not fixed-length, so they are applied with
// time.AddDate rather than a fixed number of hours.
type CalDur struct {
	Years, Months, Days, Hours int
}

// IsZero reports whether the duration selects nothing.
func (d CalDur) IsZero() bool { return d == CalDur{} }

// Policy is a retention keep-policy. Count rules (Last/Hourly/.../Yearly) select
// the N most recent distinct periods; Within keeps everything newer than a
// calendar window anchored to Now. A rule with count 0 is disabled.
type Policy struct {
	Last, Hourly, Daily, Weekly, Monthly, Yearly int
	Within                                       CalDur
	HasWithin                                    bool

	// Now anchors the Within window (wall clock; injected so tests are
	// deterministic). Loc sets bucket boundaries for the period rules
	// (injected; the CLI defaults it to time.UTC — never ambient time.Local).
	Now time.Time
	Loc *time.Location
}

// Any reports whether the policy selects anything at all. A policy that selects
// nothing must be refused by the caller (never expire every backup).
func (p Policy) Any() bool {
	return p.Last > 0 || p.Hourly > 0 || p.Daily > 0 || p.Weekly > 0 ||
		p.Monthly > 0 || p.Yearly > 0 || (p.HasWithin && !p.Within.IsZero())
}

// needsTime reports whether any active rule reasons over timestamps (so a
// zero/absent timestamp is undefined and must be rejected).
func (p Policy) needsTime() bool {
	return p.Hourly > 0 || p.Daily > 0 || p.Weekly > 0 || p.Monthly > 0 || p.Yearly > 0 ||
		(p.HasWithin && !p.Within.IsZero())
}

func (p Policy) loc() *time.Location {
	if p.Loc != nil {
		return p.Loc
	}
	return time.UTC
}

// byRecency sorts backups newest-first with a deterministic BackupID tie-break,
// so equal timestamps (and zero timestamps under an ordinal-only policy) yield
// a stable, reproducible ordering independent of input order.
func byRecency(bs []manifest.Backup) []manifest.Backup {
	out := make([]manifest.Backup, len(bs))
	copy(out, bs)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return out[i].BackupID > out[j].BackupID
	})
	return out
}

// selectByPolicy returns, for each kept backup, the ordered list of reasons it
// was kept (e.g. ["last","daily"]). A backup absent from the map is not
// policy-kept. Pure and deterministic.
//
// This keep set is NOT yet deletion-safe — see Protect, which promotes the
// referenced ancestors of these backups.
func selectByPolicy(bs []manifest.Backup, p Policy) (map[string][]string, error) {
	desc := byRecency(bs)

	if p.needsTime() {
		for _, b := range desc {
			if b.Timestamp.IsZero() {
				return nil, fmt.Errorf("backup %s has no timestamp; cannot apply a time-based keep rule (only --keep-last is defined for timestamp-less backups)", short(b.BackupID))
			}
		}
	}

	reasons := make(map[string][]string)
	add := func(id, reason string) {
		reasons[id] = append(reasons[id], reason)
	}

	// keep-last: the N most recent backups outright.
	for i := 0; i < p.Last && i < len(desc); i++ {
		add(desc[i].BackupID, "last")
	}

	loc := p.loc()
	// Each periodic rule keeps the NEWEST backup of each of the N most recent
	// distinct periods (restic semantics: gaps do not consume a slot).
	type rule struct {
		name string
		n    int
		key  func(t time.Time) int64
	}
	rules := []rule{
		{"hourly", p.Hourly, func(t time.Time) int64 {
			y, _, _ := t.Date()
			return int64(y)*1000000 + int64(t.YearDay())*100 + int64(t.Hour())
		}},
		{"daily", p.Daily, func(t time.Time) int64 {
			y, _, _ := t.Date()
			return int64(y)*1000 + int64(t.YearDay())
		}},
		{"weekly", p.Weekly, func(t time.Time) int64 {
			isoY, isoW := t.ISOWeek()
			return int64(isoY)*100 + int64(isoW)
		}},
		{"monthly", p.Monthly, func(t time.Time) int64 {
			y, m, _ := t.Date()
			return int64(y)*100 + int64(m)
		}},
		{"yearly", p.Yearly, func(t time.Time) int64 {
			return int64(t.Year())
		}},
	}
	for _, r := range rules {
		if r.n <= 0 {
			continue
		}
		kept := 0
		haveLast := false
		var lastKey int64
		for _, b := range desc {
			if kept >= r.n {
				break
			}
			k := r.key(b.Timestamp.In(loc))
			if !haveLast || k != lastKey {
				add(b.BackupID, r.name)
				lastKey = k
				haveLast = true
				kept++
			}
		}
	}

	// keep-within: everything not older than the calendar window from Now.
	if p.HasWithin && !p.Within.IsZero() {
		cutoff := p.Now.AddDate(-p.Within.Years, -p.Within.Months, -p.Within.Days).
			Add(-time.Duration(p.Within.Hours) * time.Hour)
		for _, b := range desc {
			if !b.Timestamp.Before(cutoff) {
				add(b.BackupID, "within")
			}
		}
	}

	return reasons, nil
}

// CatalogLoader loads a backup's metadata + FileCatalog (no entries). Injected
// so Protect is unit-testable against synthetic reference graphs; the CLI
// passes a closure over manifest.LoadCatalog.
type CatalogLoader func(id string) (*manifest.Backup, error)

// Dangling records a reference to a backup that does not physically exist
// (pre-existing corruption, e.g. a legacy per-backup delete removed a middle
// backup). It cannot be protected — it is already gone — and is never a delete
// candidate; forget warns rather than aborting.
type Dangling struct {
	From, To, Via string
}

type refEdge struct {
	ID, Via string
}

// referenceEdges is the single source of cross-backup reference edges — the
// mirror of the export ancestor BFS and prune's DataBackupID walk. If a future
// manifest format adds a cross-backup pointer, add it here only.
func referenceEdges(b *manifest.Backup) []refEdge {
	var edges []refEdge
	if b.ParentBackupID != "" {
		edges = append(edges, refEdge{b.ParentBackupID, "parent"})
	}
	seen := make(map[string]bool)
	for i := range b.FileCatalog {
		d := b.FileCatalog[i].DataBackupID
		if d != "" && !seen[d] {
			seen[d] = true
			edges = append(edges, refEdge{d, "data"})
		}
	}
	return edges
}

// Protect computes the transitive reference closure of the policy keep set and
// promotes every referenced ancestor to "keep", so deleting allIDs\protected
// cannot make any surviving backup unrestorable.
//
//   - allIDs: every backup ID that physically exists (raw dir scan).
//   - keepReasons: the policy keep set from selectByPolicy.
//   - load: reads a backup's catalog (for its reference edges).
//
// Returns the protected set (keep reasons, incl. "promoted:..." for ancestors),
// the dangling references found, and an error only if a backup that is IN the
// closure cannot be read (its edges are needed to prove safety). A corrupt
// manifest OUTSIDE the closure is never loaded and stays deletable — this keeps
// forget available on repos with a pre-existing broken chain.
func Protect(allIDs map[string]bool, keepReasons map[string][]string, load CatalogLoader) (map[string][]string, []Dangling, error) {
	protected := make(map[string][]string, len(keepReasons))
	var queue []string
	for id, r := range keepReasons {
		cp := make([]string, len(r))
		copy(cp, r)
		protected[id] = cp
		queue = append(queue, id)
	}
	// Deterministic dequeue order (map iteration is random) so dangling/report
	// output is stable.
	sort.Strings(queue)

	var dangling []Dangling
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		b, err := load(id)
		if err != nil {
			return nil, dangling, fmt.Errorf("cannot read manifest %s, which a kept backup depends on, to prove the keep set is safe: %w", short(id), err)
		}
		edges := referenceEdges(b)
		// Stable edge processing order.
		sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
		for _, e := range edges {
			if !allIDs[e.ID] {
				dangling = append(dangling, Dangling{From: id, To: e.ID, Via: e.Via})
				continue
			}
			if _, ok := protected[e.ID]; !ok {
				protected[e.ID] = []string{"promoted: referenced by " + short(id) + " via " + e.Via}
				queue = append(queue, e.ID)
			}
		}
	}
	return protected, dangling, nil
}

func short(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
