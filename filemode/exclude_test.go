// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatcherBasicGlob(t *testing.T) {
	m := NewMatcher([]string{"*.tmp", "*.log"})

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"file.tmp", false, true},
		{"dir/file.tmp", false, true},
		{"file.log", false, true},
		{"file.go", false, false},
		{"file.txt", false, false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("IsExcluded(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestMatcherDirOnly(t *testing.T) {
	m := NewMatcher([]string{"node_modules/"})

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"src/node_modules", true, true},
		{"node_modules", false, false}, // file named node_modules — not excluded
		{"node_modules_extra", true, false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("IsExcluded(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestMatcherDoubleGlob(t *testing.T) {
	m := NewMatcher([]string{"**/test/*.go"})

	tests := []struct {
		path string
		want bool
	}{
		{"test/main.go", true},
		{"src/test/main.go", true},
		{"a/b/test/main.go", true},
		{"test/main.txt", false},
		{"src/main.go", false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, false)
		if got != tt.want {
			t.Errorf("IsExcluded(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMatcherNegation(t *testing.T) {
	m := NewMatcher([]string{"*.log", "!important.log"})

	tests := []struct {
		path string
		want bool
	}{
		{"debug.log", true},
		{"error.log", true},
		{"important.log", false}, // negated
		{"file.txt", false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, false)
		if got != tt.want {
			t.Errorf("IsExcluded(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMatcherExactPath(t *testing.T) {
	m := NewMatcher([]string{"src/generated/api.go"})

	tests := []struct {
		path string
		want bool
	}{
		{"src/generated/api.go", true},
		{"src/other/api.go", false},
		{"api.go", false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, false)
		if got != tt.want {
			t.Errorf("IsExcluded(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestMatcherCommentAndBlank(t *testing.T) {
	m := NewMatcher([]string{"# comment", "", "  ", "*.tmp"})

	if len(m.patterns) != 1 {
		t.Errorf("got %d patterns, want 1", len(m.patterns))
	}

	if !m.IsExcluded("file.tmp", false) {
		t.Error("expected file.tmp to be excluded")
	}
}

func TestMatcherNil(t *testing.T) {
	var m *Matcher
	if m.IsExcluded("anything", false) {
		t.Error("nil matcher should not exclude anything")
	}
}

func TestMatcherEmpty(t *testing.T) {
	m := NewMatcher(nil)
	if m.IsExcluded("anything", false) {
		t.Error("empty matcher should not exclude anything")
	}
}

func TestMatcherMerge(t *testing.T) {
	m1 := NewMatcher([]string{"*.tmp"})
	m2 := NewMatcher([]string{"*.log"})
	m1.Merge(m2)

	if !m1.IsExcluded("file.tmp", false) {
		t.Error("expected .tmp excluded after merge")
	}
	if !m1.IsExcluded("file.log", false) {
		t.Error("expected .log excluded after merge")
	}
}

func TestLoadMatcherFromFile(t *testing.T) {
	dir := t.TempDir()
	excludeFile := filepath.Join(dir, ".backupignore")

	content := "# exclude temp files\n*.tmp\n\n# exclude build output\nbuild/\n"
	if err := os.WriteFile(excludeFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	m, err := LoadMatcherFromFile(excludeFile)
	if err != nil {
		t.Fatalf("LoadMatcherFromFile: %v", err)
	}

	if !m.IsExcluded("file.tmp", false) {
		t.Error("expected .tmp excluded")
	}
	if !m.IsExcluded("build", true) {
		t.Error("expected build/ excluded")
	}
	if m.IsExcluded("file.go", false) {
		t.Error("expected .go not excluded")
	}
}

func TestExcludeRepoDir(t *testing.T) {
	// Repo is a subdirectory of source
	src := t.TempDir()
	repo := filepath.Join(src, "backups", "myrepo")
	os.MkdirAll(repo, 0755)

	m := ExcludeRepoDir(repo, []string{src})
	if m == nil {
		t.Fatal("expected non-nil matcher when repo is under source")
	}
	if !m.IsExcluded("backups/myrepo", true) {
		t.Error("expected repo dir to be excluded")
	}
	if !m.IsExcluded("backups/myrepo/packs", true) {
		t.Error("expected repo subdir to be excluded")
	}
	if m.IsExcluded("backups/other", true) {
		t.Error("expected sibling dir to not be excluded")
	}
	if m.IsExcluded("documents/file.txt", false) {
		t.Error("expected unrelated file to not be excluded")
	}
}

func TestExcludeRepoDirOutside(t *testing.T) {
	// Repo is completely outside the source — should return nil
	src := t.TempDir()
	repo := t.TempDir()

	m := ExcludeRepoDir(repo, []string{src})
	if m != nil {
		t.Error("expected nil matcher when repo is outside source")
	}
}

func TestExcludeRepoDirSameAsSource(t *testing.T) {
	// Repo IS the source — unusual case, should return nil (no exclusion)
	dir := t.TempDir()

	m := ExcludeRepoDir(dir, []string{dir})
	if m != nil {
		t.Error("expected nil matcher when repo equals source")
	}
}

func TestMatcherTrailingDoubleGlob(t *testing.T) {
	m := NewMatcher([]string{"logs/**"})

	tests := []struct {
		path string
		want bool
	}{
		{"logs/app.log", true},
		{"logs/2024/app.log", true},
		{"src/logs/app.log", false},
	}

	for _, tt := range tests {
		got := m.IsExcluded(tt.path, false)
		if got != tt.want {
			t.Errorf("IsExcluded(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}
