// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/resume"
	"github.com/SmithOperatingSolutions/disknexus-engine/filemode"
)

// gateAfter wraps an io.Reader: after limit bytes it blocks until ctx cancels,
// so a file-mode backup can be interrupted mid-stream deterministically.
type gateAfter struct {
	inner io.Reader
	left  int
	ctx   context.Context
}

func (g *gateAfter) Read(p []byte) (int, error) {
	if g.left <= 0 {
		<-g.ctx.Done()
		return 0, g.ctx.Err()
	}
	if len(p) > g.left {
		p = p[:g.left]
	}
	n, err := g.inner.Read(p)
	g.left -= n
	return n, err
}

// TestFileModeResume_ByteIdentical is the #51 guarantee: a file-mode backup
// interrupted mid-stream resumes (fast path, catalog re-walked and identical)
// and the restored stream is byte-identical to the concatenated sources.
func TestFileModeResume_ByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	repo := initResumeRepo(t, cfg)

	// Source tree: several files totalling ~512 KiB.
	srcDir := t.TempDir()
	rng := rand.New(rand.NewSource(0x51))
	for i, size := range []int{200_000, 150_000, 100_000, 74_288} {
		data := make([]byte, size)
		rng.Read(data)
		if err := os.WriteFile(filepath.Join(srcDir, "f"+string(rune('a'+i))+".bin"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := filemode.Walk(context.Background(), []string{srcDir}, filemode.NewMatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The expected restored stream = full sequential read.
	fr := filemode.NewMultiFileReader(cat)
	want, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		t.Fatal(err)
	}

	backupID := "filemode-res"
	catHash := filemode.CatalogHash(cat)

	// --- Run 1: suspend after the 2nd checkpoint. ---
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.SetFileCatalog("file", cat.SourcePaths, cat.Files)
	p.CheckpointFn = func(prog checkpoint.Progress, delta checkpoint.Delta) error {
		if err := checkpoint.AppendSegmentLocal(repo, backupID, &checkpoint.Segment{
			Seq: prog.CheckpointSeq, EntriesLenAfter: prog.EntriesLen,
			SidecarBytes: delta.SidecarBytes, Inserts: delta.Inserts,
		}); err != nil {
			return err
		}
		if err := checkpoint.Write(repo, &checkpoint.Checkpoint{
			Version: checkpoint.Version, Mode: "file", BackupID: backupID, Progress: prog,
			SourceKind: "file", SourcePath: srcDir, TotalSize: cat.TotalSize, CatalogHash: catHash,
		}); err != nil {
			return err
		}
		n++
		if n == 2 {
			cancel()
		}
		return nil
	}
	r1 := filemode.NewMultiFileReader(cat)
	gated := &gateAfter{inner: r1, left: 300 * 1024, ctx: ctx}
	_, err = p.Backup(ctx, gated, "files", cat.TotalSize, repo)
	r1.Close()
	if !errors.Is(err, pipeline.ErrSuspended) {
		t.Fatalf("want ErrSuspended, got %v", err)
	}
	c, _ := checkpoint.Find(repo)
	if c == nil || c.Mode != "file" {
		t.Fatalf("no file-mode checkpoint: %+v", c)
	}

	// --- Resume: fresh walk must match; fast path; SeekTo; complete. ---
	cat2, err := filemode.Walk(context.Background(), []string{srcDir}, filemode.NewMatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if filemode.CatalogHash(cat2) != c.CatalogHash {
		t.Fatal("identical tree must reproduce the catalog hash")
	}
	rec, err := resume.Reconcile(context.Background(), repo, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Rebuilt {
		t.Fatal("expected fast path")
	}
	r2 := filemode.NewMultiFileReader(cat2)
	defer r2.Close()
	if err := r2.SeekTo(c.ResumeOffset); err != nil {
		t.Fatal(err)
	}
	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.SetFileCatalog("file", cat2.SourcePaths, cat2.Files)
	p2.CheckpointFn = func(prog checkpoint.Progress, delta checkpoint.Delta) error { return nil }
	rs := pipeline.ResumeState{
		BackupID: backupID, StartPackNum: rec.StartPack, ResumeOffset: c.ResumeOffset, EntriesLen: c.EntriesLen,
		PrefixStats:    pipeline.Stats{TotalChunks: c.TotalChunks, RawBytes: c.RawBytes, UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes},
		PreloadInserts: rec.Preload, NextCheckpointSeq: rec.NextSeq,
	}
	if _, err := p2.BackupResume(context.Background(), r2, "files", cat.TotalSize, repo, rs); err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	checkpoint.Remove(repo, backupID)

	// The restored stream equals the concatenated sources.
	got := restoreToBytes(t, repo, cfg, backupID)
	if !bytes.Equal(got, want) {
		t.Fatalf("file-mode resumed restore != concatenated sources (%d vs %d)", len(got), len(want))
	}
}

// TestFileModeResume_CatalogChangeDetected: touching one file flips the catalog
// hash, which is the resume refusal signal (#51's identity gate).
func TestFileModeResume_CatalogChangeDetected(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "x.bin"), bytes.Repeat([]byte{1}, 10000), 0644); err != nil {
		t.Fatal(err)
	}
	cat1, err := filemode.Walk(context.Background(), []string{srcDir}, filemode.NewMatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	h1 := filemode.CatalogHash(cat1)

	// Append a byte (size + mtime change).
	f, err := os.OpenFile(filepath.Join(srcDir, "x.bin"), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{2})
	f.Close()

	cat2, err := filemode.Walk(context.Background(), []string{srcDir}, filemode.NewMatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if filemode.CatalogHash(cat2) == h1 {
		t.Fatal("changed tree produced the same catalog hash — resume would not refuse")
	}

	// And VerifyIdentity surfaces it as a refusal for file-mode checkpoints.
	c := &checkpoint.Checkpoint{Mode: "file", SourceKind: "file", SourcePath: srcDir, TotalSize: cat1.TotalSize, CatalogHash: h1}
	reason := c.VerifyIdentity(checkpoint.Identity{
		SourceKind: "file", SourcePath: srcDir, TotalSize: cat2.TotalSize, CatalogHash: filemode.CatalogHash(cat2),
	})
	if reason == "" {
		t.Fatal("VerifyIdentity accepted a changed catalog")
	}
}
