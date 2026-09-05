// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// #465 slice 1: a disk restore holds each member's partition span against
// that member's capture digest, and names the member when it disagrees.
// RestoreVolume has failed on a mismatching target since #464; RestoreDisk
// — the restore that ends with an operator booting the machine — did not
// read anything back. Per member, because "partition 2's restored bytes do
// not match its capture" is actionable and "the disk failed" is not.

// captureDiskWorld captures a populated GPT image and hands back everything
// a RestoreDisk needs, plus the source image for authorities.
func captureDiskWorld(t *testing.T) (repo string, cfg config.Config, snapID string, img []byte) {
	t.Helper()
	cfg = config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo = initRepo(t, cfg)
	img = buildPopulatedDisk(t, 0x465)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "test.img",
		Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "digest-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return repo, cfg, snapID, img
}

func runDiskRestore(t *testing.T, repo string, cfg config.Config, snapID string, size int64) error {
	t.Helper()
	out := filepath.Join(t.TempDir(), "restored.img")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	return RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: size,
		ReadAt: f, // the read-back seam: same file the writes land in
		Index:  idx, ChunkStore: cs, Logger: testLogger(),
	})
}

func TestDiskRestoreFailsNamingTheMemberWhoseDigestDisagrees(t *testing.T) {
	repo, cfg, snapID, img := captureDiskWorld(t)

	// Positive control (§4) on the same fixture: with every digest intact,
	// the read-back passes and the restore completes.
	if err := runDiskRestore(t, repo, cfg, snapID, int64(len(img))); err != nil {
		t.Fatalf("healthy disk restore failed with read-back wired: %v", err)
	}

	// Tamper ONE member's stored digest — the manifest now describes a
	// stream the repository never captured. Every chunk of the member still
	// verifies perfectly; only the span read-back can object. (§2: with the
	// wiring absent, this fixture restores green — observably different.)
	m, err := disklayout.LoadMachineManifest(repo, snapID)
	if err != nil {
		t.Fatal(err)
	}
	victim := ""
	for _, mem := range m.Disks[0].Members {
		if mem.BackupID != "" {
			victim = mem.BackupID
			break
		}
	}
	b, err := manifest.Load(repo, victim)
	if err != nil {
		t.Fatal(err)
	}
	if b.ContentDigest == "" {
		t.Fatal("fixture member has no digest; the tamper below would test nothing")
	}
	flip := []byte(b.ContentDigest)
	if flip[0] == 'a' {
		flip[0] = 'b'
	} else {
		flip[0] = 'a'
	}
	b.ContentDigest = string(flip)
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}

	err = runDiskRestore(t, repo, cfg, snapID, int64(len(img)))
	if err == nil {
		t.Fatalf("a member manifest whose digest disagrees with its restored span restored GREEN.\n" +
			"This is the restore an operator boots a machine from: every chunk verified individually, " +
			"the whole-member check existed since #464, and the one surface that writes bootable disks " +
			"never ran it.")
	}
	if !strings.Contains(err.Error(), "partition") || !strings.Contains(err.Error(), victim[:8]) {
		t.Fatalf("the failure names neither the partition nor the member backup: %v\n"+
			"'the disk failed' sends the operator re-restoring all members; naming one makes it actionable.", err)
	}
}
