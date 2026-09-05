// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/resume"
)

// suspendForPrune runs a backup that suspends after the 2nd checkpoint and
// returns the repo, backup ID, and the checkpoint.
func suspendForPrune(t *testing.T, cfg config.Config, data []byte, seed int64) (string, string, *checkpoint.Checkpoint) {
	t.Helper()
	repo := initResumeRepo(t, cfg)
	backupID := "prune-resume"

	ctx, cancel := context.WithCancel(context.Background())
	ckptCount := 0
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), func(*checkpoint.Checkpoint) {
		ckptCount++
		if ckptCount == 2 {
			cancel()
		}
	})
	gated := &gatedReader{data: data, gateAt: 200 * 1024, ctx: ctx}
	if _, err := p.Backup(ctx, gated, "vol", int64(len(data)), repo); !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("want ErrSuspended, got %v", err)
	}
	c, err := checkpoint.Find(repo)
	if err != nil || c == nil {
		t.Fatalf("no checkpoint after suspend: %v", err)
	}
	return repo, backupID, c
}

func prefixHashes(t *testing.T, repo, backupID string, c *checkpoint.Checkpoint) map[[32]byte]bool {
	t.Helper()
	entries, err := manifest.ReadEntries(repo, backupID)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[[32]byte]bool{}
	for i := 0; i < int(c.EntriesCount) && i < len(entries); i++ {
		if !entries[i].IsExcluded {
			refs[entries[i].ChunkHash] = true
		}
	}
	return refs
}

// TestPruneWhileSuspended_ResumeByteIdentical is the #56 guarantee: pruning a
// repo that has a suspended backup (protecting the checkpoint prefix) must leave
// the backup resumable, and the resumed backup must restore byte-identically —
// even though prune renumbers every pack.
func TestPruneWhileSuspended_ResumeByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024

	rng := rand.New(rand.NewSource(0xBEEF))
	data := make([]byte, 512*1024)
	rng.Read(data)

	repo, backupID, c := suspendForPrune(t, cfg, data, 0xBEEF)

	// Prune while suspended, protecting the checkpoint prefix. The CLI rebuilds
	// the index first (suspended chunks were CloseDiscard'd) so they are visible.
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath: repo, RebuildBloom: true, RebuildHashIndex: true,
	}); err != nil {
		t.Fatalf("pre-prune rebuild: %v", err)
	}
	protected := prefixHashes(t, repo, backupID, c)
	if len(protected) == 0 {
		t.Fatal("no prefix hashes to protect")
	}
	if _, err := prune.Run(context.Background(), prune.Options{
		RepoPath: repo, ExtraReferencedHashes: protected,
	}); err != nil {
		t.Fatalf("prune while suspended: %v", err)
	}

	// Reconcile (pack-number-agnostic) then resume.
	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatalf("Reconcile after prune: %v", err)
	}

	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
		BackupID:     backupID,
		StartPackNum: rec.StartPack, PreloadInserts: rec.Preload, NextCheckpointSeq: rec.NextSeq,
		ResumeOffset: c.ResumeOffset,
		EntriesLen:   c.EntriesLen,
		PrefixStats: pipeline.Stats{
			TotalChunks: c.TotalChunks, RawBytes: c.RawBytes,
			UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes,
		},
	}
	if _, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo, rs); err != nil {
		t.Fatalf("BackupResume after prune: %v", err)
	}
	if err := checkpoint.Remove(repo, backupID); err != nil {
		t.Fatal(err)
	}

	got := restoreToBytes(t, repo, cfg, backupID)
	if !bytes.Equal(got, data) {
		t.Fatalf("resumed-after-prune restore != original (%d vs %d bytes)", len(got), len(data))
	}
}

// TestPruneWithoutProtection_LosesResumeData proves the protection is
// load-bearing: pruning without protecting the checkpoint prefix drops the
// suspended backup's data, and Reconcile then refuses (rather than producing an
// unrestorable backup).
func TestPruneWithoutProtection_LosesResumeData(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024

	rng := rand.New(rand.NewSource(0xF00D))
	data := make([]byte, 512*1024)
	rng.Read(data)

	repo, _, c := suspendForPrune(t, cfg, data, 0xF00D)

	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath: repo, RebuildBloom: true, RebuildHashIndex: true,
	}); err != nil {
		t.Fatalf("pre-prune rebuild: %v", err)
	}
	// Prune WITHOUT protection: the suspended backup has no manifest, so all its
	// chunks are orphans and get dropped.
	if _, err := prune.Run(context.Background(), prune.Options{RepoPath: repo}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Reconcile must now refuse: the checkpoint prefix is gone.
	if _, err := resume.Reconcile(context.Background(), repo, c, nil); err == nil {
		t.Fatal("Reconcile should fail after the prefix data was pruned away")
	}
}
