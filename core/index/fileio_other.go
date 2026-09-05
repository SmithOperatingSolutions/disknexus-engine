//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import "os"

// fileReadAt reads len(buf) bytes from f at the given byte offset.
// On non-Windows platforms this delegates to the standard ReadAt.
func fileReadAt(f *os.File, buf []byte, off int64) (int, error) {
	return f.ReadAt(buf, off)
}

// fileWriteAt writes buf to f at the given byte offset.
// On non-Windows platforms this delegates to the standard WriteAt.
func fileWriteAt(f *os.File, buf []byte, off int64) (int, error) {
	return f.WriteAt(buf, off)
}
