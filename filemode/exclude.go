// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// pattern represents a single exclusion or inclusion rule.
type pattern struct {
	raw      string // original pattern text
	negate   bool   // true for !pattern (re-include)
	dirOnly  bool   // true for patterns ending with /
	anchored bool   // true if rooted (leading slash or an internal slash)
	segments []string
	hasGlob  bool // contains ** (recursive)
}

// Matcher evaluates gitignore-style exclusion patterns against file paths.
type Matcher struct {
	patterns []pattern
}

// NewMatcher creates a Matcher from a list of gitignore-style patterns.
func NewMatcher(rawPatterns []string) *Matcher {
	m := &Matcher{}
	for _, raw := range rawPatterns {
		if p, ok := parsePattern(raw); ok {
			m.patterns = append(m.patterns, p)
		}
	}
	return m
}

// LoadMatcherFromFile reads patterns from a file (one per line, # comments).
func LoadMatcherFromFile(path string) (*Matcher, error) {
	patterns, err := PatternsFromFile(path)
	if err != nil {
		return nil, err
	}
	return NewMatcher(patterns), nil
}

// PatternsFromFile reads the raw pattern lines of an exclude file — the one
// parse LoadMatcherFromFile builds its Matcher from, exposed for callers that
// need the pattern LIST (the cloud files backup carries excludes as raw
// patterns, not as a matcher).
func PatternsFromFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var patterns []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		patterns = append(patterns, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}

// Merge adds all patterns from other into this matcher.
func (m *Matcher) Merge(other *Matcher) {
	if other != nil {
		m.patterns = append(m.patterns, other.patterns...)
	}
}

// IsExcluded returns true if the given path should be excluded.
// relPath uses forward slashes and is relative to the source root.
func (m *Matcher) IsExcluded(relPath string, isDir bool) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}

	excluded := false
	for _, p := range m.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if matchPattern(p, relPath) {
			excluded = !p.negate
		}
	}
	return excluded
}

func parsePattern(raw string) (pattern, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || s[0] == '#' {
		return pattern{}, false
	}

	p := pattern{raw: s}

	if s[0] == '!' {
		p.negate = true
		s = s[1:]
	}

	if strings.HasSuffix(s, "/") {
		p.dirOnly = true
		s = strings.TrimSuffix(s, "/")
	}

	// Normalize to forward slashes
	s = filepath.ToSlash(s)

	// Gitignore anchoring: a leading slash roots the pattern; strip it. A
	// pattern that still contains a slash is also rooted (matches only from
	// the source root, not at arbitrary depth). A bare single segment
	// matches a basename anywhere.
	if strings.HasPrefix(s, "/") {
		p.anchored = true
		s = strings.TrimPrefix(s, "/")
	} else if strings.Contains(s, "/") {
		p.anchored = true
	}

	p.segments = strings.Split(s, "/")
	for _, seg := range p.segments {
		if seg == "**" {
			p.hasGlob = true
			break
		}
	}

	return p, true
}

// matchPattern checks if relPath matches the pattern.
func matchPattern(p pattern, relPath string) bool {
	pathSegments := strings.Split(relPath, "/")

	if p.hasGlob {
		return matchGlob(p.segments, pathSegments)
	}

	// An unanchored single segment matches a basename at any depth
	// (e.g. "build/" excludes every "build" directory).
	if len(p.segments) == 1 && !p.anchored {
		basename := pathSegments[len(pathSegments)-1]
		ok, _ := filepath.Match(p.segments[0], basename)
		return ok
	}

	// Anchored pattern: match as a prefix of the path segments from the
	// root, so "foo/bar" excludes "foo/bar" and everything beneath it, but
	// never a same-named path nested elsewhere.
	return matchSegments(p.segments, pathSegments)
}

// matchSegments matches pattern segments as a root-anchored prefix of the
// path segments. A shorter pattern matches a directory and all its contents.
func matchSegments(patSegs, pathSegs []string) bool {
	if len(patSegs) > len(pathSegs) {
		return false
	}
	return matchSegmentsAt(patSegs, pathSegs, 0)
}

func matchSegmentsAt(patSegs, pathSegs []string, offset int) bool {
	for i, ps := range patSegs {
		ok, _ := filepath.Match(ps, pathSegs[offset+i])
		if !ok {
			return false
		}
	}
	return true
}

// ExcludeRepoDir returns a Matcher that excludes repoPath when it falls under
// any of the given source directories. Returns nil if the repo is not under any
// source path. This prevents accidentally backing up the repository into itself.
func ExcludeRepoDir(repoPath string, sourcePaths []string) *Matcher {
	repoAbs, err := filepath.Abs(repoPath)
	if err != nil {
		return nil
	}

	var patterns []string
	for _, src := range sourcePaths {
		srcAbs, err := filepath.Abs(src)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(srcAbs, repoAbs)
		if err != nil {
			continue
		}
		// rel starts with ".." means repo is not under this source;
		// rel == "." means repo IS the source (unusual, skip)
		if strings.HasPrefix(rel, "..") || rel == "." {
			continue
		}
		// Anchor with a leading slash so the repo path is excluded only at
		// its exact location under the source, never a same-named directory
		// elsewhere in the tree.
		patterns = append(patterns, "/"+filepath.ToSlash(rel)+"/")
	}
	if len(patterns) == 0 {
		return nil
	}
	return NewMatcher(patterns)
}

// matchGlob handles patterns containing **.
func matchGlob(patSegs, pathSegs []string) bool {
	return matchGlobRecursive(patSegs, pathSegs)
}

func matchGlobRecursive(patSegs, pathSegs []string) bool {
	for len(patSegs) > 0 {
		ps := patSegs[0]
		if ps == "**" {
			patSegs = patSegs[1:]
			if len(patSegs) == 0 {
				// trailing ** matches everything
				return true
			}
			// ** can match zero or more path segments
			for i := 0; i <= len(pathSegs); i++ {
				if matchGlobRecursive(patSegs, pathSegs[i:]) {
					return true
				}
			}
			return false
		}

		if len(pathSegs) == 0 {
			return false
		}

		ok, _ := filepath.Match(ps, pathSegs[0])
		if !ok {
			return false
		}

		patSegs = patSegs[1:]
		pathSegs = pathSegs[1:]
	}

	return len(pathSegs) == 0
}
