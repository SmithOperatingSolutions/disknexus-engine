// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func resumeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// gatedReader delivers data[:gateAt] normally, then blocks until ctx is
// cancelled and returns ctx.Err(). This keeps the chunker running until the
// consumer-side checkpoint deterministically cancels the context, so the
// interrupt lands mid-stream instead of racing against EOF on a small input.
type gatedReader struct {
	data   []byte
	pos    int
	gateAt int
	ctx    context.Context
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.pos >= r.gateAt {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	end := r.pos + len(p)
	if end > r.gateAt {
		end = r.gateAt
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}

func initResumeRepo(t *testing.T, cfg config.Config) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repo, store.RepoConfig{
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// deletePacksAbove removes chunks/NNNN.pack with NNNN > n (the resumed run's
// un-checkpointed tail), matching the CLI's resume reconciliation.
func deletePacksAbove(t *testing.T, repo string, n uint32) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(repo, "chunks", "*.pack"))
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".pack")
		num, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		if uint32(num) > n {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func restoreToBytes(t *testing.T, repo string, cfg config.Config, backupID string) []byte {
	t.Helper()
	backup, err := manifest.Load(repo, backupID)
	if err != nil {
		t.Fatalf("manifest.Load(%s): %v", backupID, err)
	}
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	out := filepath.Join(t.TempDir(), "restored-"+backupID+".img")
	w, err := volume.NewWriter(out)
	if err != nil {
		t.Fatal(err)
	}
	_, err = restore.NewRestorer(idx, cs, resumeLogger()).Restore(context.Background(), backup, w)
	w.Close()
	if err != nil {
		t.Fatalf("Restore(%s): %v", backupID, err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// restoreFails reports whether restoring the backup fails (used to prove a
// sabotaged resume path is load-bearing rather than vacuously passing).
func restoreFails(t *testing.T, repo string, cfg config.Config, backupID string) bool {
	t.Helper()
	backup, err := manifest.Load(repo, backupID)
	if err != nil {
		return true
	}
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		return true
	}
	defer idx.CloseDiscard()
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		return true
	}
	defer cs.Close()
	out := filepath.Join(t.TempDir(), "restored-fail-probe.img")
	w, err := volume.NewWriter(out)
	if err != nil {
		return true
	}
	_, rerr := restore.NewRestorer(idx, cs, resumeLogger()).Restore(context.Background(), backup, w)
	w.Close()
	return rerr != nil
}

func mkCheckpointFn(repo, backupID string, total int64, onWrite func(*checkpoint.Checkpoint)) func(checkpoint.Progress, checkpoint.Delta) error {
	return func(prog checkpoint.Progress, delta checkpoint.Delta) error {
		// Mirror the CLI: segment first, then the checkpoint record.
		if err := checkpoint.AppendSegmentLocal(repo, backupID, &checkpoint.Segment{
			Seq:             prog.CheckpointSeq,
			EntriesLenAfter: prog.EntriesLen,
			SidecarBytes:    delta.SidecarBytes,
			Inserts:         delta.Inserts,
		}); err != nil {
			return err
		}
		c := &checkpoint.Checkpoint{
			Version:         checkpoint.Version,
			Mode:            "volume",
			BackupID:        backupID,
			Progress:        prog,
			SourceKind:      "input",
			SourcePath:      "mem",
			TotalSize:       total,
			PacksGeneration: store.PacksGeneration(repo),
		}
		if err := checkpoint.Write(repo, c); err != nil {
			return err
		}
		if onWrite != nil {
			onWrite(c)
		}
		return nil
	}
}

// TestSuspendResumeRestore_ByteIdentical is the headline #42 guarantee: a backup
// interrupted mid-stream (deterministically, after the 2nd pack-seal checkpoint)
// is SUSPENDED, then resumed via the CLI-equivalent orchestration (delete packs
// > N, rebuild index, BackupResume from the checkpoint offset), and its restore
// is byte-for-byte identical BOTH to the original source AND to an
// uninterrupted backup's restore.
func TestSuspendResumeRestore_ByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024 // small packs → several seals on a modest input

	rng := rand.New(rand.NewSource(20260723))
	data := make([]byte, 512*1024) // ~10+ packs worth
	rng.Read(data)

	repo := initResumeRepo(t, cfg)
	backupID := "resume-me"

	// --- Run 1: resumable; crash (cancel ctx) right after the 2nd checkpoint. ---
	ctx, cancel := context.WithCancel(context.Background())
	ckptCount := 0
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), func(*checkpoint.Checkpoint) {
		ckptCount++
		if ckptCount == 2 {
			cancel() // deterministic interrupt at a known pack/offset boundary
		}
	})
	// Deliver ~200 KB (enough for ≥2 pack seals at 48 KB), then block until the
	// 2nd checkpoint cancels ctx, so the interrupt lands mid-stream.
	gated := &gatedReader{data: data, gateAt: 200 * 1024, ctx: ctx}
	_, err := p.Backup(ctx, gated, "vol", int64(len(data)), repo)
	if !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("run 1: want ErrSuspended, got %v", err)
	}

	// Suspended artifacts: checkpoint + sidecar + packs present; NO manifest.
	c, err := checkpoint.Find(repo)
	if err != nil || c == nil {
		t.Fatalf("no valid checkpoint after suspend: %v", err)
	}
	if _, err := os.Stat(manifest.EntriesPath(repo, backupID)); err != nil {
		t.Fatalf("entries sidecar was removed on suspend: %v", err)
	}
	if _, err := manifest.Load(repo, backupID); err == nil {
		t.Fatal("a suspended backup must not have a manifest yet")
	}
	if c.ResumeOffset <= 0 || c.EntriesLen <= 0 {
		t.Fatalf("checkpoint offset/len not set: %+v", c.Progress)
	}

	// --- CLI-equivalent resume orchestration. ---
	deletePacksAbove(t, repo, c.LastSealedPack)
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath: repo, RebuildBloom: true, RebuildHashIndex: true,
	}); err != nil {
		t.Fatalf("index.Rebuild: %v", err)
	}

	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
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
	}
	res, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo, rs)
	if err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	if res.TotalChunks <= c.TotalChunks {
		t.Fatalf("resumed total chunks %d should exceed prefix %d", res.TotalChunks, c.TotalChunks)
	}
	if err := checkpoint.Remove(repo, backupID); err != nil {
		t.Fatal(err)
	}

	// --- Restore the resumed backup; must equal the original source. ---
	got := restoreToBytes(t, repo, cfg, backupID)
	if !bytes.Equal(got, data) {
		t.Fatalf("resumed restore != original source (%d vs %d bytes)", len(got), len(data))
	}

	// --- And equal an uninterrupted backup's restore. ---
	repo2 := initResumeRepo(t, cfg)
	pun := pipeline.New(cfg, resumeLogger(), noEnc())
	pun.BackupID = "uninterrupted"
	if _, err := pun.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo2); err != nil {
		t.Fatalf("uninterrupted backup: %v", err)
	}
	want := restoreToBytes(t, repo2, cfg, "uninterrupted")
	if !bytes.Equal(got, want) {
		t.Fatal("resumed restore differs from uninterrupted restore")
	}
}

// TestCheckpointLagsSeal_Ordering asserts a checkpoint fires only on a real pack
// seal, names pack N=packNum-1, and records an EntriesLen that EXCLUDES the
// triggering (resume-point) chunk — i.e. EntriesLen/45 == the checkpoint's
// prefix TotalChunks.
func TestCheckpointLagsSeal_Ordering(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	rng := rand.New(rand.NewSource(7))
	data := make([]byte, 256*1024)
	rng.Read(data)

	repo := initResumeRepo(t, cfg)
	var progs []checkpoint.Progress
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = "ordering"
	p.Resumable = true
	p.CheckpointFn = func(prog checkpoint.Progress, _ checkpoint.Delta) error {
		progs = append(progs, prog)
		return nil
	}
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo); err != nil {
		t.Fatal(err)
	}
	if len(progs) < 2 {
		t.Fatalf("expected several seals, got %d checkpoints", len(progs))
	}
	var lastPack int64 = -1
	for i, prog := range progs {
		if int64(prog.LastSealedPack) <= lastPack {
			t.Fatalf("checkpoint %d pack %d not strictly increasing (prev %d)", i, prog.LastSealedPack, lastPack)
		}
		lastPack = int64(prog.LastSealedPack)
		if prog.ResumeOffset <= 0 {
			t.Fatalf("checkpoint %d has non-positive resume offset", i)
		}
		// EntriesLen excludes the triggering chunk: count == prefix TotalChunks.
		if prog.EntriesLen/manifest.EntryRecordSize != prog.EntriesCount {
			t.Fatalf("checkpoint %d EntriesCount %d != len/45 %d", i, prog.EntriesCount, prog.EntriesLen/manifest.EntryRecordSize)
		}
		if prog.EntriesCount != prog.TotalChunks {
			t.Fatalf("checkpoint %d entries %d != prefix total chunks %d (should exclude the resume-point chunk)", i, prog.EntriesCount, prog.TotalChunks)
		}
		// Resume offset is exactly the end of the boundary chunk (contiguity).
		if prog.BoundaryChunkLength > 0 && prog.BoundaryChunkOffset+int64(prog.BoundaryChunkLength) != prog.ResumeOffset {
			t.Fatalf("checkpoint %d boundary end %d != resume offset %d", i, prog.BoundaryChunkOffset+int64(prog.BoundaryChunkLength), prog.ResumeOffset)
		}
	}
}
