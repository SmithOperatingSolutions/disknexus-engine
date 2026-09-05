// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package forget

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
)

// Options configures a forget run. The Load/DeleteFn/PruneFn seams default to
// the real manifest/prune functions; tests inject fakes.
type Options struct {
	RepoPath string
	Policy   Policy
	DryRun   bool
	Prune    bool // reclaim chunks (prune.Run) after deleting manifests
	Strict   bool // refuse instead of auto-promoting expired-but-referenced ancestors

	Key        *crypto.MasterKey // only needed when Prune
	OnProgress func(prune.ProgressInfo)

	// Confirm, if set, is called with the plan before any deletion (skipped
	// under DryRun). Returning false aborts without deleting.
	Confirm func(*Plan) (bool, error)

	// Seams (nil ⇒ real implementations).
	Load     CatalogLoader
	DeleteFn func(id string) error
	PruneFn  func(ctx context.Context, opts prune.Options) (*prune.Result, error)
}

// Decision is the fate of one existing backup, newest-first in a Plan.
type Decision struct {
	ID         string
	Timestamp  time.Time
	BackupType string
	Keep       bool
	Reasons    []string // policy reasons or "promoted: ..."
}

// Plan is the computed (and, unless DryRun, executed) retention plan.
type Plan struct {
	Ordered    []Decision // one per existing backup, newest-first
	Remove     []string   // IDs deleted (or that would be), newest-first
	Promoted   []string   // expired backups kept because a kept backup references them
	Dangling   []Dangling
	Deleted    []string      // IDs actually deleted (empty under DryRun)
	Pruned     *prune.Result // non-nil if prune ran
	DeleteErrs map[string]error
}

// Run computes the retention plan and, unless DryRun, deletes the expired
// backups and (with Prune) reclaims their chunks. It is read-only up to the
// point of deletion, and never deletes anything under DryRun.
func Run(ctx context.Context, opts Options) (*Plan, error) {
	if !opts.Policy.Any() {
		return nil, fmt.Errorf("forget requires at least one --keep-* rule (refusing to expire every backup)")
	}
	load := opts.Load
	if load == nil {
		load = func(id string) (*manifest.Backup, error) { return manifest.LoadCatalog(opts.RepoPath, id) }
	}

	// Source the full ID set from a raw directory scan (never silently drops a
	// corrupt-but-present backup), and cross-check against List so an
	// unreadable manifest is refused loudly rather than silently excluded from
	// the reference graph.
	rawIDs, err := manifest.ListIDs(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("listing backup IDs: %w", err)
	}
	if len(rawIDs) == 0 {
		return nil, fmt.Errorf("no backups to expire")
	}
	bs, err := manifest.List(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("listing backups: %w", err)
	}
	if len(bs) != len(rawIDs) {
		return nil, fmt.Errorf("forget: %d manifest(s) present but unreadable (parsed %d of %d); refusing — every manifest must be readable to compute a safe keep set (run 'index --rebuild-all' or repair the repo first)", len(rawIDs)-len(bs), len(bs), len(rawIDs))
	}
	allIDs := make(map[string]bool, len(rawIDs))
	for _, id := range rawIDs {
		allIDs[id] = true
	}

	keepReasons, err := selectByPolicy(bs, opts.Policy)
	if err != nil {
		return nil, err
	}

	protected, dangling, err := Protect(allIDs, keepReasons, load)
	if err != nil {
		return nil, err
	}

	// Safety: never delete everything.
	if len(protected) == 0 {
		return nil, fmt.Errorf("policy would keep 0 of %d backups; refusing to delete everything", len(allIDs))
	}

	var promoted []string
	for id, r := range protected {
		if len(r) == 1 && strings.HasPrefix(r[0], "promoted:") {
			promoted = append(promoted, id)
		}
	}
	sort.Strings(promoted)
	if opts.Strict && len(promoted) > 0 {
		return nil, fmt.Errorf("--strict: %d expired backup(s) would be kept because kept backups reference them: %s", len(promoted), strings.Join(promoted, ", "))
	}

	// Build the newest-first plan and the remove set.
	descBs := byRecency(bs)
	plan := &Plan{Dangling: dangling, Promoted: promoted, DeleteErrs: map[string]error{}}
	for _, b := range descBs {
		reasons, kept := protected[b.BackupID]
		d := Decision{ID: b.BackupID, Timestamp: time.Time(b.Timestamp), BackupType: b.BackupType, Keep: kept, Reasons: reasons}
		plan.Ordered = append(plan.Ordered, d)
		if !kept {
			plan.Remove = append(plan.Remove, b.BackupID)
		}
	}
	// Defense in depth: computed remove set must never be the whole repo.
	if len(plan.Remove) == len(allIDs) {
		return nil, fmt.Errorf("computed plan would delete all %d backups; refusing", len(allIDs))
	}

	if opts.DryRun {
		return plan, nil
	}

	if opts.Confirm != nil {
		ok, err := opts.Confirm(plan)
		if err != nil {
			return plan, err
		}
		if !ok {
			return plan, nil // user declined; nothing deleted
		}
	}

	deleteFn := opts.DeleteFn
	if deleteFn == nil {
		deleteFn = func(id string) error { return manifest.Delete(opts.RepoPath, id) }
	}
	// Delete newest-first (plan.Remove already ordered that way).
	for _, id := range plan.Remove {
		if err := deleteFn(id); err != nil {
			plan.DeleteErrs[id] = err
			continue
		}
		plan.Deleted = append(plan.Deleted, id)
	}
	if len(plan.DeleteErrs) > 0 {
		// Report but continue to prune what did get deleted; surface at the end.
		return plan, fmt.Errorf("%d manifest(s) failed to delete (see plan)", len(plan.DeleteErrs))
	}

	// Prune AFTER all deletes, so orphan collection sees only survivors. Protect
	// guarantees no survivor references a deleted backup, so prune neither
	// errors on a missing DataBackupID nor reclaims a still-needed chunk.
	if opts.Prune && len(plan.Deleted) > 0 {
		pruneFn := opts.PruneFn
		if pruneFn == nil {
			pruneFn = prune.Run
		}
		res, err := pruneFn(ctx, prune.Options{RepoPath: opts.RepoPath, DryRun: false, Key: opts.Key, OnProgress: opts.OnProgress})
		if err != nil {
			return plan, fmt.Errorf("pruning after forget: %w", err)
		}
		plan.Pruned = res
	}

	return plan, nil
}
