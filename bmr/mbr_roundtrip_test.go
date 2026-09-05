// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func buildPopulatedMBRDisk(t *testing.T, seed int64) []byte {
	t.Helper()
	img := gpttest.BuildMBR(t, 512, 32768, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 8192, Bootable: true},
		{Type: 0x0C, Sectors: 4096},
		{Type: 0x07, Sectors: 4096, Logical: true},
		{Type: 0x83, Sectors: 4096, Logical: true},
	})
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

// #149 slice 3: an MBR disk (primaries + EBR-chained logicals) captures and
// restores BYTE-IDENTICALLY — boot code, EBR chain, and partition data all
// verbatim, which is exactly what makes the restored disk boot on BIOS
// firmware with no bootloader surgery. Larger-target restore keeps the
// front of the disk byte-identical (nothing relocates on MBR).
func TestCaptureRestore_MBRByteIdentical(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedMBRDisk(t, 0x37)

	snapID, m, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "mbr-test.img",
		Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "mbr-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if m.Disks[0].Layout.Scheme != "mbr" {
		t.Fatalf("manifest scheme = %q", m.Disks[0].Layout.Scheme)
	}
	if len(m.Disks[0].Members) != 4 {
		t.Fatalf("want 4 members (2 primaries + 2 logicals), got %+v", m.Disks[0].Members)
	}

	restore := func(t *testing.T, targetSize int64) []byte {
		t.Helper()
		out := filepath.Join(t.TempDir(), "restored.img")
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(targetSize); err != nil {
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
		if err := RestoreDisk(context.Background(), RestoreDiskOptions{
			RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
			Target: &imageTarget{f: f}, TargetSize: targetSize,
			Index: idx, ChunkStore: cs, Logger: testLogger(),
		}); err != nil {
			t.Fatalf("RestoreDisk: %v", err)
		}
		f.Close()
		got, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Same-size: byte-identical.
	got := restore(t, int64(len(img)))
	if !bytes.Equal(got, img) {
		for i := range got {
			if got[i] != img[i] {
				t.Fatalf("restored differs at byte %d (of %d)", i, len(img))
			}
		}
	}
	// The restored image re-parses as the same MBR layout.
	l2, err := disklayout.Parse(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("restored image does not parse: %v", err)
	}
	if l2.Scheme != "mbr" || len(l2.Partitions) != 4 {
		t.Fatalf("restored layout: %+v", l2)
	}

	// Larger target: front of disk byte-identical; tail zeros.
	got2 := restore(t, int64(len(img))+8<<20)
	if !bytes.Equal(got2[:len(img)], img) {
		t.Fatal("larger-target restore altered the disk front")
	}
}

// #151: with CaptureFiles, NTFS members of a machine snapshot get file
// catalogs on their member backups — and a file EXTRACTS from the member
// backup byte-for-byte, proving the partition-relative extents line up
// with the member stream. Both schemes.
func TestDiskCaptureMemberCatalogs(t *testing.T) {
	ntfsImg, err := os.ReadFile("../volumefs/testdata/ntfs.img")
	if err != nil {
		t.Fatal(err)
	}

	run := func(t *testing.T, img []byte, ntfsPartIdx int) {
		cfg := config.Default()
		cfg.PackFileMaxSize = 256 * 1024
		repo := initRepo(t, cfg)

		l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		p := l.Partitions[ntfsPartIdx]
		if int64(len(ntfsImg)) > p.Length(512) {
			t.Fatalf("fixture larger than partition")
		}
		copy(img[p.Offset(512):], ntfsImg)

		_, m, err := Capture(context.Background(), CaptureOptions{
			RepoPath: repo, Source: "cat.img",
			Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
			Hostname: "cat-test", CaptureFiles: true,
			NewPipeline: func() *pipeline.Pipeline {
				return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
			},
		})
		if err != nil {
			t.Fatalf("Capture: %v", err)
		}

		// The NTFS member's backup manifest carries the catalog.
		var memberID string
		for _, mem := range m.Disks[0].Members {
			if mem.Index == ntfsPartIdx {
				memberID = mem.BackupID
			}
		}
		if memberID == "" {
			t.Fatal("no member backup for the NTFS partition")
		}
		backup, err := manifest.Load(repo, memberID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range backup.FileCatalog {
			if f.Path == "./hello.txt" {
				found = true
			}
		}
		if !found {
			t.Fatalf("member catalog missing ./hello.txt (%d entries)", len(backup.FileCatalog))
		}

		// End-to-end: extract a file FROM THE MEMBER BACKUP.
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
		fr := restore.NewFileRestorer(idx, cs, repo, testLogger())
		target := filepath.Join(t.TempDir(), "hello.txt")
		if _, err := fr.ExtractFile(context.Background(), backup, "hello.txt", target); err != nil {
			t.Fatalf("ExtractFile from member backup: %v", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || len(got) == 0 {
			t.Fatalf("extracted file empty (%v)", err)
		}
	}

	t.Run("gpt", func(t *testing.T) {
		img := gpttest.BuildGPT(t, 512, 40960, []gpttest.SynthPart{
			{TypeGUID: disklayout.TypeMSBasicData, Name: "Basic data partition", Sectors: 20480},
		})
		run(t, img, 0)
	})
	t.Run("mbr", func(t *testing.T) {
		img := gpttest.BuildMBR(t, 512, 40960, []gpttest.SynthMBRPart{
			{Type: 0x07, Sectors: 20480, Bootable: true},
			{Type: 0x07, Sectors: 4096, Logical: true},
		})
		run(t, img, 0)
	})
}

// #153 slice B: a 580k-file catalog (147 MB in the field) was decoded into
// RAM by DISK restores that never use it. Block restores must skip the
// catalog section entirely — proven by corrupting it and restoring anyway.
func TestDiskRestoreIgnoresCatalogSection(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	// Layout with a partition large enough for the 8 MB NTFS fixture.
	img := gpttest.BuildMBR(t, 512, 40960, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 20480, Bootable: true},
		{Type: 0x07, Sectors: 4096, Logical: true},
	})
	ntfsImg, err := os.ReadFile("../volumefs/testdata/ntfs.img")
	if err != nil {
		t.Fatal(err)
	}
	l, _ := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	copy(img[l.Partitions[0].Offset(512):], ntfsImg)

	snapID, m, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "cat.img", CaptureFiles: true,
		Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "b-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the CATALOG section of the NTFS member's dnm in place.
	memberID := m.Disks[0].Members[0].BackupID
	dnmPath := manifest.DNMPath(repo, memberID)
	if err := manifest.CorruptCatalogSectionForTest(dnmPath); err != nil {
		t.Fatalf("corrupting catalog: %v", err)
	}
	// Positive control: the full loader must now fail on this manifest. If it
	// does not, the corruption hook has drifted and this test can no longer
	// discriminate a restore that skips the catalog from one that decodes it —
	// that is fixture drift, and fixture drift is a FAILURE, not an exemption
	// (#378: a skip here does not weaken the test, it deletes it).
	if _, err := manifest.Load(repo, memberID); err == nil {
		t.Fatal("fixture drift: CorruptCatalogSectionForTest no longer breaks a full manifest load, " +
			"so this test cannot prove disk restores skip the catalog section — fix the corruption hook " +
			"(it must make the CATALOG section unreadable) instead of skipping")
	}

	// …but the DISK restore must not care.
	out := filepath.Join(t.TempDir(), "restored.img")
	f, _ := os.Create(out)
	f.Truncate(int64(len(img)))
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
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
	}); err != nil {
		t.Fatalf("disk restore must not decode the catalog: %v", err)
	}
	f.Close()
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, img) {
		t.Fatal("restore with corrupted catalog not byte-identical")
	}
}

// #153 slice D: restores report progress — monotonic bytes against a total,
// reaching completion — so the recovery flow can print percent/rate/ETA
// instead of tens of silent minutes.
func TestRestoreDiskReportsProgress(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedMBRDisk(t, 0x77)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "p.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "p-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "r.img")
	f, _ := os.Create(out)
	defer f.Close() // Windows: TempDir cleanup cannot delete an open file
	f.Truncate(int64(len(img)))
	idx, _ := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	defer idx.CloseDiscard()
	cs, _ := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	defer cs.Close()

	var calls int
	var lastDone int64
	var lastMember, totalMembers int
	err = RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
		OnProgress: func(member, members int, done, total int64) {
			calls++
			if done < lastDone && member == lastMember {
				t.Errorf("progress regressed within member %d: %d < %d", member, done, lastDone)
			}
			if total <= 0 {
				t.Errorf("nonpositive total: %d", total)
			}
			lastDone, lastMember, totalMembers = done, member, members
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("no progress reported")
	}
	if totalMembers != 4 {
		t.Fatalf("members = %d, want 4", totalMembers)
	}
}

// #157: drivers plan fetch policy per member — bmr surfaces each member's
// entries before restoring it.
func TestRestoreDiskSurfacesMemberEntries(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo := initRepo(t, cfg)
	img := buildPopulatedMBRDisk(t, 0x99)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "e.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)),
		Hostname: "e-test",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "r.img")
	f, _ := os.Create(out)
	defer f.Close()
	f.Truncate(int64(len(img)))
	idx, _ := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	defer idx.CloseDiscard()
	cs, _ := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	defer cs.Close()

	var calls int
	err = RestoreDisk(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0,
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)),
		Index: idx, ChunkStore: cs, Logger: testLogger(),
		OnMemberEntries: func(entries manifest.EntryAccessor) {
			calls++
			if entries.Count() == 0 {
				t.Error("member surfaced empty entries")
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("OnMemberEntries calls = %d, want 4 (one per member)", calls)
	}
}
