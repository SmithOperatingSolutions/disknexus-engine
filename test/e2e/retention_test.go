// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"context"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/forget"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
)

// Retention: three generations, keep the last one. forget deletes the
// other two, prune reclaims their unique chunks, and the survivor still
// restores byte-identical afterwards — the property an operator actually
// depends on. Reclaimed bytes and the pack count are interrogated so a
// prune that deleted nothing cannot pass.
func TestForgetAndPruneKeepTheSurvivorRestorable(t *testing.T) {
	w := newWorld(t)
	gens := [][]byte{noise(21, 1024<<10), noise(22, 1024<<10), noise(23, 1024<<10)}
	var ids []string
	for i, g := range gens {
		ids = append(ids, w.backupBytes(g, "disk0").BackupID)
		_ = i
	}
	w.requirePacks(6)
	before := w.packBytes()

	plan, err := forget.Run(context.Background(), forget.Options{
		RepoPath: w.repo,
		Policy:   forget.Policy{Last: 1},
	})
	if err != nil {
		t.Fatalf("forget.Run: %v", err)
	}
	if len(plan.Deleted) != 2 {
		t.Fatalf("forget deleted %d backups (%v), want 2 of 3 under keep-last-1", len(plan.Deleted), plan.Deleted)
	}
	for _, id := range ids[:2] {
		if _, err := manifest.Load(w.repo, id); err == nil {
			t.Errorf("%s still loads after forget", id)
		}
	}

	res, err := prune.Run(context.Background(), prune.Options{RepoPath: w.repo})
	if err != nil {
		t.Fatalf("prune.Run: %v", err)
	}
	if res.BytesReclaimed == 0 || w.packBytes() >= before {
		t.Fatalf("prune reclaimed %d bytes, packs %d -> %d — two unique generations were dropped, disk must shrink", res.BytesReclaimed, before, w.packBytes())
	}
	if got := sum(w.restoreBytes(ids[2])); got != sum(gens[2]) {
		t.Fatalf("the kept backup no longer restores to its bytes after prune — prune deleted chunks it still referenced")
	}
}
