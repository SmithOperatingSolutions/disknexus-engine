// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package forget

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func ts(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		panic(err)
	}
	return t
}

func bk(id string, t time.Time) manifest.Backup {
	return manifest.Backup{BackupID: id, Timestamp: t}
}

// keptIDs returns the sorted set of IDs selectByPolicy kept.
func keptIDs(t *testing.T, bs []manifest.Backup, p Policy) []string {
	t.Helper()
	if p.Loc == nil {
		p.Loc = time.UTC
	}
	r, err := selectByPolicy(bs, p)
	if err != nil {
		t.Fatalf("selectByPolicy: %v", err)
	}
	var ids []string
	for id := range r {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kept = %v, want %v", got, want)
	}
}

// 1
func TestPolicy_KeepLastN_Newest(t *testing.T) {
	bs := []manifest.Backup{
		bk("a", ts("2026-01-01T00:00:00Z")),
		bk("b", ts("2026-01-02T00:00:00Z")),
		bk("c", ts("2026-01-03T00:00:00Z")),
		bk("d", ts("2026-01-04T00:00:00Z")),
		bk("e", ts("2026-01-05T00:00:00Z")),
	}
	eq(t, keptIDs(t, bs, Policy{Last: 2}), []string{"d", "e"})
}

// 2
func TestPolicy_KeepDaily_NewestOfDay(t *testing.T) {
	var bs []manifest.Backup
	for d := 1; d <= 3; d++ {
		bs = append(bs,
			bk(string(rune('a'+d))+"9", ts("2026-01-0"+itoa(d)+"T09:00:00Z")),
			bk(string(rune('a'+d))+"1", ts("2026-01-0"+itoa(d)+"T21:00:00Z")),
		)
	}
	// Daily:2 → keep the 21:00 backup of the two most-recent days (Jan 3, Jan 2).
	eq(t, keptIDs(t, bs, Policy{Daily: 2}), []string{"d1", "c1"})
}

// 3
func TestPolicy_EqualTimestamps_DeterministicByID(t *testing.T) {
	same := ts("2026-01-01T12:00:00Z")
	in1 := []manifest.Backup{bk("aaaa", same), bk("bbbb", same)}
	in2 := []manifest.Backup{bk("bbbb", same), bk("aaaa", same)} // reversed input
	for _, p := range []Policy{{Daily: 1}, {Last: 1}} {
		g1 := keptIDs(t, in1, p)
		g2 := keptIDs(t, in2, p)
		eq(t, g1, []string{"bbbb"}) // higher ID wins the tie
		if !reflect.DeepEqual(g1, g2) {
			t.Fatalf("nondeterministic across input order: %v vs %v", g1, g2)
		}
	}
}

// 4
func TestPolicy_RuleUnion_ReasonsAggregated(t *testing.T) {
	bs := []manifest.Backup{
		bk("d1", ts("2026-01-01T00:00:00Z")),
		bk("d2", ts("2026-01-02T00:00:00Z")),
		bk("d3", ts("2026-01-03T00:00:00Z")),
	}
	r, err := selectByPolicy(bs, Policy{Last: 1, Daily: 3, Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	// All three kept (daily:3); newest also kept by last → reasons union.
	if len(r) != 3 {
		t.Fatalf("kept %d, want 3", len(r))
	}
	if !reflect.DeepEqual(r["d3"], []string{"last", "daily"}) {
		t.Fatalf("d3 reasons = %v, want [last daily]", r["d3"])
	}
}

// 5
func TestPolicy_KeepWithinCalendar_Inclusive_AnchoredNow(t *testing.T) {
	now := ts("2026-03-01T00:00:00Z")
	bs := []manifest.Backup{
		bk("recent", now.Add(-12*time.Hour)),
		bk("twodays", now.Add(-48*time.Hour)),
		bk("old", now.AddDate(0, 0, -40)),
	}
	p := Policy{HasWithin: true, Within: CalDur{Days: 1}, Now: now, Loc: time.UTC}
	eq(t, keptIDs(t, bs, p), []string{"recent"})
}

// 6
func TestPolicy_KeepWithin_Months_NotMinutes(t *testing.T) {
	now := ts("2026-07-01T00:00:00Z")
	bs := []manifest.Backup{
		bk("fivemo", now.AddDate(0, -5, 0)),
		bk("sevenmo", now.AddDate(0, -7, 0)),
	}
	p := Policy{HasWithin: true, Within: CalDur{Months: 6}, Now: now, Loc: time.UTC}
	eq(t, keptIDs(t, bs, p), []string{"fivemo"})
}

// 7
func TestPolicy_ISOWeekBoundary(t *testing.T) {
	// 2020-12-31 (Thu) and 2021-01-01 (Fri) are both ISO week 2020-W53.
	bs := []manifest.Backup{
		bk("dec31", ts("2020-12-31T10:00:00Z")),
		bk("jan01", ts("2021-01-01T10:00:00Z")),
	}
	// Same ISO week → weekly:1 keeps only the newest (jan01).
	eq(t, keptIDs(t, bs, Policy{Weekly: 1}), []string{"jan01"})
}

// 8
func TestPolicy_ZeroTimestamp_RejectedOnlyWhenTimeRuleActive(t *testing.T) {
	bs := []manifest.Backup{
		bk("z", time.Time{}),
		bk("a", ts("2026-01-01T00:00:00Z")),
	}
	if _, err := selectByPolicy(bs, Policy{Daily: 1, Loc: time.UTC}); err == nil {
		t.Fatal("expected error: time rule with a zero-timestamp backup")
	}
	if _, err := selectByPolicy(bs, Policy{Last: 1, Loc: time.UTC}); err != nil {
		t.Fatalf("keep-last must tolerate zero timestamps: %v", err)
	}
}

// 9
func TestPolicy_EmptyPolicySelectsNothing(t *testing.T) {
	bs := []manifest.Backup{bk("a", ts("2026-01-01T00:00:00Z"))}
	r, err := selectByPolicy(bs, Policy{Loc: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if len(r) != 0 {
		t.Fatalf("empty policy kept %d, want 0", len(r))
	}
	if (Policy{}).Any() {
		t.Fatal("Policy{}.Any() must be false")
	}
}

// --- Protect (reference promotion) ---

// graphLoader builds a CatalogLoader from in-memory backups.
func graphLoader(bs map[string]*manifest.Backup) CatalogLoader {
	return func(id string) (*manifest.Backup, error) {
		b, ok := bs[id]
		if !ok {
			return nil, errors.New("not found: " + id)
		}
		return b, nil
	}
}

func allSet(ids ...string) map[string]bool {
	m := make(map[string]bool)
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func protectedIDs(t *testing.T, all map[string]bool, keep map[string][]string, load CatalogLoader) []string {
	t.Helper()
	p, _, err := Protect(all, keep, load)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	var ids []string
	for id := range p {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// 10
func TestProtect_PromoteParentChain(t *testing.T) {
	A := &manifest.Backup{BackupID: "A", BackupType: "full"}
	B := &manifest.Backup{BackupID: "B", BackupType: "incremental", ParentBackupID: "A"}
	C := &manifest.Backup{BackupID: "C", BackupType: "incremental", ParentBackupID: "B"}
	load := graphLoader(map[string]*manifest.Backup{"A": A, "B": B, "C": C})

	got := protectedIDs(t, allSet("A", "B", "C"), map[string][]string{"C": {"last"}}, load)
	eq(t, got, []string{"A", "B", "C"})
}

// 11
func TestProtect_DataBackupID_Transitive_TwoHop(t *testing.T) {
	A := &manifest.Backup{BackupID: "A"}
	B := &manifest.Backup{BackupID: "B", FileCatalog: []manifest.FileEntry{{Path: "x", DataBackupID: "A"}}}
	C := &manifest.Backup{BackupID: "C", FileCatalog: []manifest.FileEntry{{Path: "x", DataBackupID: "B"}}}
	load := graphLoader(map[string]*manifest.Backup{"A": A, "B": B, "C": C})

	got := protectedIDs(t, allSet("A", "B", "C"), map[string][]string{"C": {"last"}}, load)
	eq(t, got, []string{"A", "B", "C"})
}

// 12
func TestProtect_CycleTerminates(t *testing.T) {
	D1 := &manifest.Backup{BackupID: "D1", FileCatalog: []manifest.FileEntry{{DataBackupID: "D2"}}}
	D2 := &manifest.Backup{BackupID: "D2", FileCatalog: []manifest.FileEntry{{DataBackupID: "D1"}}}
	load := graphLoader(map[string]*manifest.Backup{"D1": D1, "D2": D2})

	done := make(chan []string, 1)
	go func() { done <- protectedIDs(t, allSet("D1", "D2"), map[string][]string{"D1": {"last"}}, load) }()
	select {
	case got := <-done:
		eq(t, got, []string{"D1", "D2"})
	case <-time.After(5 * time.Second):
		t.Fatal("Protect did not terminate on a reference cycle")
	}
}

// 13
func TestProtect_DanglingAbsentAncestor_WarnsNotAbort(t *testing.T) {
	B := &manifest.Backup{BackupID: "B", ParentBackupID: "A"} // A is gone
	load := graphLoader(map[string]*manifest.Backup{"B": B})

	protected, dangling, err := Protect(allSet("B"), map[string][]string{"B": {"last"}}, load)
	if err != nil {
		t.Fatalf("dangling ancestor must warn, not error: %v", err)
	}
	if _, ok := protected["A"]; ok {
		t.Fatal("absent ancestor A must not be in the protected set")
	}
	if len(dangling) != 1 || dangling[0].From != "B" || dangling[0].To != "A" || dangling[0].Via != "parent" {
		t.Fatalf("dangling = %+v, want one B->A via parent", dangling)
	}
}

// 14
func TestProtect_CorruptClosureNode_Aborts(t *testing.T) {
	C := &manifest.Backup{BackupID: "C", ParentBackupID: "B"}
	// B is present in allIDs but not loadable (corrupt) — and it IS in the closure.
	load := func(id string) (*manifest.Backup, error) {
		if id == "C" {
			return C, nil
		}
		return nil, errors.New("corrupt manifest " + id)
	}
	_, _, err := Protect(allSet("B", "C"), map[string][]string{"C": {"last"}}, load)
	if err == nil {
		t.Fatal("expected abort: a manifest in the keep closure is unreadable")
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
