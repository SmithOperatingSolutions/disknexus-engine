// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// #455 slice 2: a FULL verify reconstructs the stream and holds it against
// the manifest's digest. Per-chunk verification proves every chunk matches
// its own hash; it cannot see a stream-level defect — an entry list that
// lost, duplicated or reordered a record still has every chunk verifying
// perfectly (#376's lesson, one level up). The digest is the assertion
// about the WHOLE.

func digestVerifyWorld(t *testing.T) (string, config.Config, []byte) {
	t.Helper()
	cfg := config.Default()
	rng := rand.New(rand.NewSource(20260831))
	data := make([]byte, 256*1024)
	rng.Read(data)
	repo := initResumeRepo(t, cfg)
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = "dv"
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo); err != nil {
		t.Fatal(err)
	}
	return repo, cfg, data
}

func runVerify(t *testing.T, repo string, cfg config.Config, b *manifest.Backup, indices []int) *restore.VerifyResult {
	t.Helper()
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
	var res *restore.VerifyResult
	if indices == nil {
		res, err = restore.Verify(context.Background(), b, idx, cs)
	} else {
		res, err = restore.VerifySelectedWithNormalizer(context.Background(), b, idx, cs, nil, indices)
	}
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestAFullVerifyHoldsTheBackupAgainstItsDigest(t *testing.T) {
	repo, cfg, _ := digestVerifyWorld(t)
	b, err := manifest.Load(repo, "dv")
	if err != nil {
		t.Fatal(err)
	}

	// Healthy first (§4): the digest matches, and verify SAYS so.
	res := runVerify(t, repo, cfg, b, nil)
	if !res.OK() {
		t.Fatalf("healthy fixture failed chunk verification: %v — the digest assertions below prove nothing", res.Errors)
	}
	if res.DigestVerdict != restore.DigestMatch {
		t.Fatalf("a full verify of a digested backup reported verdict %q, want %q.\n"+
			"Per-chunk checks prove each chunk matches ITSELF; only the stream fold can say the backup as "+
			"a WHOLE is the bytes that were captured — and it ran, matched, and reported nothing.",
			res.DigestVerdict, restore.DigestMatch)
	}

	// Now the manifest claims a different stream (one flipped hex digit):
	// every chunk still verifies perfectly, and only the digest can object.
	bad := []byte(b.ContentDigest)
	if bad[0] == 'a' {
		bad[0] = 'b'
	} else {
		bad[0] = 'a'
	}
	b.ContentDigest = string(bad)
	res = runVerify(t, repo, cfg, b, nil)
	if !res.OK() {
		t.Fatalf("chunk verification failed on the digest-mismatch fixture: %v — the verdict below would "+
			"be reporting chunk damage, not the stream check", res.Errors)
	}
	if res.DigestVerdict != restore.DigestMismatch {
		t.Fatalf("the manifest's digest disagrees with the reconstruction and verify said %q.\n"+
			"This is the exact blindness that shipped #376: every per-chunk check green while the backup "+
			"as a whole is not what was captured.", res.DigestVerdict)
	}
}

func TestAPreDigestBackupIsNotVerifiableNeverPassed(t *testing.T) {
	repo, cfg, _ := digestVerifyWorld(t)
	b, err := manifest.Load(repo, "dv")
	if err != nil {
		t.Fatal(err)
	}
	b.ContentDigest = "" // the pre-#455 manifest shape
	b.ContentDigestCovers = ""
	res := runVerify(t, repo, cfg, b, nil)
	if !res.OK() {
		t.Fatalf("chunk verification failed: %v", res.Errors)
	}
	if res.DigestVerdict != restore.DigestNotVerifiable {
		t.Fatalf("a backup with no stored digest reported verdict %q, want %q.\n"+
			"Anything else lies in one of two directions: %q pins absence as success on exactly the fleet "+
			"that needs the feature, and silence hides that the strongest check never ran.",
			res.DigestVerdict, restore.DigestNotVerifiable, restore.DigestMatch)
	}
}

func TestASampledVerifyMakesNoDigestClaim(t *testing.T) {
	repo, cfg, _ := digestVerifyWorld(t)
	b, err := manifest.Load(repo, "dv")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Entries) < 2 {
		t.Fatalf("fixture produced %d entries; sampling needs at least 2", len(b.Entries))
	}
	res := runVerify(t, repo, cfg, b, []int{0}) // a sample, not the stream
	if res.DigestVerdict != "" {
		t.Fatalf("a SAMPLED verify reported digest verdict %q.\n"+
			"A fold over one chunk of the stream can neither match nor honestly mismatch the whole-stream "+
			"digest; any verdict from a sample is a claim the data does not support.", res.DigestVerdict)
	}
}
