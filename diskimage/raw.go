// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"fmt"
	"os"
)

// rawWriter is a sparse raw image: the file is sized to the disk and only
// non-zero runs are written, so every unwritten region reads as zeros
// through a hole the filesystem does not store.
type rawWriter struct {
	f    *os.File
	size int64
}

func newRaw(path string, size int64) (*rawWriter, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("sizing %s: %w", path, err)
	}
	return &rawWriter{f: f, size: size}, nil
}

func (w *rawWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > w.size {
		return 0, fmt.Errorf("write at %d+%d is outside the %d-byte image", off, len(p), w.size)
	}
	err := forEachDataRun(off, p, func(rel int, chunk []byte) error {
		_, werr := w.f.WriteAt(chunk, off+int64(rel))
		return werr
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// ReadAt reads the image back; holes read as zeros (bounded by the size,
// so a fixed VHD's footer is never part of the disk).
func (w *rawWriter) ReadAt(p []byte, off int64) (int, error) {
	return readMapped(w.f, w.size, 1<<20, p, off, func(unit int64) int64 { return unit << 20 })
}

// Truncate is accepted for the restore engine's sake; the image keeps the
// size it was created with (a restore never grows a disk it is exporting).
func (w *rawWriter) Truncate(size int64) error {
	if size > w.size {
		return fmt.Errorf("cannot grow a %d-byte image to %d", w.size, size)
	}
	return nil
}

func (w *rawWriter) Sync() error     { return w.f.Sync() }
func (w *rawWriter) Files() []string { return []string{w.f.Name()} }
func (w *rawWriter) Close() error    { return w.f.Close() }
