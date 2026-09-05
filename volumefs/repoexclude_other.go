//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import "github.com/SmithOperatingSolutions/disknexus-engine/volume"

// AddRepoExclusionRanges is a no-op on non-Windows platforms.
func AddRepoExclusionRanges(repoPath string, m *volume.ExclusionMap) {}
