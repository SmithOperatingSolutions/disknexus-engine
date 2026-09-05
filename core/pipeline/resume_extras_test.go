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
	"github.com/SmithOperatingSolutions/disknexus-engine/core/resume"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestResumeIncremental_LineageAndByteIdentical: a resumable incremental backup,
// interrupted and resumed, still restores byte-identically and its manifest
// carries the parent lineage (#54).
func TestResumeIncremental_LineageAndByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)

	rng := rand.New(rand.NewSource(1))
	parentData := make([]byte, 256*1024)
	rng.Read(parentData)

	// Parent (full) backup.
	pp := pipeline.New(cfg, resumeLogger(), noEnc())
	pp.BackupID = "parent"
	if _, err := pp.Backup(context.Background(), bytes.NewReader(parentData), "vol", int64(len(parentData)), repo); err != nil {
		t.Fatal(err)
	}

	// Child data: parent's first half + fresh second half (some dedup, some new).
	childData := make([]byte, 512*1024)
	copy(childData, parentData[:128*1024])
	rng.Read(childData[128*1024:])

	backupID := "child"
	ctx, cancel := context.WithCancel(context.Background())
	ckptCount := 0
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(childData)), func(*checkpoint.Checkpoint) {
		ckptCount++
		if ckptCount == 2 {
			cancel()
		}
	})
	gated := &gatedReader{data: childData, gateAt: 200 * 1024, ctx: ctx}
	if _, err := p.Backup(ctx, gated, "vol", int64(len(childData)), repo); !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("want ErrSuspended, got %v", err)
	}
	c, _ := checkpoint.Find(repo)
	if c == nil {
		t.Fatal("no checkpoint")
	}

	// Reconcile + resume.
	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(childData)), nil)
	rs := pipeline.ResumeState{
		BackupID: backupID, StartPackNum: rec.StartPack, PreloadInserts: rec.Preload, NextCheckpointSeq: rec.NextSeq, ResumeOffset: c.ResumeOffset, EntriesLen: c.EntriesLen,
		PrefixStats: pipeline.Stats{TotalChunks: c.TotalChunks, RawBytes: c.RawBytes, UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes},
	}
	if _, err := p2.BackupResume(context.Background(), bytes.NewReader(childData[c.ResumeOffset:]), "vol", int64(len(childData)), repo, rs); err != nil {
		t.Fatal(err)
	}
	checkpoint.Remove(repo, backupID)

	// Apply incremental lineage (what the CLI does on completion).
	if _, _, err := pipeline.ApplyParentLineage(repo, backupID, "parent"); err != nil {
		t.Fatal(err)
	}

	// Manifest carries lineage.
	m, err := manifest.Load(repo, backupID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ParentBackupID != "parent" || m.BackupType != "incremental" {
		t.Fatalf("lineage not recorded: parent=%q type=%q", m.ParentBackupID, m.BackupType)
	}

	// Restore is byte-identical.
	if got := restoreToBytes(t, repo, cfg, backupID); !bytes.Equal(got, childData) {
		t.Fatal("resumed incremental restore != source")
	}
}

// TestResumeVolatileExclusion_ByteIdentical: a resumable backup whose source is
// filtered through a volatile-exclusion map restores byte-identically (excluded
// regions zeroed) across a suspend/resume, using the offset-aware
// ExcludedReaderAt on resume (#54).
func TestResumeVolatileExclusion_ByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)

	rng := rand.New(rand.NewSource(2))
	data := make([]byte, 512*1024)
	rng.Read(data)

	// Exclude two ranges (as volatile files would be).
	excl := volume.NewExclusionMap()
	excl.AddRange(90*1024, 40*1024)  // spans the ~gate region
	excl.AddRange(300*1024, 20*1024) // in the resumed suffix

	// Expected restore = data with excluded ranges zeroed.
	want := make([]byte, len(data))
	copy(want, data)
	excl.ZeroExcluded(want, 0)

	backupID := "vol-excl"
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
	excluded := volume.NewExcludedReader(gated, excl)
	if _, err := p.Backup(ctx, excluded, "vol", int64(len(data)), repo); !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("want ErrSuspended, got %v", err)
	}
	c, _ := checkpoint.Find(repo)
	if c == nil {
		t.Fatal("no checkpoint")
	}

	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
		BackupID: backupID, StartPackNum: rec.StartPack, PreloadInserts: rec.Preload, NextCheckpointSeq: rec.NextSeq, ResumeOffset: c.ResumeOffset, EntriesLen: c.EntriesLen,
		PrefixStats: pipeline.Stats{TotalChunks: c.TotalChunks, RawBytes: c.RawBytes, UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes},
	}
	// Offset-aware excluded reader on the resumed stream.
	resumedReader := volume.NewExcludedReaderAt(bytes.NewReader(data[c.ResumeOffset:]), excl, c.ResumeOffset)
	if _, err := p2.BackupResume(context.Background(), resumedReader, "vol", int64(len(data)), repo, rs); err != nil {
		t.Fatal(err)
	}
	checkpoint.Remove(repo, backupID)

	if got := restoreToBytes(t, repo, cfg, backupID); !bytes.Equal(got, want) {
		t.Fatal("resumed volatile-exclusion restore != expected (zeroed) source")
	}
}
