//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func nativeScanAvailable(_ string) bool { return false }
func nativeScan(_ context.Context, _ string, _ func(int, int), _ string) ([]manifest.FileEntry, error) {
	return nil, nil
}
