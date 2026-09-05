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
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
)

// #365, the resume half of the same defect.
//
// The resume preload replays the suspended session's index inserts from
// checkpoint segments. Those tuples carry the STRONG hash only, so every
// preloaded chunk reached the bloom with a WEAK hash of zero — and the bloom's
// tier-1 negative is keyed on the weak hash. The resumed run then FLUSHES that
// bloom as the repo's own, so the prefix's chunks are missing from it forever:
// every later backup is told they are new and re-stores them.
//
// The test resumes WITHOUT an index rebuild, which is not an artificial
// restriction — it is the cloud shape. Rebuilding the index from packs is
// impossible for a cloud repo (checkpoint/segment.go says so, and it is why
// segments carry inserts at all), expensive locally and refused under managed
// encryption. Preload is the only thing repairing the index there, so preload
// is where the weak hash has to survive.
//
// Assertion is on durable evidence: a later backup of UNCHANGED data must
// store nothing.
func TestResumedBackupLeavesTheRepoDedupable(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024 // small packs → several seals on a modest input

	rng := rand.New(rand.NewSource(20260823))
	data := make([]byte, 512*1024)
	rng.Read(data)

	repo := initResumeRepo(t, cfg)
	const backupID = "resume-then-dedup"

	// --- Run 1: resumable; interrupt right after the 2nd checkpoint. ---
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
		t.Fatalf("run 1: want ErrSuspended, got %v", err)
	}

	c, err := checkpoint.Find(repo)
	if err != nil || c == nil {
		t.Fatalf("no valid checkpoint after suspend: %v", err)
	}

	// --- Resume from segments alone: no index rebuild, as in the cloud. ---
	segs, err := checkpoint.ReadSegmentsLocal(repo, backupID)
	if err != nil {
		t.Fatalf("reading segments: %v", err)
	}
	replay, err := checkpoint.ReplaySegments(segs, c)
	if err != nil {
		t.Fatalf("replaying segments: %v", err)
	}
	if len(replay.Inserts) == 0 {
		t.Fatal("fixture broken: the suspended prefix replayed no index inserts")
	}
	deletePacksAbove(t, repo, c.LastSealedPack)

	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	res, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo,
		pipeline.ResumeState{
			BackupID:     backupID,
			StartPackNum: c.LastSealedPack + 1,
			ResumeOffset: c.ResumeOffset,
			EntriesLen:   c.EntriesLen,
			PrefixStats: pipeline.Stats{
				TotalChunks:  c.TotalChunks,
				RawBytes:     c.RawBytes,
				UniqueChunks: c.UniqueChunks,
				DedupChunks:  c.DedupChunks,
				StoredBytes:  c.StoredBytes,
			},
			PreloadInserts:    replay.Inserts,
			NextCheckpointSeq: c.CheckpointSeq + 1,
		})
	if err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	if err := checkpoint.Remove(repo, backupID); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(repo, backupID); err != nil {
		t.Fatalf("resumed backup produced no manifest: %v", err)
	}

	// --- The repo the resumed run left behind must still dedup its own data. ---
	p3 := pipeline.New(cfg, resumeLogger(), noEnc())
	p3.BackupID = "same-data-again"
	again, err := p3.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if again.UniqueChunks != 0 || again.StoredBytes != 0 {
		t.Fatalf("backing up UNCHANGED data after a resume re-stored it: new chunks=%d (want 0), "+
			"dedup chunks=%d/%d, stored bytes=%d (want 0); the resumed run flushed a bloom missing "+
			"the %d chunks it preloaded (weak hash zero), and the prefix was %d of the %d chunks",
			again.UniqueChunks, again.DedupChunks, again.TotalChunks, again.StoredBytes,
			len(replay.Inserts), c.TotalChunks, res.TotalChunks)
	}
}
