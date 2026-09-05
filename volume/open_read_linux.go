//go:build linux

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"syscall"
)

// openDeviceRead opens a device for reading with O_DIRECT to bypass the
// OS page cache, avoiding double-buffering for large sequential volume reads.
func openDeviceRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
}
