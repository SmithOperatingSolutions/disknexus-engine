//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "os"

// openDevice opens a device path for writing on non-Windows platforms.
func openDevice(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR, 0644)
}

// deviceWriteAt writes data at the given offset. No alignment needed on non-Windows.
func deviceWriteAt(f *os.File, data []byte, offset int64) (int, error) {
	return f.WriteAt(data, offset)
}

// deviceSize returns the size of a raw device.
// On non-Windows platforms, stat is sufficient; this path is unused.
func deviceSize(_ *os.File) (int64, error) {
	return 0, nil
}

// VolumeSize is a no-op on non-Windows platforms.
func VolumeSize(_ string) (int64, error) {
	return 0, nil
}
