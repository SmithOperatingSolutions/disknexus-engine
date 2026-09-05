// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func initRepo(t *testing.T, cfg config.Config) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repo, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize, ChunkMaxSize: cfg.ChunkMaxSize,
		BuzhashMask: cfg.BuzhashMask, PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatal(err)
	}
	return repo
}

// buildPopulatedDisk builds a synthetic Windows-shaped GPT image and fills
// every partition with deterministic random content (gaps stay zero, so
// full-image equality is provable after restore).
func buildPopulatedDisk(t *testing.T, seed int64) []byte {
	t.Helper()
	img := gpttest.BuildGPT(t, 512, 8192, gpttest.StdWindowsParts())
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for _, p := range l.Partitions {
		data := make([]byte, p.Length(512))
		rng.Read(data)
		copy(img[p.Offset(512):], data)
	}
	return img
}

type imageTarget struct{ f *os.File }

func (i *imageTarget) WriteAt(p []byte, off int64) (int, error) { return i.f.WriteAt(p, off) }
func (i *imageTarget) Sync() error                              { return i.f.Sync() }

// TestCaptureRestore_FullDiskByteIdentical is the #69 headline: capture a
// populated GPT disk image as a machine snapshot, restore it onto a fresh
// same-size image, and the ENTIRE disk is byte-identical — GPT metadata
// (primary + backup), every partition's content, and zeroed gaps.
func TestCaptureRestore_FullDiskByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedDisk(t, 0x69)

	snapID, m, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "test.img",
		Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "bmr-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(m.Disks[0].Members) != 4 {
		t.Fatalf("want 4 members, got %+v", m.Disks[0].Members)
	}

	// Restore onto a fresh zero image of the same size.
	out := filepath.Join(t.TempDir(), "restored.img")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(img))); err != nil {
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

	err = RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("RestoreDisk: %v", err)
	}
	f.Close()

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, img) {
		// Pinpoint the first difference for diagnosis.
		for i := range got {
			if got[i] != img[i] {
				t.Fatalf("restored disk differs from source at byte %d (of %d)", i, len(img))
			}
		}
		t.Fatalf("length mismatch %d vs %d", len(got), len(img))
	}

	// The restored image must itself parse as a valid GPT identical in layout.
	l2, err := disklayout.Parse(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("restored image does not parse as GPT: %v", err)
	}
	if l2.DiskGUID != m.Disks[0].Layout.DiskGUID || len(l2.Partitions) != 4 {
		t.Fatalf("restored layout differs: %+v", l2)
	}
	if err := l2.VerifyBackupHeader(bytes.NewReader(got)); err != nil {
		t.Fatalf("restored backup GPT header invalid: %v", err)
	}
}

// TestCapture_SkippedAndListing: skipped members restore as zeros even when the
// source had data there; machine snapshots are listable.
func TestCapture_SkippedAndListing(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedDisk(t, 0x70)
	l, _ := disklayout.Parse(bytes.NewReader(img), int64(len(img)))

	plan := DefaultPlan(l)
	plan[1] = MemberPlan{Index: 1, Kind: disklayout.MemberSkipped, Reason: "MSR carries no data"}

	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "test.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Plan: plan, Hostname: "bmr-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ids, err := disklayout.ListMachineManifests(repo)
	if err != nil || len(ids) != 1 || ids[0] != snapID {
		t.Fatalf("ListMachineManifests = %v (%v)", ids, err)
	}

	out := filepath.Join(t.TempDir(), "restored.img")
	f, _ := os.Create(out)
	f.Truncate(int64(len(img)))
	idx, _ := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	defer idx.CloseDiscard()
	cs, _ := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	defer cs.Close()
	if err := RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
	}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, _ := os.ReadFile(out)

	// Skipped MSR partition (index 1) must be zeros in the restore.
	p1 := l.Partitions[1]
	zero := make([]byte, p1.Length(512))
	if !bytes.Equal(got[p1.Offset(512):p1.Offset(512)+p1.Length(512)], zero) {
		t.Fatal("skipped partition not zeroed on restore")
	}
	// Other partitions still byte-identical.
	p2 := l.Partitions[2]
	if !bytes.Equal(got[p2.Offset(512):p2.Offset(512)+p2.Length(512)], img[p2.Offset(512):p2.Offset(512)+p2.Length(512)]) {
		t.Fatal("non-skipped partition content differs")
	}
}

// TestRestoreDisk_RefusesSmallerTarget: shrinking restores stay refused (a
// smaller target would truncate partitions), as do targets that are not a
// sector-size multiple.
func TestRestoreDisk_RefusesSmallerTarget(t *testing.T) {
	cfg := config.Default()
	repo := initRepo(t, cfg)
	img := buildPopulatedDisk(t, 0x71)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "test.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "bmr-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Create(filepath.Join(t.TempDir(), "small.img"))
	defer f.Close()
	for _, wrong := range []int64{int64(len(img)) - 512, int64(len(img)) + 100} {
		err := RestoreDisk(context.Background(), RestoreDiskOptions{
			RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
			Target: &imageTarget{f: f}, TargetSize: wrong,
			Logger: testLogger(),
		})
		if err == nil {
			t.Fatalf("size %d: restore accepted an invalid target", wrong)
		}
	}
}

// TestRestoreDisk_LargerTargetRelocation is the #76 guarantee: restoring onto
// a LARGER disk relocates the backup GPT to the new end, patches AlternateLBA,
// and leaves everything else byte-identical — partitions at the same offsets
// with the same content and GUIDs, extra space unallocated.
func TestRestoreDisk_LargerTargetRelocation(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedDisk(t, 0x76)
	origLayout, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}

	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "test.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "bmr-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Target: 1 MiB larger.
	newSize := int64(len(img)) + 1<<20
	out := filepath.Join(t.TempDir(), "bigger.img")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(newSize)
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
	if err := RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: newSize,
		Index: idx, ChunkStore: cs, Logger: testLogger(),
	}); err != nil {
		t.Fatalf("RestoreDisk onto larger target: %v", err)
	}
	f.Close()
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// The restored disk parses as a valid GPT sized to the NEW disk.
	l2, err := disklayout.Parse(bytes.NewReader(got), newSize)
	if err != nil {
		t.Fatalf("restored larger disk does not parse: %v", err)
	}
	if l2.AlternateLBA != uint64(newSize/512-1) {
		t.Fatalf("AlternateLBA = %d, want new last LBA %d", l2.AlternateLBA, newSize/512-1)
	}
	if err := l2.VerifyBackupHeader(bytes.NewReader(got)); err != nil {
		t.Fatalf("relocated backup header invalid: %v", err)
	}
	// Identity preserved: disk GUID, partition GUIDs, offsets, content.
	if l2.DiskGUID != origLayout.DiskGUID || len(l2.Partitions) != len(origLayout.Partitions) {
		t.Fatalf("identity drift: %+v", l2)
	}
	for i, p := range origLayout.Partitions {
		q := l2.Partitions[i]
		if q.PartGUID != p.PartGUID || q.FirstLBA != p.FirstLBA || q.LastLBA != p.LastLBA {
			t.Fatalf("partition %d drifted: %+v vs %+v", i, q, p)
		}
		if !bytes.Equal(got[p.Offset(512):p.Offset(512)+p.Length(512)], img[p.Offset(512):p.Offset(512)+p.Length(512)]) {
			t.Fatalf("partition %d content differs", i)
		}
	}
	// The extra tail (beyond the old backup region, before the new one) is zeros.
	oldEnd := origLayout.BackupRegion.Offset
	newBackupStart := int64(l2.AlternateLBA)*512 - int64(128*128)
	zero := make([]byte, 4096)
	for off := oldEnd; off+4096 < newBackupStart; off += 1 << 19 {
		if !bytes.Equal(got[off:off+4096], zero) {
			t.Fatalf("unallocated gap not zero at %d", off)
		}
	}
}
