// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"fmt"
	"os"
	"strings"
)

// Writer provides random-access writing to a file or device for restoring backups.
type Writer struct {
	file *os.File
	path string
}

// isDevicePath returns true for Windows device paths like \\.\PhysicalDriveN
// and Linux device paths like /dev/sda.
func isDevicePath(path string) bool {
	return strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `//./`) || strings.HasPrefix(path, "/dev/")
}

// NewWriter opens or creates a file for writing at arbitrary offsets.
// For device paths (e.g., \\.\PhysicalDrive0), platform-specific opening is used.
func NewWriter(path string) (*Writer, error) {
	var f *os.File
	var err error

	if isDevicePath(path) {
		f, err = openDevice(path)
	} else {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s for writing: %w", path, err)
	}
	return &Writer{file: f, path: path}, nil
}

// WriteAt writes data at the given byte offset.
// For device paths on Windows, writes are sector-aligned automatically.
func (w *Writer) WriteAt(data []byte, offset int64) (int, error) {
	if isDevicePath(w.path) {
		return deviceWriteAt(w.file, data, offset)
	}
	return w.file.WriteAt(data, offset)
}

// Truncate sets the file to the given size.
// For device paths, truncate is a no-op since devices have a fixed size.
func (w *Writer) Truncate(size int64) error {
	if isDevicePath(w.path) {
		return nil
	}
	return w.file.Truncate(size)
}

// Sync flushes writes to stable storage.
func (w *Writer) Sync() error {
	return w.file.Sync()
}

// Close closes the writer.
func (w *Writer) Close() error {
	return w.file.Close()
}
