// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestBackupRefusesMissingBloom guards the round-3 revision of the
// missing-bloom guard: the check moved from NewDedupIndex (where it blocked
// restore/verify/export of intact data) to the backup write path. A backup
// against a missing-bloom/populated-index repo must refuse — the empty bloom
// reports every chunk as new and the whole source would be re-stored.
func TestBackupRefusesMissingBloom(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	data := make([]byte, 128*1024)
	rand.Read(data)
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// First backup populates the index.
	p := pipeline.New(cfg, newLogger(), noEnc())
	r1, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := p.Backup(context.Background(), r1, sourcePath, r1.Size(), repoPath); err != nil {
		r1.Close()
		t.Fatalf("Backup 1: %v", err)
	}
	r1.Close()

	// Corrupt the repo: bloom gone, index populated.
	if err := os.Remove(filepath.Join(repoPath, "index", "bloom.bin")); err != nil {
		t.Fatalf("removing bloom: %v", err)
	}

	r2, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader 2: %v", err)
	}
	defer r2.Close()
	_, err = pipeline.New(cfg, newLogger(), noEnc()).Backup(context.Background(), r2, sourcePath, r2.Size(), repoPath)
	if err == nil {
		t.Fatal("backup succeeded against a missing-bloom/populated-index repo; it would re-store the entire source as duplicates")
	}
	if !strings.Contains(err.Error(), "rebuild-all") {
		t.Fatalf("error should direct to rebuild-all, got: %v", err)
	}
}

// errAtManifestSave is unused here; the flush-before-manifest ordering is
// asserted structurally: after a SUCCESSFUL backup both the manifest and the
// flushed index exist, and after a backup whose reader failed, NEITHER exists
// (TestFailedBackupDiscardsSessionState). The dangerous middle state — manifest
// present, index inserts discarded — required flush-after-save, which
// TestFlushHappensBeforeManifestSave pins by ordering.

// TestFlushHappensBeforeManifestSave guards the round-3 finding: the manifest
// used to be saved BEFORE the index flush, so an ENOSPC at flush time (largest
// write of the finalization sequence) left a listable manifest whose chunks
// the discarding close dropped from the index — and the next prune then
// permanently deleted those chunks. With the flush first, a manifest can only
// exist if its index entries are durable. This test asserts the invariant by
// making the manifest save fail (read-only manifests dir) and verifying the
// index WAS flushed (the reverse order would flush nothing).
func TestFlushHappensBeforeManifestSave(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	data := make([]byte, 128*1024)
	rand.Read(data)
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Make ONLY the final manifest save fail: plant a non-empty DIRECTORY at
	// the exact .dnm target path (rename file→dir fails), while the .entries
	// sidecar and everything before the save work normally.
	const backupID = "0000dead-0000-0000-0000-00000000beef"
	dnmAsDir := filepath.Join(repoPath, "manifests", backupID+".dnm")
	if err := os.MkdirAll(dnmAsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dnmAsDir, "block"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	p.BackupID = backupID

	r, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()
	_, err = p.Backup(context.Background(), r, sourcePath, r.Size(), repoPath)
	if err == nil {
		t.Fatal("expected backup to fail at manifest save")
	}

	// No loadable manifest may exist (the save failed; the planted dir is not
	// a manifest)...
	if _, lerr := manifest.Load(repoPath, backupID); lerr == nil {
		t.Fatal("a manifest loads despite the failed save")
	}
	// ...but the index flush already happened (flush-before-save), so the
	// index file is non-empty. This is the harmless orphan direction; the
	// pre-fix order left the DANGEROUS direction (manifest without index).
	info, err := os.Stat(filepath.Join(repoPath, "index", "hash-index.db"))
	if err != nil || info.Size() == 0 {
		t.Fatalf("index was not flushed before the manifest save (size=%v err=%v); the pre-fix order risks a listable backup whose chunks prune deletes", info, err)
	}
}
