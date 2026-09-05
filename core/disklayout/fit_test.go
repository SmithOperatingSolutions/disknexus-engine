// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
)

// A 32 MiB synthetic Windows disk (512-byte sectors): ESP 1 MiB, MSR 1 MiB,
// a 40000-sector data partition, a 4096-sector Recovery partition after it.
// BuildGPT packs partitions contiguously from LBA 34, so:
//
//	ESP  [34, 2081]   MSR  [2082, 4129]   Data [4130, 44129]   WinRE [44130, 48225]
//
// and the last usable LBA is 65502 (32 entry sectors + backup header).
func upgradeFixture(t *testing.T) ([]byte, *disklayout.DiskLayout) {
	t.Helper()
	img := gpttest.BuildGPT(t, 512, 65536, []gpttest.SynthPart{
		{TypeGUID: gpttest.TypeESP, Name: "EFI system partition", Sectors: 2048},
		{TypeGUID: gpttest.TypeMSR, Name: "Microsoft reserved partition", Sectors: 2048},
		{TypeGUID: gpttest.TypeMSBasicData, Name: "Basic data partition", Sectors: 40000},
		{TypeGUID: gpttest.TypeWinRE, Name: "Recovery", Sectors: 4096},
	})
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Partitions) != 4 || l.Partitions[2].FirstLBA != 4130 || l.Partitions[3].LastLBA != 48225 || l.LastUsableLBA != 65502 {
		t.Fatalf("fixture geometry drifted: %+v lastUsable=%d", l.Partitions, l.LastUsableLBA)
	}
	return img, l
}

func regions(img []byte, l *disklayout.DiskLayout) (primary, backup []byte) {
	return img[l.PrimaryRegion.Offset : l.PrimaryRegion.Offset+l.PrimaryRegion.Length],
		img[l.BackupRegion.Offset : l.BackupRegion.Offset+l.BackupRegion.Length]
}

// applyAndParse writes the plan's structures onto a blank target and parses
// it back — Parse validates both header CRCs and the entry-array CRC, so a
// stale checksum fails here, the way firmware would refuse the disk.
func applyAndParse(t *testing.T, l *disklayout.DiskLayout, plan *disklayout.FitPlan, primary, backup []byte) *disklayout.DiskLayout {
	t.Helper()
	newPrimary, backupOff, newBackup, err := disklayout.ApplyFit(l, plan, primary, backup)
	if err != nil {
		t.Fatalf("ApplyFit: %v", err)
	}
	disk := make([]byte, plan.TargetSize)
	copy(disk, newPrimary)
	if newBackup != nil {
		if backupOff+int64(len(newBackup)) != plan.TargetSize {
			t.Fatalf("backup structures end at %d, want the disk end %d", backupOff+int64(len(newBackup)), plan.TargetSize)
		}
		copy(disk[backupOff:], newBackup)
	}
	got, err := disklayout.Parse(bytes.NewReader(disk), plan.TargetSize)
	if err != nil {
		t.Fatalf("the applied layout does not parse on the target: %v", err)
	}
	return got
}

func hasWarning(p *disklayout.FitPlan, sub string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestPlanFitLargerTargetKeepsEverythingUnlessAskedToGrow(t *testing.T) {
	img, l := upgradeFixture(t)
	primary, backup := regions(img, l)
	tg := disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512, PhysicalSector: 4096}

	plan, err := disklayout.PlanFit(l, tg, disklayout.FitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applicable() || plan.Changed() {
		t.Fatalf("a larger target without Grow changed something or refused: %+v", plan)
	}
	if plan.NewLastUsableLBA != 131072-1-32-1 {
		t.Fatalf("NewLastUsableLBA = %d, want %d", plan.NewLastUsableLBA, 131072-1-32-1)
	}
	// Every partition in this fixture starts off the 1 MiB boundary; the plan says so, per partition.
	if n := strings.Count(strings.Join(plan.Warnings, "\n"), "not on a 1024 KiB boundary"); n != 4 {
		t.Fatalf("alignment warnings = %d, want 4:\n%s", n, strings.Join(plan.Warnings, "\n"))
	}
	got := applyAndParse(t, l, plan, primary, backup)
	for i := range l.Partitions {
		if got.Partitions[i] != l.Partitions[i] {
			t.Fatalf("partition %d changed on a no-change plan:\n got  %+v\n want %+v", i, got.Partitions[i], l.Partitions[i])
		}
	}
	if got.DiskGUID != l.DiskGUID || got.AlternateLBA != 131071 {
		t.Fatalf("disk GUID %s→%s, alternate LBA %d", l.DiskGUID, got.DiskGUID, got.AlternateLBA)
	}

	// Grow without permission to move Recovery: blocked, and it says by what.
	plan, _ = disklayout.PlanFit(l, tg, disklayout.FitOptions{Grow: true})
	if plan.Changed() || !hasWarning(plan, `cannot grow`) || !hasWarning(plan, `"Recovery"`) {
		t.Fatalf("grow with Recovery in the way: changed=%v warnings=%v", plan.Changed(), plan.Warnings)
	}
}

func TestPlanFitGrowMovesRecoveryToTheEndAndGrowsTheDataPartition(t *testing.T) {
	img, l := upgradeFixture(t)
	primary, backup := regions(img, l)
	tg := disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}
	plan, err := disklayout.PlanFit(l, tg, disklayout.FitOptions{Grow: true, MoveRecoveryToEnd: true})
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: err=%v refusals=%v", err, plan.Refusals)
	}
	// By hand: last usable 131038; Recovery (4096 sectors) packs at the
	// end, aligned down to 2048: first = floor((131038+1-4096)/2048)*2048 = 126976,
	// so Recovery = [126976, 131071-? no: 126976+4096-1 = 131071]. That ends
	// past 131038 — alignDown picks 126976 only if 126976+4095 <= 131038;
	// it is not, so the pack starts from 131038: 131039-4096 = 126943 → 124928.
	data, _ := plan.Partition(2)
	rec, _ := plan.Partition(3)
	if !rec.Moved || rec.NewFirst != 124928 || rec.NewLast != 124928+4095 {
		t.Fatalf("Recovery = [%d, %d] moved=%v, want [124928, 129023] moved", rec.NewFirst, rec.NewLast, rec.Moved)
	}
	if !data.Grow || data.NewFirst != 4130 || data.NewLast != 124927 {
		t.Fatalf("data = [%d, %d] grow=%v, want [4130, 124927] grown up to Recovery", data.NewFirst, data.NewLast, data.Grow)
	}
	got := applyAndParse(t, l, plan, primary, backup)
	// GUIDs and types untouched; only extents changed; the moved
	// Recovery is aligned.
	for i := range l.Partitions {
		if got.Partitions[i].PartGUID != l.Partitions[i].PartGUID || got.Partitions[i].TypeGUID != l.Partitions[i].TypeGUID || got.Partitions[i].Name != l.Partitions[i].Name {
			t.Fatalf("partition %d identity changed: %+v vs %+v", i, got.Partitions[i], l.Partitions[i])
		}
	}
	if got.Partitions[2].LastLBA != 124927 || got.Partitions[3].FirstLBA != 124928 || got.Partitions[3].FirstLBA%2048 != 0 {
		t.Fatalf("applied extents: data ends %d, Recovery starts %d", got.Partitions[2].LastLBA, got.Partitions[3].FirstLBA)
	}
	if got.LastUsableLBA != 131038 || got.AlternateLBA != 131071 {
		t.Fatalf("header: lastUsable %d alternate %d", got.LastUsableLBA, got.AlternateLBA)
	}
}

func TestPlanFitSmallerTargetShrinksTheDataPartitionAndMovesRecovery(t *testing.T) {
	img, l := upgradeFixture(t)
	primary, backup := regions(img, l)
	tg := disklayout.TargetGeometry{Size: 32768 * 512, LogicalSector: 512}
	minSize := func(p disklayout.Partition) (int64, bool) {
		if p.Index == 2 {
			return 10000 * 512, true // the filesystem needs 10000 sectors
		}
		return 0, false
	}
	plan, err := disklayout.PlanFit(l, tg, disklayout.FitOptions{MinSize: minSize})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Applicable() {
		t.Fatalf("refused: %v", plan.Refusals)
	}
	// By hand: last usable 32734; tail was 48225 → 15491 sectors over. Data
	// shrinks by 15491 → ends 28638; Recovery re-lays at alignUp(28639) =
	// 28672 and ends 32767, 33 past the end. 33 sectors cannot move an
	// aligned partition, so the next pass takes a whole 2048-sector unit:
	// data ends 26590 (size 22461); Recovery at alignUp(26591) = 26624..30719.
	data, _ := plan.Partition(2)
	rec, _ := plan.Partition(3)
	if !data.Shrink || data.NewFirst != 4130 || data.NewLast != 26590 || data.MinBytes != 10000*512 {
		t.Fatalf("data = [%d, %d] shrink=%v min=%d, want [4130, 26590] shrunk against 5120000", data.NewFirst, data.NewLast, data.Shrink, data.MinBytes)
	}
	if !rec.Moved || rec.NewFirst != 26624 || rec.NewLast != 30719 || rec.Shrink {
		t.Fatalf("Recovery = [%d, %d] moved=%v shrink=%v, want [26624, 30719] moved, not shrunk", rec.NewFirst, rec.NewLast, rec.Moved, rec.Shrink)
	}
	if data.NewBytes(512) < data.MinBytes {
		t.Fatal("data was shrunk below its filesystem minimum")
	}
	for _, i := range []int{0, 1} {
		if p, _ := plan.Partition(i); p.Moved || p.Shrink || p.Grow {
			t.Fatalf("partition %d before the shrink changed: %+v", i, p)
		}
	}
	got := applyAndParse(t, l, plan, primary, backup)
	if got.Partitions[2].LastLBA != 26590 || got.Partitions[3].FirstLBA != 26624 || got.Partitions[3].LastLBA != 30719 || got.LastUsableLBA != 32734 {
		t.Fatalf("applied: data ends %d, Recovery [%d, %d], lastUsable %d", got.Partitions[2].LastLBA, got.Partitions[3].FirstLBA, got.Partitions[3].LastLBA, got.LastUsableLBA)
	}
	for i := range l.Partitions {
		if got.Partitions[i].PartGUID != l.Partitions[i].PartGUID {
			t.Fatalf("partition %d GUID changed", i)
		}
	}

	// Unknown minimum: still planned, and the plan says the shrink is unconfirmed.
	plan, _ = disklayout.PlanFit(l, tg, disklayout.FitOptions{})
	if !plan.Applicable() || !hasWarning(plan, "minimum size") || !hasWarning(plan, "unknown") {
		t.Fatalf("unknown minimum: refusals=%v warnings=%v", plan.Refusals, plan.Warnings)
	}
	// A filesystem that cannot shrink enough: refused, naming the shortfall.
	plan, _ = disklayout.PlanFit(l, tg, disklayout.FitOptions{MinSize: func(disklayout.Partition) (int64, bool) { return 40000 * 512, true }})
	if plan.Applicable() || !strings.Contains(strings.Join(plan.Refusals, "\n"), "too small even after shrinking") {
		t.Fatalf("unshrinkable data: applicable=%v refusals=%v", plan.Applicable(), plan.Refusals)
	}
	if _, _, _, err := disklayout.ApplyFit(l, plan, primary, backup); err == nil {
		t.Fatal("ApplyFit accepted a plan with refusals")
	}
}

func TestPlanFitRefusesASectorSizeMismatch(t *testing.T) {
	_, l := upgradeFixture(t)
	plan, err := disklayout.PlanFit(l, disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 4096}, disklayout.FitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applicable() || !strings.Contains(strings.Join(plan.Refusals, "\n"), "logical sector size is 4096") {
		t.Fatalf("4Kn target: applicable=%v refusals=%v", plan.Applicable(), plan.Refusals)
	}
	// Positive control: the same size with matching sectors is fine.
	plan, _ = disklayout.PlanFit(l, disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}, disklayout.FitOptions{})
	if !plan.Applicable() {
		t.Fatalf("512-sector target refused: %v", plan.Refusals)
	}
	// A 512e drive (physical 4096) aligns to 1 MiB, which already covers 4 KiB.
	plan, _ = disklayout.PlanFit(l, disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512, PhysicalSector: 4096}, disklayout.FitOptions{Grow: true, MoveRecoveryToEnd: true})
	rec, _ := plan.Partition(3)
	if !plan.Applicable() || rec.NewFirst%2048 != 0 {
		t.Fatalf("512e target: refusals=%v Recovery starts at %d", plan.Refusals, rec.NewFirst)
	}
}

func TestPlanFitRealignMovesEveryPrimaryPartitionOntoTheBoundary(t *testing.T) {
	img, l := upgradeFixture(t)
	primary, backup := regions(img, l)
	plan, err := disklayout.PlanFit(l, disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}, disklayout.FitOptions{Realign: true})
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: err=%v refusals=%v", err, plan.Refusals)
	}
	for _, p := range plan.Partitions {
		if p.NewFirst%2048 != 0 {
			t.Fatalf("partition %d starts at %d after realign", p.Index, p.NewFirst)
		}
		if !p.Moved || p.NewBytes(512) != p.OldBytes(512) {
			t.Fatalf("partition %d: moved=%v size %d→%d (realign must move, never resize)", p.Index, p.Moved, p.OldBytes(512), p.NewBytes(512))
		}
	}
	if hasWarning(plan, "boundary") {
		t.Fatalf("misalignment still reported after realign: %v", plan.Warnings)
	}
	got := applyAndParse(t, l, plan, primary, backup)
	if got.Partitions[0].FirstLBA != 2048 || got.Partitions[1].FirstLBA != 4096 || got.Partitions[2].FirstLBA != 6144 {
		t.Fatalf("applied starts: %d %d %d", got.Partitions[0].FirstLBA, got.Partitions[1].FirstLBA, got.Partitions[2].FirstLBA)
	}
}

func TestPlanFitMBRGrowsThePrimaryAndRefusesToMoveLogicals(t *testing.T) {
	img := gpttest.BuildMBR(t, 512, 65536, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 4096, Bootable: true},
		{Type: 0x07, Sectors: 40000},
	})
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if l.Scheme != "mbr" || len(l.Partitions) != 2 || l.Partitions[1].FirstLBA != 6144 {
		t.Fatalf("fixture: %+v", l)
	}
	primary := img[l.PrimaryRegion.Offset : l.PrimaryRegion.Offset+l.PrimaryRegion.Length]
	plan, err := disklayout.PlanFit(l, disklayout.TargetGeometry{Size: 131072 * 512, LogicalSector: 512}, disklayout.FitOptions{Grow: true})
	if err != nil || !plan.Applicable() {
		t.Fatalf("plan: err=%v refusals=%v", err, plan.Refusals)
	}
	p1, _ := plan.Partition(1)
	if !p1.Grow || p1.NewLast != 131071 {
		t.Fatalf("MBR data partition: grow=%v last=%d, want grown to 131071", p1.Grow, p1.NewLast)
	}
	newPrimary, off, backup, err := disklayout.ApplyFit(l, plan, primary, nil)
	if err != nil || off != 0 || backup != nil {
		t.Fatalf("ApplyFit MBR: err=%v off=%d backup=%v", err, off, backup != nil)
	}
	disk := make([]byte, 131072*512)
	copy(disk, newPrimary)
	got, err := disklayout.Parse(bytes.NewReader(disk), int64(len(disk)))
	if err != nil {
		t.Fatalf("applied MBR does not parse: %v", err)
	}
	if got.Partitions[1].LastLBA != 131071 || got.Partitions[0] != l.Partitions[0] || !got.Partitions[0].Bootable {
		t.Fatalf("applied MBR: %+v", got.Partitions)
	}

	// Logical partitions restore verbatim only: a shrink that would move one is refused.
	imgL := gpttest.BuildMBR(t, 512, 65536, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 30000, Bootable: true},
		{Type: 0x83, Sectors: 8192, Logical: true},
	})
	lL, err := disklayout.Parse(bytes.NewReader(imgL), int64(len(imgL)))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = disklayout.PlanFit(lL, disklayout.TargetGeometry{Size: 32768 * 512, LogicalSector: 512}, disklayout.FitOptions{MinSize: func(disklayout.Partition) (int64, bool) { return 4096 * 512, true }})
	if plan.Applicable() || !strings.Contains(strings.Join(plan.Refusals, "\n"), "logical partition") {
		t.Fatalf("moving a logical partition: applicable=%v refusals=%v", plan.Applicable(), plan.Refusals)
	}
}
