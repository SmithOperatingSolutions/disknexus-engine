// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// Catalog holds the result of walking source directories.
type Catalog struct {
	SourcePaths []string
	Files       []manifest.FileEntry // sorted by (SourceIndex, Path)
	TotalSize   int64
}

// WalkProgress is passed to the optional progress callback during Walk.
type WalkProgress struct {
	Dirs  int
	Files int
}

// Walk enumerates files in the given source paths, applying exclusions,
// and returns a deterministically ordered catalog with stream offsets assigned.
// The optional onProgress callback is invoked after each entry is discovered.
func Walk(ctx context.Context, sourcePaths []string, matcher *Matcher, onProgress func(WalkProgress)) (*Catalog, error) {
	cat := &Catalog{
		SourcePaths: sourcePaths,
	}

	var dirs, files int

	for srcIdx, srcPath := range sourcePaths {
		srcPath, err := filepath.Abs(srcPath)
		if err != nil {
			return nil, fmt.Errorf("resolving %s: %w", srcPath, err)
		}

		info, err := os.Lstat(srcPath)
		if err != nil {
			return nil, fmt.Errorf("stat %s: %w", srcPath, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", srcPath)
		}

		err = filepath.WalkDir(srcPath, func(path string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}

			if walkErr != nil {
				return walkErr
			}

			relPath, err := filepath.Rel(srcPath, path)
			if err != nil {
				return err
			}
			if relPath == "." {
				return nil
			}
			relPath = filepath.ToSlash(relPath)

			isDir := d.IsDir()

			// Check for symlinks
			linfo, err := os.Lstat(path)
			if err != nil {
				return err
			}
			isSymlink := linfo.Mode()&os.ModeSymlink != 0

			if matcher != nil && matcher.IsExcluded(relPath, isDir) {
				if isDir {
					return filepath.SkipDir
				}
				return nil
			}

			entry := manifest.FileEntry{
				Path:        relPath,
				SourceIndex: srcIdx,
				Mode:        uint32(linfo.Mode().Perm()),
				ModTime:     linfo.ModTime(),
				IsDir:       isDir,
				IsSymlink:   isSymlink,
			}

			if isSymlink {
				target, err := os.Readlink(path)
				if err != nil {
					return fmt.Errorf("readlink %s: %w", path, err)
				}
				entry.LinkTarget = filepath.ToSlash(target)
			} else if !isDir {
				entry.Size = linfo.Size()
			}

			cat.Files = append(cat.Files, entry)

			if isDir {
				dirs++
			} else {
				files++
			}
			if onProgress != nil {
				onProgress(WalkProgress{Dirs: dirs, Files: files})
			}

			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", srcPath, err)
		}
	}

	// Sort deterministically by (SourceIndex, Path)
	sort.Slice(cat.Files, func(i, j int) bool {
		if cat.Files[i].SourceIndex != cat.Files[j].SourceIndex {
			return cat.Files[i].SourceIndex < cat.Files[j].SourceIndex
		}
		return cat.Files[i].Path < cat.Files[j].Path
	})

	// Assign stream offsets — only regular files with size > 0 contribute
	var offset int64
	for i := range cat.Files {
		f := &cat.Files[i]
		if f.IsDir || f.IsSymlink || f.Size == 0 {
			continue
		}
		f.StreamOffset = offset
		f.StreamLength = f.Size
		offset += f.Size
	}
	cat.TotalSize = offset

	return cat, nil
}

// FilterByPrefix returns entries whose path starts with the given prefix.
func (c *Catalog) FilterByPrefix(prefix string) []manifest.FileEntry {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return c.Files
	}

	var result []manifest.FileEntry
	for _, f := range c.Files {
		if f.Path == prefix || strings.HasPrefix(f.Path, prefix+"/") {
			result = append(result, f)
		}
	}
	return result
}
