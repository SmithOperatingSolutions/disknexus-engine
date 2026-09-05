// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestCaptureRestore_MultiDisk: one Capture call over TWO disks produces ONE
// machine snapshot whose manifest carries both DiskCaptures, and each disk
// restores byte-identically by index. This is the boot+data-disk machine
// use case: a single snapshot ID covers the whole machine.
func TestCaptureRestore_MultiDisk(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img0 := buildPopulatedDisk(t, 0xD15C0)
	img1 := buildPopulatedDisk(t, 0xD15C1)

	snapID, m, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo,
		Hostname: "multi-disk-test",
		Disks: []DiskSpec{
			{Source: "disk0.img", Disk: bytes.NewReader(img0), DiskSize: int64(len(img0))},
			{Source: "disk1.img", Disk: bytes.NewReader(img1), DiskSize: int64(len(img1))},
		},
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatalf("multi-disk Capture: %v", err)
	}
	if len(m.Disks) != 2 {
		t.Fatalf("manifest has %d disks, want 2", len(m.Disks))
	}
	if m.Disks[0].Source != "disk0.img" || m.Disks[1].Source != "disk1.img" {
		t.Fatalf("disk sources wrong: %q %q", m.Disks[0].Source, m.Disks[1].Source)
	}
	for i, d := range m.Disks {
		if len(d.Members) != 4 {
			t.Fatalf("disk %d has %d members, want 4", i, len(d.Members))
		}
	}
	// Member backup IDs must be disjoint across disks (each is its own backup).
	seen := map[string]bool{}
	for _, d := range m.Disks {
		for _, mem := range d.Members {
			if mem.BackupID != "" && seen[mem.BackupID] {
				t.Fatalf("backup ID %s reused across disks", mem.BackupID)
			}
			seen[mem.BackupID] = true
		}
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

	for i, want := range [][]byte{img0, img1} {
		out := filepath.Join(t.TempDir(), "restored.img")
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(int64(len(want))); err != nil {
			t.Fatal(err)
		}
		if err := RestoreDisk(context.Background(), RestoreDiskOptions{
			RepoPath: repo, SnapshotID: snapID, DiskIndex: i,
			Target: &imageTarget{f: f}, TargetSize: int64(len(want)),
			Index: idx, ChunkStore: cs, Logger: testLogger(),
		}); err != nil {
			t.Fatalf("RestoreDisk index %d: %v", i, err)
		}
		f.Close()
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("disk %d restore differs from source", i)
		}
	}

	// Out-of-range index still refuses.
	if err := RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 2,
		Target: &imageTarget{f: nil}, TargetSize: 1,
		Index: idx, ChunkStore: cs, Logger: testLogger(),
	}); err == nil {
		t.Fatal("DiskIndex 2 accepted for a 2-disk snapshot")
	}

	// Legacy single-disk fields must still work unchanged.
	if _, m1, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "legacy.img",
		Disk: bytes.NewReader(img0), DiskSize: int64(len(img0)),
		Hostname: "legacy",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	}); err != nil || len(m1.Disks) != 1 {
		t.Fatalf("legacy single-disk capture broken: %v / %+v", err, m1)
	}
}
