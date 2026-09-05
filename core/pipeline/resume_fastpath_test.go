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
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/resume"
)

// suspendMidStream runs a resumable backup that suspends after the 2nd
// checkpoint and returns its checkpoint.
func suspendMidStream(t *testing.T, repo, backupID string, cfg config.Config, data []byte) *checkpoint.Checkpoint {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), func(*checkpoint.Checkpoint) {
		n++
		if n == 2 {
			cancel()
		}
	})
	gated := &gatedReader{data: data, gateAt: 200 * 1024, ctx: ctx}
	if _, err := p.Backup(ctx, gated, "vol", int64(len(data)), repo); !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("want ErrSuspended, got %v", err)
	}
	c, err := checkpoint.Find(repo)
	if err != nil || c == nil {
		t.Fatalf("no checkpoint: %v", err)
	}
	return c
}

func resumeToCompletion(t *testing.T, repo, backupID string, cfg config.Config, data []byte, c *checkpoint.Checkpoint, rec resume.Result) {
	t.Helper()
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
		BackupID: backupID, StartPackNum: rec.StartPack, ResumeOffset: c.ResumeOffset, EntriesLen: c.EntriesLen,
		PrefixStats:    pipeline.Stats{TotalChunks: c.TotalChunks, RawBytes: c.RawBytes, UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes},
		PreloadInserts: rec.Preload, NextCheckpointSeq: rec.NextSeq,
	}
	if _, err := p.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo, rs); err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	checkpoint.Remove(repo, backupID)
}

// TestResumeFastPath_NoRebuild is the #55 guarantee: with intact segments,
// Reconcile takes the fast path (no index rebuild), and the resumed backup
// still restores byte-identically. Deleting the segments must force the rebuild
// fallback — proving the fast/slow discrimination actually works.
func TestResumeFastPath_NoRebuild(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)
	rng := rand.New(rand.NewSource(0xAA))
	data := make([]byte, 512*1024)
	rng.Read(data)

	c := suspendMidStream(t, repo, "fast", cfg, data)

	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rebuilt {
		t.Fatal("intact segments must take the fast path, not rebuild")
	}
	if len(rec.Preload) == 0 {
		t.Fatal("fast path returned no preload inserts")
	}
	resumeToCompletion(t, repo, "fast", cfg, data, c, rec)
	if got := restoreToBytes(t, repo, cfg, "fast"); !bytes.Equal(got, data) {
		t.Fatal("fast-path resumed restore != source")
	}
}

// TestResumeFastPath_FallsBackWithoutSegments: same scenario but the segments
// file is deleted → Reconcile must rebuild (Rebuilt=true) and still succeed.
func TestResumeFastPath_FallsBackWithoutSegments(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)
	rng := rand.New(rand.NewSource(0xBB))
	data := make([]byte, 512*1024)
	rng.Read(data)

	c := suspendMidStream(t, repo, "slow", cfg, data)
	if err := checkpoint.RemoveSegmentsLocal(repo, "slow"); err != nil {
		t.Fatal(err)
	}

	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Rebuilt {
		t.Fatal("missing segments must force the rebuild fallback")
	}
	resumeToCompletion(t, repo, "slow", cfg, data, c, rec)
	if got := restoreToBytes(t, repo, cfg, "slow"); !bytes.Equal(got, data) {
		t.Fatal("fallback resumed restore != source")
	}
}

// TestResumePreload_IsLoadBearing proves the preload isn't vacuous: resuming on
// the fast path but with the preload inserts DROPPED (and no rebuild) must
// yield a backup whose restore FAILS — the prefix chunks stored by the
// suspended session resolve nowhere. This is the failure the fast path's
// preload exists to prevent.
func TestResumePreload_IsLoadBearing(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)
	rng := rand.New(rand.NewSource(0xCC))
	data := make([]byte, 512*1024)
	rng.Read(data)

	c := suspendMidStream(t, repo, "noload", cfg, data)

	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rebuilt {
		t.Fatal("test needs the fast path (no rebuild)")
	}
	rec.Preload = nil // sabotage: drop the preload

	resumeToCompletion(t, repo, "noload", cfg, data, c, rec)

	// The restore must FAIL: prefix chunks are in packs but in no index.
	if !restoreFails(t, repo, cfg, "noload") {
		t.Fatal("restore succeeded without preload — the preload is vacuous or the index was rebuilt behind our back")
	}
}
