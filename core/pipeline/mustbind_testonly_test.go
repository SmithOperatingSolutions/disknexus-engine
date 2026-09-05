// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMustBindIsTestOnly: MustBind panics instead of returning an error, so a
// production caller that reached for it would be re-opening the exact hole
// #265 closed — a way to get a Pipeline without handling the case where the
// repo's key could not be resolved.
//
// Same genre as the controller's TestPanelDoesNotHardcodeCompressionChoices:
// a rule the compiler cannot state, held by a source scan over the whole
// module rather than a review convention.
func TestMustBindIsTestOnly(t *testing.T) {
	root, workspace := moduleRoot(t)
	fset := token.NewFileSet()
	// Anti-vacuity (docs/TESTING.md §8): on a healthy tree this scan finds
	// ZERO MustBind calls, which is indistinguishable from a scan that reached
	// zero files. Count what was actually cleared, and separately confirm the
	// walk still reaches the pipeline package itself — the package whose
	// unqualified `MustBind(` spelling the second matcher arm exists for.
	parsed, pipelinePkgFiles := 0, 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// .claude holds agent worktrees — full checkouts of OTHER commits;
			// scanning them reports code that is not on this branch.
			case ".git", "node_modules", "vendor", "dist", ".claude", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// Not this test's job to police unparseable files.
			return nil
		}
		parsed++
		inPipelinePkg := f.Name.Name == "pipeline"
		if inPipelinePkg {
			pipelinePkgFiles++
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok && id.Name == "pipeline" && fn.Sel.Name == "MustBind" {
					t.Errorf("%s: pipeline.MustBind in production code (#265) — "+
						"call pipeline.Bind and handle the error; a repo whose key cannot be resolved "+
						"must fail the backup, and MustBind exists only so tests can stay terse",
						fset.Position(call.Pos()))
				}
			case *ast.Ident:
				if inPipelinePkg && fn.Name == "MustBind" {
					t.Errorf("%s: MustBind in production code (#265) — call Bind and handle the error",
						fset.Position(call.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Workspace: ~320 production files parse today; the engine alone: ~105.
	// 2 of them are package pipeline. Floors far below each, so growth or
	// refactors pass, but a walk that lost the module — moved root,
	// over-broad skip — cannot clear MustBind by parsing nothing.
	floor := 60
	if workspace {
		floor = 200
	}
	if parsed < floor {
		t.Fatalf("parsed only %d production .go file(s) (floor %d, workspace=%v) — the walk no longer reaches the "+
			"module, so a production pipeline.MustBind call (a keyless-repo panic path, #265) would pass unseen",
			parsed, floor, workspace)
	}
	if pipelinePkgFiles == 0 {
		t.Fatalf("parsed %d files but none in package pipeline — the unqualified-MustBind arm of this rule "+
			"covers nothing, and an in-package production caller would pass unseen", parsed)
	}
}

// moduleRoot walks up from the working directory to the directory holding
// go.mod.
// moduleRoot returns the walk root and whether it is the two-module
// workspace or the engine module alone (the engine repo after the split, or
// GOWORK=off). The anti-vacuity floor is sized to which one it is.
func moduleRoot(t *testing.T) (root string, workspace bool) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			// The engine is one module of a two-module workspace (#542): a
			// production MustBind call in the PRODUCT module is just as
			// much a keyless-repo panic path, so the walk covers the whole
			// repository when a go.work sits above this module.
			if _, werr := os.Stat(filepath.Join(filepath.Dir(dir), "go.work")); werr == nil {
				return filepath.Dir(dir), true
			}
			return dir, false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the working directory")
		}
		dir = parent
	}
}
