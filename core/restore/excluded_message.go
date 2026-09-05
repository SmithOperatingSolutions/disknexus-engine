// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"fmt"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// excludedFileError is the refusal for a catalog entry whose blocks were
// zeroed at capture. It names WHICH exclusion did it (#468): an operator's
// own exclusion from the device's backup settings, or the built-in set
// (volatile files, the repository's own directories). Restore day is when
// an exclusion is discovered; the message has to point at the config that
// caused it, not leave the person guessing which list to look in.
func excludedFileError(backup *manifest.Backup, f manifest.FileEntry) error {
	if backup != nil {
		if ex, ok := OperatorExclusionFor(backup.ExcludePaths, f.Path); ok {
			return fmt.Errorf("%q was excluded from capture by the exclusion %s in this device's backup settings; its blocks were zeroed and cannot be restored", f.Path, ex)
		}
	}
	return fmt.Errorf("%q was excluded from capture (built-in: volatile/repo-backend file); its blocks were zeroed and cannot be restored", f.Path)
}

// OperatorExclusionFor finds the configured exclusion (canonical
// `C:\a\b` form) that covers a catalog path (forward-slash, volume-root
// relative, with or without a leading "./"). Case-insensitive, as NTFS
// paths are; a match is the path itself or anything under it.
func OperatorExclusionFor(excludePaths []string, catalogPath string) (string, bool) {
	p := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(catalogPath, "./"), "/"))
	for _, ex := range excludePaths {
		rel := ex
		if len(rel) >= 2 && rel[1] == ':' {
			rel = rel[2:]
		}
		rel = strings.ToLower(strings.Trim(strings.ReplaceAll(rel, `\`, "/"), "/"))
		rel = strings.TrimSuffix(rel, "/*") // the operator's "everything under" spelling
		if rel == "" {
			continue
		}
		if p == rel || strings.HasPrefix(p, rel+"/") {
			return ex, true
		}
	}
	return "", false
}
