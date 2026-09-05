//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"io"
	"os"
	"runtime"
	"syscall"
)

// fileReadAt reads len(buf) bytes from f at the given byte offset.
//
// Go's os.File.ReadAt delegates to internal/poll.FD.Pread, which calls
// SetFilePointerEx to save/restore the file position before using ReadFile
// with an OVERLAPPED structure.  On some Windows configurations (observed
// on GitHub Actions windows-latest) SetFilePointerEx returns
// ERROR_INVALID_HANDLE for file handles that have been idle (no I/O since
// open).  This wrapper calls ReadFile with OVERLAPPED directly, avoiding
// SetFilePointerEx entirely.
func fileReadAt(f *os.File, buf []byte, off int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	handle := syscall.Handle(f.Fd())
	total := 0
	for len(buf) > 0 {
		var o syscall.Overlapped
		o.Offset = uint32(off)
		o.OffsetHigh = uint32(off >> 32)
		var done uint32
		err := syscall.ReadFile(handle, buf, &done, &o)
		if err != nil {
			if err == syscall.ERROR_HANDLE_EOF {
				err = io.EOF
			}
			runtime.KeepAlive(f)
			return total + int(done), &os.PathError{Op: "read", Path: f.Name(), Err: err}
		}
		if done == 0 {
			runtime.KeepAlive(f)
			return total, &os.PathError{Op: "read", Path: f.Name(), Err: io.EOF}
		}
		total += int(done)
		buf = buf[done:]
		off += int64(done)
	}
	runtime.KeepAlive(f)
	return total, nil
}

// fileWriteAt writes buf to f at the given byte offset.
// See fileReadAt for the rationale.
func fileWriteAt(f *os.File, buf []byte, off int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	handle := syscall.Handle(f.Fd())
	total := 0
	for len(buf) > 0 {
		var o syscall.Overlapped
		o.Offset = uint32(off)
		o.OffsetHigh = uint32(off >> 32)
		var done uint32
		err := syscall.WriteFile(handle, buf, &done, &o)
		if err != nil {
			runtime.KeepAlive(f)
			return total + int(done), &os.PathError{Op: "write", Path: f.Name(), Err: err}
		}
		total += int(done)
		buf = buf[done:]
		off += int64(done)
	}
	runtime.KeepAlive(f)
	return total, nil
}
