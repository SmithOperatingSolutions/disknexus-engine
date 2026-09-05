//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

// toRawVolumePath is a no-op on non-Windows platforms.
func toRawVolumePath(sourcePath string) string {
	return sourcePath
}
