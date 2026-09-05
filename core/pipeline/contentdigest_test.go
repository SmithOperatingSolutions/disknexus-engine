// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
)

// #455: the backup carries a content digest over its captured source stream.
//
// The strongest thing the repository could previously assert was that every
// chunk RESOLVES — and #376 proved chunks can resolve perfectly on a backup
// that is unrestorable. The digest is the one number that answers the
// operator's actual question: are the bytes I would get back the bytes I
// put in. It is folded where the stream is still sequential (the chunker),
// covers the captured logical stream — original bytes, post
// exclusion-zeroing, pre compression/encryption, offset order — and lands
// in the manifest, where a verify or a restore can hold the reconstruction
// against it.

func TestABackupRecordsTheDigestOfItsSourceStream(t *testing.T) {
	cfg := config.Default()
	rng := rand.New(rand.NewSource(20260828))
	data := make([]byte, 256*1024)
	rng.Read(data)
	repo := initResumeRepo(t, cfg)

	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = "digested"
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(repo, "digested")
	if err != nil {
		t.Fatal(err)
	}

	// The authority (§3) is an INDEPENDENT hash of the source, not anything
	// the pipeline computed: sha256sum of the image a full restore writes.
	want := sha256.Sum256(data)
	if m.ContentDigest == "" {
		t.Fatalf("the manifest carries no content digest.\n" +
			"Every chunk of this backup resolves, and #376 shipped completed, unrestorable backups on " +
			"exactly that assurance — without the digest, nothing in the product can ever say 'the bytes " +
			"you would get back are the bytes you put in'.")
	}
	if m.ContentDigest != hex.EncodeToString(want[:]) {
		t.Fatalf("manifest digest %s != sha256 of the source %x — a digest that does not equal an "+
			"independent hash of the source verifies the pipeline's opinion of itself, not the data",
			m.ContentDigest, want)
	}
	if m.ContentDigestCovers != manifest.DigestCoversSourceStreamV1 {
		t.Fatalf("digest covers %q — without a recorded definition, a future change to what is folded "+
			"silently invalidates every stored digest while matching none of them", m.ContentDigestCovers)
	}
}

// The digest must survive suspend/resume, because a resumed run CANNOT
// re-read the bytes before its resume offset — the state has to travel in
// the checkpoint. And it must equal the unbroken run's digest, or a digest
// mismatch on a resumed backup reads as corruption when it is bookkeeping.
func TestAResumedBackupCarriesTheSameDigestAsAnUnbrokenOne(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024

	rng := rand.New(rand.NewSource(20260829))
	data := make([]byte, 512*1024)
	rng.Read(data)
	repo := initResumeRepo(t, cfg)
	backupID := "digest-resume"

	ctx, cancel := context.WithCancel(context.Background())
	ckptCount := 0
	var lastCkpt *checkpoint.Checkpoint
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = backupID
	p.Resumable = true
	p.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), func(c *checkpoint.Checkpoint) {
		ckptCount++
		lastCkpt = c
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
		t.Fatalf("no checkpoint: %v", err)
	}
	if len(c.DigestState) == 0 {
		t.Fatalf("the checkpoint carries no digest state.\n"+
			"A resumed run starts at offset %d and never re-reads the prefix, so without the state the "+
			"resumed backup either records no digest or records a wrong one — and a WRONG stored digest "+
			"is worse than none: it makes an honest verify condemn a healthy backup.", c.ResumeOffset)
	}
	_ = lastCkpt

	deletePacksAbove(t, repo, c.LastSealedPack)
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath: repo, RebuildBloom: true, RebuildHashIndex: true,
	}); err != nil {
		t.Fatal(err)
	}
	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
		BackupID:     backupID,
		StartPackNum: c.LastSealedPack + 1,
		ResumeOffset: c.ResumeOffset,
		EntriesLen:   c.EntriesLen,
		DigestState:  c.DigestState,
		PrefixStats: pipeline.Stats{
			TotalChunks:  c.TotalChunks,
			RawBytes:     c.RawBytes,
			UniqueChunks: c.UniqueChunks,
			DedupChunks:  c.DedupChunks,
			StoredBytes:  c.StoredBytes,
		},
	}
	if _, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo, rs); err != nil {
		t.Fatalf("BackupResume: %v", err)
	}
	if err := checkpoint.Remove(repo, backupID); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Load(repo, backupID)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if m.ContentDigest != hex.EncodeToString(want[:]) {
		t.Fatalf("resumed backup's digest %s != sha256 of the full source %x.\n"+
			"The restore of this backup is byte-identical to the source (the #42 guarantee, proven in "+
			"TestSuspendResumeRestore_ByteIdentical) — so a differing digest here means the DIGEST "+
			"bookkeeping broke across the suspend, and every resumed backup on the fleet would fail an "+
			"honest verify forever after.", m.ContentDigest, want)
	}
}

// A resume whose checkpoint carries NO digest state (suspended by a
// pre-digest build) must record NO digest — not a fold of the suffix
// wearing a whole-stream name. A wrong stored digest is the one state worse
// than none: an honest verify then condemns a healthy backup forever.
func TestAResumeWithoutDigestStateRecordsNoDigestRatherThanAWrongOne(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 48 * 1024
	rng := rand.New(rand.NewSource(20260830))
	data := make([]byte, 512*1024)
	rng.Read(data)
	repo := initResumeRepo(t, cfg)
	backupID := "pre-digest-resume"

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
		t.Fatal(err)
	}
	deletePacksAbove(t, repo, c.LastSealedPack)
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath: repo, RebuildBloom: true, RebuildHashIndex: true,
	}); err != nil {
		t.Fatal(err)
	}
	p2 := pipeline.New(cfg, resumeLogger(), noEnc())
	p2.Resumable = true
	p2.CheckpointFn = mkCheckpointFn(repo, backupID, int64(len(data)), nil)
	rs := pipeline.ResumeState{
		BackupID:     backupID,
		StartPackNum: c.LastSealedPack + 1,
		ResumeOffset: c.ResumeOffset,
		EntriesLen:   c.EntriesLen,
		DigestState:  nil, // the pre-#455 checkpoint shape
		PrefixStats: pipeline.Stats{
			TotalChunks: c.TotalChunks, RawBytes: c.RawBytes,
			UniqueChunks: c.UniqueChunks, DedupChunks: c.DedupChunks, StoredBytes: c.StoredBytes,
		},
	}
	if _, err := p2.BackupResume(context.Background(), bytes.NewReader(data[c.ResumeOffset:]), "vol", int64(len(data)), repo, rs); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Remove(repo, backupID); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(repo, backupID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ContentDigest != "" {
		want := sha256.Sum256(data)
		verdict := "a SUFFIX fold stamped as the whole stream"
		if m.ContentDigest == hex.EncodeToString(want[:]) {
			verdict = "somehow correct, which this fixture cannot explain"
		}
		t.Fatalf("a resume with no digest state recorded digest %s (%s).\n"+
			"Every backup suspended by a pre-digest build and resumed by a new one would carry a value "+
			"an honest verify must fail, turning the upgrade itself into a fleet of red verifies.",
			m.ContentDigest, verdict)
	}
}

// File mode rides the same fold (#465 slice 4): the pipeline digests the
// concatenated file stream exactly as it digests a volume, so the sweep and
// the full verify already cover file-mode backups. Pinned, because "mode-
// independent by construction" is a claim about today's code — a file-mode
// special case added later must fail something.
func TestAFileModeBackupCarriesTheStreamDigestToo(t *testing.T) {
	cfg := config.Default()
	rng := rand.New(rand.NewSource(20260901))
	data := make([]byte, 128*1024)
	rng.Read(data)
	repo := initResumeRepo(t, cfg)
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = "filemode-digest"
	p.SetFileCatalog("file", []string{"/src"}, []manifest.FileEntry{
		{Path: "a.bin", Size: int64(len(data)), Mode: 0644, StreamOffset: 0, StreamLength: int64(len(data))},
	})
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "files", int64(len(data)), repo); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(repo, "filemode-digest")
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if m.ContentDigest != hex.EncodeToString(want[:]) {
		t.Fatalf("file-mode digest %q != sha256 of the concatenated stream %x — file-mode backups fall "+
			"out of everything slices 1-3 built: no verdicts, no sweep coverage, not-verifiable forever",
			m.ContentDigest, want)
	}
}
