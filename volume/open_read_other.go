//go:build !windows && !linux

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "os"

// openDeviceRead opens a device for reading. On platforms without O_DIRECT
// support (e.g., macOS), this falls back to plain os.Open.
func openDeviceRead(path string) (*os.File, error) {
	return os.Open(path)
}
