// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// IndexKeyFor is documented as "the single place that rule lives" (#265) — the
// rule being that a MANAGED repo's dedup index is written in the CLEAR, because
// the controller's server-side restore opens it with a nil key while opening
// the chunk store with the DEK.
//
// That claim was not true when it was written: the rule was adopted on the
// write side and in restorezip.go, while seven CLI read paths still re-derived
// it by hand and two more sites (prune, and export/import) simply passed the
// chunk key. Passing the chunk key there is not a cosmetic slip — index.NewDedupIndex
// treats an existing plaintext bloom.bin as a decrypted working copy and
// deletes it on close, so a single such call silently strands every backup in
// the repo.
//
// This test makes the claim enforceable: every dedup-index open in the module
// must take its key from IndexKeyFor or from a Binding (which is built on
// IndexKeyFor), or pass nil outright. A new call site that reaches for the
// chunk key fails here rather than in a customer's repo.
func TestEveryDedupIndexOpenDerivesItsKeyFromIndexKeyFor(t *testing.T) {
	root := moduleRoot(t)
	// Any qualifier, not just `index.`. The previous spelling had two branches —
	// a literal `index.` prefix, or an identifier NOT preceded by a dot — and an
	// import alias defeats both at once: in `idx.NewDedupIndex(` the character
	// before the identifier IS a dot, so the second branch excludes it and the
	// first does not match. A managed-repo open under an alias scanned clean
	// (#402).
	call := regexp.MustCompile(`(?:\b\w+\.)?NewDedupIndex(?:ReadOnly)?\(`)
	// The independent authority this scanner is checked against: the bare
	// identifier, found without any structure. Anything the plain identifier
	// sees but the structured matcher misses is a call site this rule stopped
	// covering, which is the failure mode a `checked > 0` counter alone does not
	// catch (docs/TESTING.md §8).
	ident := regexp.MustCompile(`\bNewDedupIndex(?:ReadOnly)?\b`)
	var matched, present int

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "test-results", ".claude":
				// .claude holds agent worktrees: full checkouts of OTHER
				// commits. Scanning them makes this test report code that is
				// not on this branch — a pre-#265 worktree fails a green main.
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(src), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") ||
				strings.Contains(line, "func NewDedupIndex") {
				continue // a comment, or the constructor itself
			}
			if ident.MatchString(line) {
				present++
			}
			loc := call.FindStringIndex(line)
			if loc == nil {
				continue
			}
			matched++
			arg, ok := lastCallArg(line[loc[0]:])
			if !ok {
				t.Errorf("%s:%d: dedup-index open spans lines; keep the call on one line so the "+
					"index-key rule stays checkable:\n\t%s", rel, i+1, strings.TrimSpace(line))
				continue
			}
			if !allowedIndexKeyArg(arg) {
				t.Errorf("%s:%d: dedup index opened with key %q — it must come from store.IndexKeyFor "+
					"(or a pipeline.Binding's IndexKey, which is built on it), or be nil. A managed repo's "+
					"index is PLAINTEXT; opening it with the chunk key deletes bloom.bin and hash-index.db "+
					"and strands every backup (#265).\n\t%s", rel, i+1, arg, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Anti-vacuity. "Found no violations" and "matched nothing" are the same
	// result from the outside, and this rule's whole value is that it covers
	// EVERY open — one uncovered call site "silently strands every backup in
	// the repo", per this file's own opening claim.
	if matched == 0 {
		t.Fatalf("this rule scanned the module and matched no dedup-index open at all, so it is "+
			"enforcing nothing. Either the constructor was renamed or the matcher stopped reaching it "+
			"(the plain identifier appears on %d line(s))", present)
	}
	if matched != present {
		t.Errorf("the index-key rule matched %d dedup-index open(s) but the bare identifier appears on %d "+
			"non-comment line(s): %d call site(s) are no longer covered by this rule. A respelling — an "+
			"import alias, a wrapper, a method — removes a call site from the rule silently, which is how "+
			"a managed repo gets opened with the chunk key and loses bloom.bin and hash-index.db (#265)",
			matched, present, present-matched)
	}
}

// allowedIndexKeyArg: nil, an IndexKeyFor call, or something's IndexKey().
// p.indexKey is the pipeline's own field, which pipeline.Bind fills from
// IndexKeyFor and nothing else can set.
func allowedIndexKeyArg(arg string) bool {
	switch {
	case arg == "nil", arg == "p.indexKey":
		return true
	case strings.HasSuffix(arg, "IndexKey()"):
		return true
	case strings.Contains(arg, "IndexKeyFor("):
		return true
	}
	return false
}

// lastCallArg returns the final argument of the call that starts at the front
// of s, respecting nesting. ok=false when the call does not close on this line.
func lastCallArg(s string) (string, bool) {
	open := strings.Index(s, "(")
	if open < 0 {
		return "", false
	}
	depth := 0
	start := open + 1
	last := ""
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start:i]), true
			}
		case ',':
			if depth == 1 {
				last = strings.TrimSpace(s[start:i])
				start = i + 1
			}
		}
	}
	_ = last
	return "", false
}

func moduleRoot(t *testing.T) string {
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
				return filepath.Dir(dir)
			}
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}
