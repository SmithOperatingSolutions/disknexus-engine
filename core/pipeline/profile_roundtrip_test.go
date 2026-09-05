// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"math/rand"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestLowFootprintProfileRoundTrip: the low-footprint geometry (64 KB avg
// chunks, mask 0xFFFF, 128 MB packs) must back up and restore byte-identically
// and actually dedup repeated content — proving the mask/avg/min/max
// combination is internally consistent for the chunker, not just persisted.
func TestLowFootprintProfileRoundTrip(t *testing.T) {
	rc := store.RepoConfig{CompressionLevel: 3}
	if err := store.ApplyProfile(&rc, "low-footprint"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ChunkMinSize = rc.ChunkMinSize
	cfg.ChunkAvgSize = rc.ChunkAvgSize
	cfg.ChunkMaxSize = rc.ChunkMaxSize
	cfg.BuzhashMask = rc.BuzhashMask
	cfg.PackFileMaxSize = rc.PackFileMaxSize

	// 4 MB of data with a repeated 1 MB block: dedup must catch the repeat.
	rng := rand.New(rand.NewSource(0x10F1))
	block := make([]byte, 1<<20)
	rng.Read(block)
	data := bytes.Repeat(block, 3)
	tail := make([]byte, 1<<20)
	rng.Read(tail)
	data = append(data, tail...)

	repo := initResumeRepo(t, cfg)
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	res, err := p.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo)
	if err != nil {
		t.Fatalf("backup under low-footprint profile: %v", err)
	}
	if res.DedupChunks == 0 {
		t.Fatal("no dedup on repeated 1 MB blocks — chunk boundaries unstable under profile mask")
	}

	got := restoreToBytes(t, repo, cfg, res.BackupID)
	if !bytes.Equal(got, data) {
		t.Fatalf("restore != original (%d vs %d bytes)", len(got), len(data))
	}
}
