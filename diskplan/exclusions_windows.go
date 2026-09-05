//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"path/filepath"
	"strings"
)

// volumeRelative reports lp as a volume-root-relative slash path when lp lives
// on the captured volume (volumeLetter, "C:" style), and ok=false otherwise —
// including when the capture source is not a lettered volume (image inputs).
func volumeRelative(lp, volumeLetter string) (string, bool) {
	vl := strings.TrimSuffix(volumeLetter, `\`)
	if vl == "" {
		return "", false
	}
	abs, err := filepath.Abs(lp)
	if err != nil {
		return "", false
	}
	vol := filepath.VolumeName(abs)
	if !strings.EqualFold(vol, vl) {
		return "", false
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(abs, vol), `\`)
	if rel == "" {
		return "", false // the whole volume is not an excludable subtree
	}
	return filepath.ToSlash(rel), true
}
