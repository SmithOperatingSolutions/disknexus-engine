// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import "testing"

// TestMultiSegmentPatternIsRootAnchored proves that a pattern containing a
// slash matches only at the root, per gitignore semantics. The old
// matchSegments did a suffix scan at every alignment, so "foo/bar" wrongly
// matched "x/foo/bar" — and via ExcludeRepoDir this silently excluded
// unrelated user directories that happened to share the repo's tail path.
func TestMultiSegmentPatternIsRootAnchored(t *testing.T) {
	m := NewMatcher([]string{"foo/bar/"})
	if m.IsExcluded("x/foo/bar", true) {
		t.Error(`"foo/bar/" must not match "x/foo/bar" (pattern is root-anchored)`)
	}
	if !m.IsExcluded("foo/bar", true) {
		t.Error(`"foo/bar/" must match "foo/bar" at the root`)
	}
}

// TestLeadingSlashPatternMatches proves that a leading-slash pattern like
// "/build" is honored. The old parsePattern split it into ["", "build"] and
// filepath.Match("", "build") is always false, so it silently matched
// nothing and the directory was backed up despite being excluded.
func TestLeadingSlashPatternMatches(t *testing.T) {
	m := NewMatcher([]string{"/build"})
	if !m.IsExcluded("build", true) {
		t.Error(`"/build" must match "build" at the root`)
	}
	if m.IsExcluded("src/build", true) {
		t.Error(`"/build" must not match "src/build" (anchored to root)`)
	}
}

// TestExcludeRepoDirDirectlyUnderSourceIsAnchored proves that when the repo
// sits directly under the source, ExcludeRepoDir excludes only that path,
// not every same-named directory in the tree. The old code emitted a
// single-segment pattern "repo/" which matched a basename at any depth,
// silently dropping unrelated "repo" directories from the backup.
func TestExcludeRepoDirDirectlyUnderSourceIsAnchored(t *testing.T) {
	m := ExcludeRepoDir("/data/repo", []string{"/data"})
	if m == nil {
		t.Fatal("ExcludeRepoDir returned nil")
	}
	if !m.IsExcluded("repo", true) {
		t.Error(`repo directly under source must be excluded at the root`)
	}
	if m.IsExcluded("projects/foo/repo", true) {
		t.Error(`an unrelated nested "repo" directory must not be excluded`)
	}
}
