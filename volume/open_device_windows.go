//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

const sectorSize = 512

// openDevice opens a Windows device path (e.g., \\.\PhysicalDrive0) for writing
// using CreateFileW with the correct flags for raw device access.
func openDevice(path string) (*os.File, error) {
	pathp, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, err
	}

	// Lock the volume for exclusive write access by issuing FSCTL_LOCK_VOLUME.
	// This dismounts the filesystem and prevents other processes from interfering.
	const FSCTL_LOCK_VOLUME = 0x00090018
	var bytesReturned uint32
	_ = syscall.DeviceIoControl(h, FSCTL_LOCK_VOLUME, nil, 0, nil, 0, &bytesReturned, nil)

	return os.NewFile(uintptr(h), path), nil
}

// alignedWriteAt performs a sector-aligned write to a raw device.
// Raw devices on Windows require both offset and size to be multiples of the
// sector size (typically 512 bytes). For unaligned writes, this does a
// read-modify-write of the boundary sectors.
func alignedWriteAt(f *os.File, data []byte, offset int64) (int, error) {
	alignedStart := (offset / sectorSize) * sectorSize
	endByte := offset + int64(len(data))
	alignedEnd := ((endByte + sectorSize - 1) / sectorSize) * sectorSize

	// Fast path: already aligned
	if alignedStart == offset && alignedEnd == endByte {
		return f.WriteAt(data, offset)
	}

	// Slow path: read-modify-write for unaligned boundaries
	buf := make([]byte, alignedEnd-alignedStart)

	// Read existing data at aligned boundaries. This read MUST succeed (short
	// reads at device end excepted): if it fails, buf stays zeroed and the
	// write below would destroy the neighboring bytes that share the boundary
	// sectors. io.EOF/ErrUnexpectedEOF are tolerated — the tail beyond device
	// end reads as zero, and a write there fails on its own.
	if _, err := f.ReadAt(buf, alignedStart); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("read-modify-write pre-read at offset %d: %w (aborting to avoid corrupting boundary sectors)", alignedStart, err)
	}

	// Overlay our data
	copy(buf[offset-alignedStart:], data)

	// Write the aligned buffer
	_, err := f.WriteAt(buf, alignedStart)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// deviceWriteAt writes data with sector alignment for raw device access.
func deviceWriteAt(f *os.File, data []byte, offset int64) (int, error) {
	return alignedWriteAt(f, data, offset)
}

// deviceSize returns the size of a raw device backed by an *os.File. Safe only
// when the caller owns f and closes it exactly once (e.g. volume.Reader). For a
// bare handle that the caller closes itself, use deviceSizeHandle — wrapping
// such a handle in an *os.File double-closes it (see issue #28).
func deviceSize(f *os.File) (int64, error) {
	return deviceSizeHandle(syscall.Handle(f.Fd()))
}

// DeviceSize is the exported form for callers outside this package that
// hold an *os.File on a raw device (cmd/disknexus --disk sizing, #83).
func DeviceSize(f *os.File) (int64, error) {
	return deviceSize(f)
}

// deviceSizeHandle returns the size of a raw device using
// IOCTL_DISK_GET_LENGTH_INFO, operating directly on a handle. It deliberately
// does NOT wrap the handle in an *os.File: os.NewFile installs a finalizer that
// CloseHandle()s the handle, which double-closes any handle the caller also
// closes and can corrupt the process handle table (see issue #28).
func deviceSizeHandle(h syscall.Handle) (int64, error) {
	const IOCTL_DISK_GET_LENGTH_INFO = 0x0007405C
	var lengthInfo struct {
		Length int64
	}
	var bytesReturned uint32
	err := syscall.DeviceIoControl(
		h,
		IOCTL_DISK_GET_LENGTH_INFO,
		nil, 0,
		(*byte)(unsafe.Pointer(&lengthInfo)), uint32(unsafe.Sizeof(lengthInfo)),
		&bytesReturned, nil,
	)
	if err != nil {
		return 0, err
	}
	return lengthInfo.Length, nil
}

// VolumeSize returns the total byte size of a volume by opening the volume
// device (e.g. "C:") and issuing IOCTL_DISK_GET_LENGTH_INFO. This succeeds
// for real volumes even when the same IOCTL fails on a VSS shadow-copy path.
func VolumeSize(volumeLetter string) (int64, error) {
	// Normalize: "C" or "C:" or "C:\" → \\.\C:
	letter := strings.TrimSuffix(strings.TrimSuffix(volumeLetter, "\\"), ":")
	devPath := `\\.\` + letter + `:`
	pathp, err := syscall.UTF16PtrFromString(devPath)
	if err != nil {
		return 0, err
	}
	h, err := syscall.CreateFile(
		pathp,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return 0, err
	}
	// Close the handle exactly once. Do NOT wrap it in os.NewFile — that would
	// add a finalizer that closes it a second time (see issue #28).
	defer syscall.CloseHandle(h)
	return deviceSizeHandle(h)
}
