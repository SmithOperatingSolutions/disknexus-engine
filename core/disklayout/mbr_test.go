// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"bytes"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
)

// Native MBR support (#149, reopens #88): the #83 validation machine is
// MBR-partitioned and Win10-on-BIOS fleets are a real population. Parse
// must recognize a genuine MBR disk natively instead of failing with
// "no GPT signature".

func TestParseMBRPrimaries(t *testing.T) {
	img := gpttest.BuildMBR(t, 512, 1<<20, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 100 * 2048, Bootable: true}, // NTFS system
		{Type: 0x07, Sectors: 200 * 2048},                 // NTFS data
	})
	l, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("MBR disk must parse natively: %v", err)
	}
	if l.Scheme != "mbr" {
		t.Fatalf("scheme = %q, want mbr", l.Scheme)
	}
	if len(l.Partitions) != 2 {
		t.Fatalf("partitions = %d, want 2", len(l.Partitions))
	}
	p0 := l.Partitions[0]
	if p0.MBRType != 0x07 || !p0.Bootable || p0.FirstLBA != 2048 {
		t.Fatalf("p0 = %+v", p0)
	}
	if p0.LastLBA != 2048+100*2048-1 {
		t.Fatalf("p0 LastLBA = %d (inclusive expected)", p0.LastLBA)
	}
	if p0.TypeName != "NTFS/exFAT" {
		t.Fatalf("p0 TypeName = %q", p0.TypeName)
	}
	// Boot track: LBA0 through the first partition start, captured verbatim.
	if l.PrimaryRegion.Offset != 0 || l.PrimaryRegion.Length != 2048*512 {
		t.Fatalf("boot track = %+v, want [0, first partition)", l.PrimaryRegion)
	}
}

func TestParseMBRExtendedChain(t *testing.T) {
	img := gpttest.BuildMBR(t, 512, 1<<21, []gpttest.SynthMBRPart{
		{Type: 0x07, Sectors: 100 * 2048, Bootable: true},
		{Type: 0x07, Sectors: 50 * 2048, Logical: true},
		{Type: 0x0C, Sectors: 60 * 2048, Logical: true},
	})
	l, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	// 1 primary + 2 logicals; the extended CONTAINER is not listed as a
	// capturable partition (its logicals are).
	if len(l.Partitions) != 3 {
		t.Fatalf("partitions = %d, want 3 (primary + 2 logicals): %+v", len(l.Partitions), l.Partitions)
	}
	log1, log2 := l.Partitions[1], l.Partitions[2]
	if log1.MBRType != 0x07 || log2.MBRType != 0x0C {
		t.Fatalf("logical types = %02x/%02x", log1.MBRType, log2.MBRType)
	}
	if log1.FirstLBA <= l.Partitions[0].LastLBA || log2.FirstLBA <= log1.LastLBA {
		t.Fatalf("logical LBAs not absolute/ordered: %+v", l.Partitions)
	}
	if log1.LastLBA-log1.FirstLBA+1 != 50*2048 {
		t.Fatalf("log1 length = %d sectors", log1.LastLBA-log1.FirstLBA+1)
	}
}

// A protective MBR (0xEE) without a valid GPT header is a CORRUPT GPT —
// not an MBR disk; the error must say so instead of "no GPT signature".
func TestParseProtectiveMBRWithoutGPTIsCorrupt(t *testing.T) {
	img := gpttest.BuildMBR(t, 512, 1<<20, []gpttest.SynthMBRPart{
		{Type: 0xEE, Sectors: (1 << 20) - 2048},
	})
	_, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err == nil {
		t.Fatal("protective MBR without GPT header accepted")
	}
	if !strings.Contains(err.Error(), "corrupt") || !strings.Contains(err.Error(), "GPT") {
		t.Fatalf("error should identify corrupt GPT: %v", err)
	}
}

// GPT parsing is unchanged (zero-regression guard for the probe reorder).
func TestParseGPTUnaffectedByMBRSupport(t *testing.T) {
	img := gpttest.BuildGPT(t, 512, 1<<20, gpttest.StdWindowsParts())
	l, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if l.Scheme != "gpt" {
		t.Fatalf("scheme = %q, want gpt", l.Scheme)
	}
	if len(l.Partitions) != len(gpttest.StdWindowsParts()) {
		t.Fatalf("gpt partitions = %d", len(l.Partitions))
	}
}
