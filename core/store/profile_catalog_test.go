// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/bits"
	"strconv"
	"strings"
	"testing"
)

// TestApplyProfileArchive: large, already-compressed data (video libraries,
// media archives, encrypted blobs, archive tiers) does not dedup, so fine
// chunking buys nothing and costs index + manifest pressure per chunk. The
// archive profile trades dedup granularity it can never use for ~8× fewer
// chunks than the default and packs big enough that a multi-TB tier is not
// millions of objects.
func TestApplyProfileArchive(t *testing.T) {
	cfg := RepoConfig{ChunkMinSize: 1, ChunkAvgSize: 2, ChunkMaxSize: 3, BuzhashMask: 1, PackFileMaxSize: 4, CompressionLevel: 7}
	if err := ApplyProfile(&cfg, "archive"); err != nil {
		t.Fatalf("ApplyProfile(archive): %v", err)
	}
	if cfg.ChunkAvgSize != 1<<20 || cfg.ChunkMinSize != 256<<10 || cfg.ChunkMaxSize != 8<<20 {
		t.Fatalf("archive chunk sizes = %d/%d/%d, want 256K/1M/8M",
			cfg.ChunkMinSize, cfg.ChunkAvgSize, cfg.ChunkMaxSize)
	}
	if cfg.BuzhashMask != uint64(cfg.ChunkAvgSize-1) {
		t.Fatalf("archive mask %#x inconsistent with avg %d", cfg.BuzhashMask, cfg.ChunkAvgSize)
	}
	if cfg.PackFileMaxSize != 1<<30 {
		t.Fatalf("archive pack = %d, want 1 GiB", cfg.PackFileMaxSize)
	}
	// Profiles only touch chunker/pack geometry.
	if cfg.CompressionLevel != 7 {
		t.Fatalf("CompressionLevel changed: %d", cfg.CompressionLevel)
	}
}

// TestApplyProfileMixed: databases and mixed fileservers rewrite records in
// place. A 64 KB chunk re-stores twice the bytes per small write that a 32 KB
// chunk does, while 8 KB doubles index pressure to buy dedup granularity these
// workloads never exploit — so 32 KB halves the default's write amplification
// at roughly twice its index cost.
func TestApplyProfileMixed(t *testing.T) {
	var cfg RepoConfig
	if err := ApplyProfile(&cfg, "mixed"); err != nil {
		t.Fatalf("ApplyProfile(mixed): %v", err)
	}
	if cfg.ChunkAvgSize != 32<<10 || cfg.ChunkMinSize != 8<<10 || cfg.ChunkMaxSize != 256<<10 {
		t.Fatalf("mixed chunk sizes = %d/%d/%d, want 8K/32K/256K",
			cfg.ChunkMinSize, cfg.ChunkAvgSize, cfg.ChunkMaxSize)
	}
	if cfg.BuzhashMask != uint64(32<<10)-1 {
		t.Fatalf("mixed mask %#x inconsistent with 32K avg", cfg.BuzhashMask)
	}
	if cfg.PackFileMaxSize != 128<<20 {
		t.Fatalf("mixed pack = %d, want the default 128 MB", cfg.PackFileMaxSize)
	}
}

// TestProfileLadderDoubles: the catalog is a clean doubling ladder from 8 KB
// to 1 MB. Every step up halves index/manifest pressure and doubles what a
// small in-place write re-stores — that is the sentence the panel's helper
// text makes to operators, so it must stay true of the actual definitions.
func TestProfileLadderDoubles(t *testing.T) {
	names := ProfileNames()
	var prev int
	for _, name := range names {
		var cfg RepoConfig
		if err := ApplyProfile(&cfg, name); err != nil {
			t.Fatal(err)
		}
		if prev != 0 && cfg.ChunkAvgSize <= prev {
			t.Fatalf("ProfileNames() must run finest→coarsest: %q avg %d follows %d", name, cfg.ChunkAvgSize, prev)
		}
		prev = cfg.ChunkAvgSize
	}
	first, last := RepoConfig{}, RepoConfig{}
	if err := ApplyProfile(&first, names[0]); err != nil {
		t.Fatal(err)
	}
	if err := ApplyProfile(&last, names[len(names)-1]); err != nil {
		t.Fatal(err)
	}
	if first.ChunkAvgSize != 8<<10 || last.ChunkAvgSize != 1<<20 {
		t.Fatalf("ladder runs %d..%d, want 8 KB..1 MB", first.ChunkAvgSize, last.ChunkAvgSize)
	}
	if len(names) != 5 {
		t.Fatalf("want 5 profiles on the ladder, got %d: %v", len(names), names)
	}
}

// TestApplyProfileUnknownNamesArchive: the "valid:" list in the error is what
// an operator (and the CLI) reads to discover profiles — a new profile that
// is not listed there is invisible.
func TestApplyProfileUnknownNamesWholeCatalog(t *testing.T) {
	err := ApplyProfile(&RepoConfig{}, "no-such-profile")
	if err == nil {
		t.Fatal("unknown profile accepted")
	}
	for _, want := range ProfileNames() {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should list %q: %v", want, err)
		}
	}
}

// TestProfileNamesMatchesApplyProfileCases is the drift guard at the source of
// truth: ProfileNames() is what the controller exposes to the panel, so a
// profile added to ApplyProfile's switch but not to ProfileNames() would exist
// in the CLI and be invisible in the UI. Parse the switch and compare.
func TestProfileNamesMatchesApplyProfileCases(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "profile.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "ApplyProfile" {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			cc, ok := m.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil || v == "" { // "" is the documented no-op, not a profile
					continue
				}
				cases = append(cases, v)
			}
			return true
		})
		return false
	})
	if len(cases) == 0 {
		t.Fatal("found no profile cases in ApplyProfile — did the switch move?")
	}
	listed := map[string]bool{}
	for _, n := range ProfileNames() {
		listed[n] = true
	}
	for _, c := range cases {
		if !listed[c] {
			t.Errorf("profile %q is implemented in ApplyProfile but missing from ProfileNames()", c)
		}
	}
	if len(ProfileNames()) != len(cases) {
		t.Errorf("ProfileNames() = %v, ApplyProfile cases = %v", ProfileNames(), cases)
	}
}

// TestEveryProfileIsInternallyConsistent: the mask IS what produces the
// average, so a profile whose average is not a power of two (or whose mask
// disagrees with it) would chunk to something other than what its config
// claims. Ordering must hold for the chunker's min/max clamps too.
func TestEveryProfileIsInternallyConsistent(t *testing.T) {
	for _, name := range ProfileNames() {
		var cfg RepoConfig
		if err := ApplyProfile(&cfg, name); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bits.OnesCount64(uint64(cfg.ChunkAvgSize)) != 1 {
			t.Errorf("%s: avg %d is not a power of two", name, cfg.ChunkAvgSize)
		}
		if cfg.BuzhashMask != uint64(cfg.ChunkAvgSize-1) {
			t.Errorf("%s: mask %#x != avg-1 (%d)", name, cfg.BuzhashMask, cfg.ChunkAvgSize-1)
		}
		if !(0 < cfg.ChunkMinSize && cfg.ChunkMinSize <= cfg.ChunkAvgSize && cfg.ChunkAvgSize <= cfg.ChunkMaxSize) {
			t.Errorf("%s: min/avg/max out of order: %d/%d/%d", name, cfg.ChunkMinSize, cfg.ChunkAvgSize, cfg.ChunkMaxSize)
		}
		if cfg.PackFileMaxSize < int64(cfg.ChunkMaxSize) {
			t.Errorf("%s: pack %d cannot hold one max-size chunk (%d)", name, cfg.PackFileMaxSize, cfg.ChunkMaxSize)
		}
	}
}
