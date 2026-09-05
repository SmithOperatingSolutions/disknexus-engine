// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
)

// TestApplyProfileLowFootprint: the low-footprint profile bounds the peak
// local disk of a (cloud) run: 64 KB average chunks shrink the per-chunk
// artifacts (48 B index + 45 B sidecar entries) 8× vs the 8 KB local default,
// and 128 MB packs cap the staged in-flight pack.
func TestApplyProfileLowFootprint(t *testing.T) {
	cfg := RepoConfig{
		ChunkMinSize: 4096, ChunkAvgSize: 8192, ChunkMaxSize: 65536,
		BuzhashMask: 0x1FFF, PackFileMaxSize: 512 << 20, CompressionLevel: 3,
	}
	if err := ApplyProfile(&cfg, "low-footprint"); err != nil {
		t.Fatalf("ApplyProfile: %v", err)
	}
	if cfg.ChunkAvgSize != 64<<10 || cfg.ChunkMinSize != 16<<10 || cfg.ChunkMaxSize != 512<<10 {
		t.Fatalf("chunk sizes = %d/%d/%d", cfg.ChunkMinSize, cfg.ChunkAvgSize, cfg.ChunkMaxSize)
	}
	// The chunker mask must match the average (mask = avg-1 for power-of-two).
	if cfg.BuzhashMask != uint64(cfg.ChunkAvgSize-1) {
		t.Fatalf("BuzhashMask %#x inconsistent with avg %d", cfg.BuzhashMask, cfg.ChunkAvgSize)
	}
	if cfg.PackFileMaxSize != 128<<20 {
		t.Fatalf("PackFileMaxSize = %d", cfg.PackFileMaxSize)
	}
	// Fields outside the profile's concern are untouched.
	if cfg.CompressionLevel != 3 {
		t.Fatalf("CompressionLevel changed: %d", cfg.CompressionLevel)
	}
}

// TestApplyProfileUnknown: unknown names are refused with the valid set named.
func TestApplyProfileUnknown(t *testing.T) {
	cfg := RepoConfig{}
	err := ApplyProfile(&cfg, "nope")
	if err == nil || !strings.Contains(err.Error(), "low-footprint") {
		t.Fatalf("want error naming valid profiles, got %v", err)
	}
}

// TestApplyProfileEmptyNoop: empty profile name = no changes, no error.
func TestApplyProfileEmptyNoop(t *testing.T) {
	cfg := RepoConfig{ChunkAvgSize: 8192}
	if err := ApplyProfile(&cfg, ""); err != nil {
		t.Fatal(err)
	}
	if cfg.ChunkAvgSize != 8192 {
		t.Fatal("empty profile mutated config")
	}
}

// #83 field decision: 64 KB becomes the DEFAULT geometry (proven on real
// volume-scale capture); the old 8 KB default survives as "fine-grained"
// for small-file file-mode repos; "coarse" (128 KB) serves multi-TB image
// fleets that accept larger incrementals for halved metadata.
func TestProfileCatalog(t *testing.T) {
	base := func() RepoConfig {
		return RepoConfig{ChunkMinSize: 1, ChunkAvgSize: 2, ChunkMaxSize: 3, BuzhashMask: 1, PackFileMaxSize: 4}
	}

	cfg := base()
	if err := ApplyProfile(&cfg, "fine-grained"); err != nil {
		t.Fatal(err)
	}
	if cfg.ChunkAvgSize != 8<<10 || cfg.ChunkMinSize != 4<<10 || cfg.ChunkMaxSize != 64<<10 || cfg.PackFileMaxSize != 512<<20 {
		t.Fatalf("fine-grained = %+v", cfg)
	}

	cfg = base()
	if err := ApplyProfile(&cfg, "coarse"); err != nil {
		t.Fatal(err)
	}
	if cfg.ChunkAvgSize != 128<<10 || cfg.ChunkMinSize != 32<<10 || cfg.ChunkMaxSize != 1<<20 || cfg.PackFileMaxSize != 256<<20 {
		t.Fatalf("coarse = %+v", cfg)
	}
	if cfg.BuzhashMask != uint64(128<<10)-1 {
		t.Fatalf("coarse mask = %x, want avg-1", cfg.BuzhashMask)
	}

	// Unknown profiles name the full catalog.
	cfg = base()
	err := ApplyProfile(&cfg, "nope")
	if err == nil {
		t.Fatal("unknown profile accepted")
	}
	for _, want := range []string{"low-footprint", "fine-grained", "coarse"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should list %q: %v", want, err)
		}
	}
}

// The DEFAULT config is now the 64 KB geometry.
func TestDefaultGeometryIs64KB(t *testing.T) {
	cfg := config.Default()
	if cfg.ChunkAvgSize != 64<<10 {
		t.Fatalf("default avg = %d, want 64 KB", cfg.ChunkAvgSize)
	}
	if cfg.ChunkMinSize != 16<<10 || cfg.ChunkMaxSize != 512<<10 {
		t.Fatalf("default min/max = %d/%d, want 16K/512K", cfg.ChunkMinSize, cfg.ChunkMaxSize)
	}
	if cfg.BuzhashMask != uint64(64<<10)-1 {
		t.Fatalf("default mask = %x, want avg-1", cfg.BuzhashMask)
	}
	if cfg.PackFileMaxSize != 128<<20 {
		t.Fatalf("default pack = %d, want 128 MB", cfg.PackFileMaxSize)
	}
}
