// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
)

// A backup interrupted mid-stream (after a checkpoint) resumes from that
// checkpoint and the finished backup restores byte-identical to a source
// the engine only ever saw in two pieces.
func TestInterruptedBackupResumesByteIdentical(t *testing.T) {
	w := newWorld(t)
	data := noise(31, 768<<10)
	const id = "resume-e2e"

	ctx, cancel := context.WithCancel(context.Background())
	ckpts := 0
	p := w.pipeline()
	p.BackupID, p.Resumable = id, true
	p.CheckpointFn = mkCheckpointFn(w.repo, id, int64(len(data)), func(*checkpoint.Checkpoint) {
		if ckpts++; ckpts == 2 {
			cancel()
		}
	})
	_, err := p.Backup(ctx, &gatedReader{data: data, gateAt: 300 << 10, ctx: ctx}, "disk0", int64(len(data)), w.repo)
	if !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("interrupted run: want ErrSuspended, got %v", err)
	}
	c, err := checkpoint.Find(w.repo)
	if err != nil || c == nil || c.ResumeOffset <= 0 {
		t.Fatalf("no usable checkpoint after the interrupt: %+v %v", c, err)
	}
	if _, err := manifest.Load(w.repo, id); err == nil {
		t.Fatal("a suspended backup must not present a manifest — a restore would see a truncated image as complete")
	}

	deletePacksAbove(t, w.repo, c.LastSealedPack)
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{RepoPath: w.repo, RebuildBloom: true, RebuildHashIndex: true}); err != nil {
		t.Fatalf("index.Rebuild: %v", err)
	}
	p2 := w.pipeline()
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(w.repo, id, int64(len(data)), nil)
	res, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "disk0", int64(len(data)), w.repo, pipeline.ResumeState{
		BackupID: id, StartPackNum: c.LastSealedPack + 1, ResumeOffset: c.ResumeOffset, EntriesLen: c.EntriesLen,
		PrefixStats: pipeline.Stats{TotalChunks: c.TotalChunks, RawBytes: c.RawBytes, UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes},
	})
	if err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	if res.TotalChunks <= c.TotalChunks {
		t.Fatalf("resumed run reports %d chunks, no more than the %d checkpointed — nothing was appended", res.TotalChunks, c.TotalChunks)
	}
	if got := sum(w.restoreBytes(id)); got != sum(data) {
		t.Fatalf("resumed backup does not restore to the original bytes")
	}
}
