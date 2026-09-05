// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #356 item 10: the bare literal 10000, eleven times.
//
// NewDedupIndex and NewDedupIndexReadOnly take expectedChunks, which sizes a
// bloom filter. The WRITE path computes it from the source
// (pipeline.go: totalSize/ChunkAvgSize + 1000). Every READ-side open — verify,
// restore, restore-files, report, prune, gc, resume reconciliation, the
// controller's zip restore — passes the literal 10000 instead, with no comment
// anywhere saying what it means or why that number.
//
// It is not a tuning knob at those sites: an index being opened to read
// already has its bloom on disk, and expectedChunks only sizes a fresh one.
// Eleven un-commented magic numbers are eleven invitations to "tune" one of
// them, and the reader who tries cannot tell from any single site that the
// value does not matter.

// indexModuleRoot returns the walk root and whether it is the two-module
// workspace (the product tree above the engine) or the engine module alone —
// the engine repo after the split, or GOWORK=off. The anti-vacuity floor is
// sized to which one it is.
func indexModuleRoot(t *testing.T) (root string, workspace bool) {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			// Two-module workspace (#542): the rule is codebase-wide, so
			// when a go.work sits above this module the walk starts there.
			if _, werr := os.Stat(filepath.Join(filepath.Dir(dir), "go.work")); werr == nil {
				return filepath.Dir(dir), true
			}
			return dir, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

func TestExpectedChunksIsNamedOnce(t *testing.T) {
	root, workspace := indexModuleRoot(t)
	// The calls nest (filepath.Join(...)), so match the constructor and the
	// literal in the argument list rather than trying to balance parens.
	callRe := regexp.MustCompile(`NewDedupIndex(?:ReadOnly)?\(.*,\s*10000\s*,`)
	// Anti-vacuity (docs/TESTING.md §8): on a healthy tree callRe matches
	// NOTHING, which is exactly what a scan that reached nothing reports.
	// Count files actually read, and — the positive control — the constructor
	// call sites the rule polices: they exist in this module by definition
	// (every read-side open passes ReadOpenExpectedChunks TO that constructor).
	anyCallRe := regexp.MustCompile(`\bNewDedupIndex(?:ReadOnly)?\(`)
	scanned, constructorCalls := 0, 0

	var sites []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		// Production sources only: a test fixture picking its own sizing is
		// choosing a number for that fixture, not repeating an unexplained one.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			if anyCallRe.MatchString(line) && !strings.Contains(line, "func NewDedupIndex") {
				constructorCalls++
			}
			if callRe.MatchString(line) {
				sites = append(sites, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1)+"   "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// ~20 constructor call lines today; file counts below. Floors far
	// below those: a refactor passes, but a walk that lost the module cannot
	// report "no bare 10000" by scanning nothing (docs/TESTING.md §8).
	// Workspace: 323 production files today. Engine alone: 103. Floors far
	// below each — a refactor passes, a lost walk does not.
	floor := 60
	if workspace {
		floor = 200
	}
	if scanned < floor {
		t.Fatalf("scanned only %d production .go file(s) (floor %d, workspace=%v) — the walk no longer reaches the "+
			"module, so a reintroduced bare-literal expectedChunks would pass unseen", scanned, floor, workspace)
	}
	if constructorCalls == 0 {
		t.Fatalf("scanned %d files but found no NewDedupIndex/NewDedupIndexReadOnly call at all — the "+
			"constructor this rule polices is called throughout the module, so the matcher stopped reaching "+
			"the call sites (renamed constructor, or a blind scan)", scanned)
	}
	sort.Strings(sites)
	if len(sites) > 0 {
		t.Errorf("%d read-side index opens pass the bare literal 10000 as expectedChunks:\n  %s\n"+
			"Name it once, where NewDedupIndex is, and say there what it means — an index opened to "+
			"read already has its bloom on disk, so the number is a placeholder, not a tuning knob.",
			len(sites), strings.Join(sites, "\n  "))
	}

	// The name has to exist and has to be documented where the parameter is.
	src, err := os.ReadFile("dedup.go") // cwd is this package
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "ReadOpenExpectedChunks") {
		t.Error("engine/core/index/dedup.go does not name the read-side expectedChunks placeholder — " +
			"it belongs beside the parameter it is passed to, not at eleven call sites")
	}
}
