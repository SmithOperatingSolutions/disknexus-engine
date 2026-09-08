// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package diskimage writes disk images in the formats a backup can be
// exported to (#456): sparse raw, VHD (dynamic and fixed), qcow2 and
// VMDK (monolithicFlat). Every writer takes the same shape the restore
// engine writes into — WriteAt at disk offsets, Truncate to the virtual
// size, Sync, Close — and keeps the image small by allocating only what
// is written: a 4 KiB run of zeros is never stored, so a 2 TB disk with
// 100 GB used costs about 100 GB in every format.
package diskimage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Format is an image file format.
type Format string

const (
	Raw   Format = "raw"
	VHD   Format = "vhd"
	QCOW2 Format = "qcow2"
	VMDK  Format = "vmdk"
)

// Formats lists what Create accepts, in the order the panel offers them.
var Formats = []Format{Raw, VHD, QCOW2, VMDK}

// ParseFormat accepts the format names and their common file extensions.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "raw", "img", "bin", "":
		return Raw, nil
	case "vhd":
		return VHD, nil
	case "qcow2", "qcow":
		return QCOW2, nil
	case "vmdk":
		return VMDK, nil
	}
	return "", fmt.Errorf("unknown image format %q (raw, vhd, qcow2, vmdk)", s)
}

// Extension is the file extension a format's image carries.
func (f Format) Extension() string {
	switch f {
	case Raw:
		return ".img"
	default:
		return "." + string(f)
	}
}

// Options tune a writer.
type Options struct {
	// Fixed writes a VHD as a fixed disk (raw plus footer) instead of a
	// dynamic one. Ignored by the other formats.
	Fixed bool
}

// Writer is what the restore engine writes an image through. ReadAt reads
// the virtual disk back through the writer's own tables, so a restore's
// read-back verification works before the image is finished. Close
// finishes the format (tables, footers) and must be called exactly once;
// Files names every file the image consists of (a VMDK is two).
type Writer interface {
	io.WriterAt
	io.ReaderAt
	Truncate(size int64) error
	Sync() error
	Close() error
	Files() []string
}

// blockSize is the granularity zero runs are elided at: one filesystem
// page on every platform that can hold a hole.
const blockSize = 4096

// Create opens a new image of the format at path with the given virtual
// size. The file must not exist; a VMDK's extent is created beside it.
func Create(format Format, path string, size int64, opts Options) (Writer, error) {
	if size <= 0 {
		return nil, errors.New("image size must be positive")
	}
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("%s exists: an export is a new file, never an overwrite", path)
	}
	switch format {
	case Raw:
		return newRaw(path, size)
	case VHD:
		if opts.Fixed {
			return newFixedVHD(path, size)
		}
		return newDynamicVHD(path, size)
	case QCOW2:
		return newQCOW2(path, size)
	case VMDK:
		return newVMDK(path, size)
	}
	return nil, fmt.Errorf("unknown image format %q", format)
}

// readMapped serves ReadAt for a format that maps fixed-size virtual
// units to file offsets: locate returns the file offset of the unit
// holding off, or -1 when the unit is unallocated and reads as zeros.
func readMapped(f *os.File, size, unit int64, p []byte, off int64, locate func(unitIndex int64) int64) (int, error) {
	if off < 0 || off > size {
		return 0, fmt.Errorf("read at %d is outside the %d-byte image", off, size)
	}
	if off+int64(len(p)) > size {
		p = p[:size-off]
	}
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		in := cur % unit
		n := int64(len(p) - done)
		if in+n > unit {
			n = unit - in
		}
		chunk := p[done : done+int(n)]
		if fo := locate(cur / unit); fo < 0 {
			for i := range chunk {
				chunk[i] = 0
			}
		} else if _, err := f.ReadAt(chunk, fo+in); err != nil && err != io.EOF {
			return done, err
		}
		done += int(n)
	}
	if int64(done) < int64(len(p)) {
		return done, io.EOF
	}
	return done, nil
}

// allZero reports whether p is entirely zero bytes.
func allZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// forEachDataRun calls fn for every maximal run of blocks in p that holds
// a non-zero byte, with the run's offset relative to p. Runs are aligned
// to blockSize from base (the absolute offset of p) so holes line up with
// filesystem pages wherever they fall in the image.
func forEachDataRun(base int64, p []byte, fn func(rel int, chunk []byte) error) error {
	n := len(p)
	i := 0
	for i < n {
		// The first block may start mid-page: cut at the next page edge.
		end := i + blockSize - int((base+int64(i))%blockSize)
		if end > n {
			end = n
		}
		if allZero(p[i:end]) {
			i = end
			continue
		}
		start := i
		i = end
		for i < n {
			e := i + blockSize
			if e > n {
				e = n
			}
			if allZero(p[i:e]) {
				break
			}
			i = e
		}
		if err := fn(start, p[start:i]); err != nil {
			return err
		}
	}
	return nil
}
