// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
)

// A 32 MiB Windows-shaped disk with a REAL FAT32 EFI System Partition
// carrying the two files a boot needs, and random bytes elsewhere:
//
//	ESP  [34, 16417]  MSR [16418, 18465]  Data [18466, 43041]  Recovery [43042, 47137]
func upgradeDisk(t *testing.T, seed int64) []byte {
	t.Helper()
	img := gpttest.BuildGPT(t, 512, 65536, []gpttest.SynthPart{
		{TypeGUID: gpttest.TypeESP, Name: "EFI system partition", Sectors: 16384},
		{TypeGUID: gpttest.TypeMSR, Name: "Microsoft reserved partition", Sectors: 2048},
		{TypeGUID: gpttest.TypeMSBasicData, Name: "Basic data partition", Sectors: 24576},
		{TypeGUID: gpttest.TypeWinRE, Name: "Recovery", Sectors: 4096},
	})
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(seed))
	for _, p := range l.Partitions[1:] {
		data := make([]byte, p.Length(512))
		rng.Read(data)
		copy(img[p.Offset(512):], data)
	}
	esp := l.Partitions[0]
	copy(img[esp.Offset(512):], fat32ESP(t, esp.Length(512)))
	return img
}

// fat32ESP builds a FAT32 image with EFI\Microsoft\Boot\BCD and
// EFI\BOOT\BOOTX64.EFI, through go-diskfs (the same library the scanner
// reads FAT with — an independent writer would be better, but none is
// available in-process; the files' presence is what is under test).
func fat32ESP(t *testing.T, size int64) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "esp.img")
	d, err := diskfs.Create(path, size, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeFat32, VolumeLabel: "ESP"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"/EFI/Microsoft/Boot/BCD", "/EFI/BOOT/BOOTX64.EFI"} {
		if err := fs.Mkdir(filepath.ToSlash(filepath.Dir(f))); err != nil {
			t.Fatal(err)
		}
		w, err := fs.OpenFile(f, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(bytes.Repeat([]byte(filepath.Base(f)), 64)); err != nil {
			t.Fatal(err)
		}
		w.Close()
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func captureDisk(t *testing.T, img []byte) (repo, snapID string, cfg config.Config) {
	t.Helper()
	cfg = config.Default()
	cfg.PackFileMaxSize = 256 * 1024
	repo = initRepo(t, cfg)
	snapID, _, err := Capture(context.Background(), CaptureOptions{
		RepoPath: repo, Source: "u.img", Disk: bytes.NewReader(img), DiskSize: int64(len(img)), Hostname: "upgrade",
		NewPipeline: func() *pipeline.Pipeline {
			return pipeline.New(cfg, testLogger(), pipeline.MustBind(store.RepoConfig{}, nil))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, snapID, cfg
}

func openRepo(t *testing.T, repo string, cfg config.Config) (*index.DedupIndex, *store.ChunkStore) {
	t.Helper()
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); idx.CloseDiscard() })
	return idx, cs
}

// targetFile is a "used drive": every byte 0xEE, so a region the restore
// was supposed to zero and did not is visibly not zero (a sparse file
// would read as zero either way, and the fixture could not tell).
func targetFile(t *testing.T, size int64) (*os.File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "target.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	junk := bytes.Repeat([]byte{0xEE}, 1<<20)
	for off := int64(0); off < size; off += int64(len(junk)) {
		n := int64(len(junk))
		if size-off < n {
			n = size - off
		}
		if _, err := f.WriteAt(junk[:n], off); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { f.Close() })
	return f, path
}

func slice(img []byte, off, n int64) []byte { return img[off : off+n] }

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// A drive upgrade onto a larger disk: every partition's bytes land at the
// PLANNED offsets (authority: the source image's own slices), the grown
// data partition's new tail is zero, the moved Recovery partition is
// intact, and the target says it can boot — the ESP still carries the BCD
// and the fallback loader after being moved through a restore.
func TestRestoreDiskFitGrowsIntoTheLargerDriveAndKeepsBoot(t *testing.T) {
	img := upgradeDisk(t, 0x223)
	repo, snapID, cfg := captureDisk(t, img)
	idx, cs := openRepo(t, repo, cfg)
	l, _ := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	tg := disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}
	plan, err := disklayout.PlanFit(l, tg, disklayout.FitOptions{Grow: true, MoveRecoveryToEnd: true})
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: %v %v", err, plan.Refusals)
	}
	f, path := targetFile(t, tg.Size)
	err = RestoreDiskFit(context.Background(), RestoreDiskOptions{
		RepoPath: repo, SnapshotID: snapID, DiskIndex: 0, Target: &imageTarget{f: f}, TargetSize: tg.Size,
		Index: idx, ChunkStore: cs, Logger: testLogger(), ReadAt: f,
	}, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tl, err := disklayout.Parse(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("the upgraded disk does not parse: %v", err)
	}
	for _, p := range l.Partitions {
		pp, _ := plan.Partition(p.Index)
		want := slice(img, p.Offset(512), p.Length(512))
		if !bytes.Equal(slice(got, int64(pp.NewFirst)*512, p.Length(512)), want) {
			t.Fatalf("partition %d's bytes at its new offset %d differ from the source", p.Index, pp.NewFirst)
		}
		if pp.Grow && !allZero(slice(got, int64(pp.NewFirst)*512+p.Length(512), pp.NewBytes(512)-p.Length(512))) {
			t.Fatalf("partition %d's grown tail is not zero", p.Index)
		}
		if tl.Partitions[p.Index].FirstLBA != pp.NewFirst || tl.Partitions[p.Index].LastLBA != pp.NewLast {
			t.Fatalf("partition %d on the target = [%d, %d], plan said [%d, %d]", p.Index, tl.Partitions[p.Index].FirstLBA, tl.Partitions[p.Index].LastLBA, pp.NewFirst, pp.NewLast)
		}
	}
	rep, err := CheckBootStructures(context.Background(), path, tg.Size)
	if err != nil {
		t.Fatal(err)
	}
	if rep.ESPIndex != 0 || !rep.WindowsBoot || !rep.FallbackLoader || !rep.BackupHeaderOK {
		t.Fatalf("boot report after the upgrade: %+v", rep)
	}
	if strings.Contains(strings.Join(rep.Notes, "\n"), "nothing to boot") {
		t.Fatalf("boot report claims nothing to boot from: %v", rep.Notes)
	}
}

// The HDD→SSD case: the data partition is restored full-length into
// staging (RestoreMemberTo), "shrunk" there, and placed from the staged
// bytes; Recovery moves earlier; the target parses. A shrink with no
// staged bytes is refused by name — never restored truncated.
func TestRestoreDiskFitShrinksThroughStaging(t *testing.T) {
	img := upgradeDisk(t, 0x224)
	repo, snapID, cfg := captureDisk(t, img)
	idx, cs := openRepo(t, repo, cfg)
	l, _ := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	data := l.Partitions[2]
	tg := disklayout.TargetGeometry{Size: 40960 * 512, LogicalSector: 512}
	plan, err := disklayout.PlanFit(l, tg, disklayout.FitOptions{MinSize: func(p disklayout.Partition) (int64, bool) {
		if p.Index == 2 {
			return 4096 * 512, true
		}
		return 0, false
	}})
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: %v %v", err, plan.Refusals)
	}
	pd, _ := plan.Partition(2)
	if !pd.Shrink || pd.NewBytes(512) >= data.Length(512) {
		t.Fatalf("fixture: data was not planned to shrink: %+v", pd)
	}
	base := RestoreDiskOptions{RepoPath: repo, SnapshotID: snapID, DiskIndex: 0, Index: idx, ChunkStore: cs, Logger: testLogger()}

	// Staging: the member, full length, verified by read-back.
	stage, _ := targetFile(t, data.Length(512))
	if err := RestoreMemberTo(context.Background(), base, 2, &imageTarget{f: stage}, stage); err != nil {
		t.Fatal(err)
	}
	staged := make([]byte, data.Length(512))
	if _, err := stage.ReadAt(staged, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, slice(img, data.Offset(512), data.Length(512))) {
		t.Fatal("the staged member differs from the source partition")
	}
	// (The ISO would shrink the filesystem here; the front bytes are what
	// gets placed.)
	f, path := targetFile(t, tg.Size)
	opts := base
	opts.Target, opts.TargetSize, opts.ReadAt = &imageTarget{f: f}, tg.Size, f
	err = RestoreDiskFit(context.Background(), opts, plan, nil)
	if err == nil || !strings.Contains(err.Error(), "partition 2") || !strings.Contains(err.Error(), "no staged") {
		t.Fatalf("a shrink with nothing staged was not refused by name: %v", err)
	}
	err = RestoreDiskFit(context.Background(), opts, plan, map[int]StagedMember{2: {Reader: stage, Length: pd.NewBytes(512)}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	tl, err := disklayout.Parse(bytes.NewReader(got), int64(len(got)))
	if err != nil {
		t.Fatalf("the shrunk-onto disk does not parse: %v", err)
	}
	if !bytes.Equal(slice(got, int64(pd.NewFirst)*512, pd.NewBytes(512)), staged[:pd.NewBytes(512)]) {
		t.Fatal("the placed data partition is not the front of the staged bytes")
	}
	pr, _ := plan.Partition(3)
	if !pr.Moved || !bytes.Equal(slice(got, int64(pr.NewFirst)*512, pr.NewBytes(512)), slice(img, l.Partitions[3].Offset(512), l.Partitions[3].Length(512))) {
		t.Fatalf("Recovery (moved=%v) is not intact at its new place", pr.Moved)
	}
	if tl.Partitions[2].LastLBA != pd.NewLast || tl.Partitions[3].FirstLBA != pr.NewFirst || tl.DiskSize != tg.Size {
		t.Fatalf("target table: %+v", tl.Partitions)
	}
}

// A direct clone is byte-exact against the source (same size), relocates
// under a plan (larger), and refuses to report success when the target
// reads back differently from what was written.
func TestCloneDiskIsByteExactAndDistrustsABadTarget(t *testing.T) {
	img := upgradeDisk(t, 0x225)
	src := bytes.NewReader(img)

	f, path := targetFile(t, int64(len(img)))
	res, err := CloneDisk(context.Background(), CloneDiskOptions{Source: src, SourceSize: int64(len(img)),
		Target: &imageTarget{f: f}, TargetSize: int64(len(img)), ReadAt: f})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, img) {
		t.Fatal("a same-size clone is not byte-identical to the source")
	}
	if res.ReadBack != 4 {
		t.Fatalf("read back %d partitions of %d — a clone with ReadAt set must compare every one", res.ReadBack, res.Partitions)
	}
	// Without ReadAt nothing is compared, and the result says so.
	fBlind, _ := targetFile(t, int64(len(img)))
	blind, err := CloneDisk(context.Background(), CloneDiskOptions{Source: src, SourceSize: int64(len(img)),
		Target: &imageTarget{f: fBlind}, TargetSize: int64(len(img))})
	if err != nil || blind.ReadBack != 0 || blind.Partitions != 4 {
		t.Fatalf("a clone without ReadAt: err=%v read back %d of %d, want 0 — the count must not claim a verification that did not happen", err, blind.ReadBack, blind.Partitions)
	}
	if res.Partitions != 4 || len(res.Digests) != 4 {
		t.Fatalf("result = %+v", res)
	}

	// Larger, grown: partitions at planned offsets, tail zero.
	l, _ := disklayout.Parse(src, int64(len(img)))
	tg := disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}
	plan, _ := disklayout.PlanFit(l, tg, disklayout.FitOptions{Grow: true, MoveRecoveryToEnd: true})
	f2, path2 := targetFile(t, tg.Size)
	if _, err := CloneDisk(context.Background(), CloneDiskOptions{Source: src, SourceSize: int64(len(img)),
		Target: &imageTarget{f: f2}, TargetSize: tg.Size, Fit: plan, ReadAt: f2}); err != nil {
		t.Fatal(err)
	}
	got2, _ := os.ReadFile(path2)
	for _, p := range l.Partitions {
		pp, _ := plan.Partition(p.Index)
		if !bytes.Equal(slice(got2, int64(pp.NewFirst)*512, p.Length(512)), slice(img, p.Offset(512), p.Length(512))) {
			t.Fatalf("cloned partition %d differs at its planned offset", p.Index)
		}
	}
	if _, err := disklayout.Parse(bytes.NewReader(got2), tg.Size); err != nil {
		t.Fatalf("the grown clone does not parse: %v", err)
	}

	// A target that lies: read-back returns other bytes for partition 2.
	f3, _ := targetFile(t, int64(len(img)))
	liar := &corruptReadAt{f: f3, from: l.Partitions[2].Offset(512), to: l.Partitions[2].Offset(512) + l.Partitions[2].Length(512)}
	_, err = CloneDisk(context.Background(), CloneDiskOptions{Source: src, SourceSize: int64(len(img)),
		Target: &imageTarget{f: f3}, TargetSize: int64(len(img)), ReadAt: liar})
	if err == nil || !strings.Contains(err.Error(), "partition 2") || !strings.Contains(err.Error(), "reads back differently") {
		t.Fatalf("a target whose read-back differs was reported as a good clone: %v", err)
	}
}

type corruptReadAt struct {
	f        *os.File
	from, to int64
}

func (c *corruptReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.f.ReadAt(p, off)
	for i := 0; i < n; i++ {
		if off+int64(i) >= c.from && off+int64(i) < c.to {
			p[i] ^= 0x5a
		}
	}
	return n, err
}

// The boot report names what is missing, without refusing.
func TestCheckBootStructuresReportsMBRAndEmptyGPT(t *testing.T) {
	mbr := gpttest.BuildMBR(t, 512, 8192, []gpttest.SynthMBRPart{{Type: 0x07, Sectors: 2048, Bootable: true}})
	p := filepath.Join(t.TempDir(), "mbr.img")
	os.WriteFile(p, mbr, 0o644)
	rep, err := CheckBootStructures(context.Background(), p, int64(len(mbr)))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scheme != "mbr" || !rep.BIOSBootable || rep.ESPIndex != -1 {
		t.Fatalf("MBR report: %+v", rep)
	}
	gpt := gpttest.BuildGPT(t, 512, 8192, []gpttest.SynthPart{{TypeGUID: gpttest.TypeMSBasicData, Name: "data", Sectors: 4096}})
	p2 := filepath.Join(t.TempDir(), "gpt.img")
	os.WriteFile(p2, gpt, 0o644)
	rep, err = CheckBootStructures(context.Background(), p2, int64(len(gpt)))
	if err != nil {
		t.Fatal(err)
	}
	if rep.ESPIndex != -1 || rep.BIOSBootable || !strings.Contains(strings.Join(rep.Notes, "\n"), "nothing to boot from") || !rep.BackupHeaderOK {
		t.Fatalf("data-only GPT report: %+v", rep)
	}
	if _, err := CheckBootStructures(context.Background(), filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Fatal("a missing target produced a report")
	}
}
