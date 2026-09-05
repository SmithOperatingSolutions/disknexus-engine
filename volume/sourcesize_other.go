//go:build !windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"io"
	"os"
)

// SourceSize sizes an opened capture source; Seek(END) covers regular
// files and Unix block devices alike. See sourcesize_windows.go for why
// this is the ONE shared derivation (#309).
func SourceSize(f *os.File) (int64, error) {
	cur, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(cur, io.SeekStart); err != nil {
		return 0, err
	}
	return size, nil
}
