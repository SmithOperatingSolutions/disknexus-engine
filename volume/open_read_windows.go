//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"syscall"
)

// openDeviceRead opens a device for reading with FILE_FLAG_NO_BUFFERING
// to bypass the OS page cache, avoiding double-buffering for large
// sequential volume reads.
func openDeviceRead(path string) (*os.File, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	const FILE_FLAG_NO_BUFFERING = 0x20000000

	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		FILE_FLAG_NO_BUFFERING,
		0,
	)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(h), path), nil
}
