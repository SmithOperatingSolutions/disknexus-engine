// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// A bare-metal restore reads chunks whose identity was recorded as the hash of
// NORMALIZED bytes — `disknexus backup --normalize` records the choice
// repo-wide, and a disk capture into that repo obeys it. RestoreDisk built its
// restorers with no normalizer at all, so restoring a Windows disk from a
// normalized repo failed the integrity check on healthy data.
//
// Same defect as the local service's restore job, one layer down: a read path
// that has to remember to ask for the normalizer (#265).

// peBlock is a minimal but PLAUSIBLE PE image; the PE normalizer validates the
// COFF header before touching anything, so random bytes would normalize to
// themselves and this test would pass with or without the fix.
func peBlock(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + 13)
	}
	const peOff = 0x80
	data[0], data[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:], peOff)
	copy(data[peOff:], []byte{'P', 'E', 0, 0})
	coff := peOff + 4
	binary.LittleEndian.PutUint16(data[coff:], 0x8664)
	binary.LittleEndian.PutUint16(data[coff+2:], 3)
	binary.LittleEndian.PutUint32(data[coff+4:], 0xDEADBEEF) // zeroed by the normalizer
	binary.LittleEndian.PutUint16(data[coff+16:], 0)
	return data
}

// TestRestoreDiskAppliesTheRepoNormalizer captures a disk full of PE-shaped
// content into a repo that records normalizers:["pe"], then restores it.
func TestRestoreDiskAppliesTheRepoNormalizer(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	rc := store.RepoConfig{
		Version:      1,
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize, ChunkMaxSize: cfg.ChunkMaxSize,
		BuzhashMask: cfg.BuzhashMask, PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
		Normalizers: []string{preprocess.NameePE},
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repo, rc); err != nil {
		t.Fatal(err)
	}

	img := gpttest.BuildGPT(t, 512, 8192, gpttest.StdWindowsParts())
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	block := peBlock(4096)
	for _, p := range l.Partitions {
		data := make([]byte, p.Length(512))
		for off := 0; off < len(data); off += len(block) {
			copy(data[off:], block)
		}
		copy(img[p.Offset(512):], data)
	}

	binding := pipeline.MustBind(rc, nil)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "pe-test.img",
		Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "pe-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), binding)
		},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	out := filepath.Join(t.TempDir(), "restored.img")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(img))); err != nil {
		t.Fatal(err)
	}
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, binding.IndexKey())
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel, binding.Key())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	if err := RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
		Normalizer: binding.Normalizer(),
	}); err != nil {
		t.Fatalf("RestoreDisk from a repo with normalizers:[pe] failed: %v", err)
	}
	f.Close()

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, img) {
		t.Error("restored disk differs from the captured one — a normalized repo must restore the ORIGINAL bytes")
	}
}
