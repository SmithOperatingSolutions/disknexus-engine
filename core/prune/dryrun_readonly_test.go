// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
)

// TestPruneDryRunDoesNotRecover guards issue #16: Run called recoverIfNeeded
// (which renames/RemoveAlls interrupted-prune state) unconditionally, so a
// --dry-run mutated the repo. A dry run must be read-only; with recovery state
// present it now refuses and leaves that state untouched.
func TestPruneDryRunDoesNotRecover(t *testing.T) {
	repoPath, _ := setupRepo(t)

	// Simulate an interrupted prior prune.
	staging := filepath.Join(repoPath, ".prune-staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("mkdir staging: %v", err)
	}

	_, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath, DryRun: true})
	if err == nil {
		t.Fatal("DryRun should refuse when a prior prune's recovery is pending, instead of silently recovering")
	}

	if _, statErr := os.Stat(staging); statErr != nil {
		t.Fatalf(".prune-staging was removed under DryRun — recovery mutated the repo: %v", statErr)
	}
}
