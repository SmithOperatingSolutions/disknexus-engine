//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

// volumeRelative: same-volume detection for repo/temp exclusion needs a
// path→volume mapping, which exists only for Windows drive letters today.
// On Unix (no VSS, mount-table mapping unimplemented) no local path ever
// matches, so no exclusion is attempted — same contract as
// volumefs.AddRepoExclusionRanges being a Windows-only operation.
func volumeRelative(lp, volumeLetter string) (string, bool) { return "", false }
