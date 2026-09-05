// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// TestAlignedReadDoesNotTreatShortReadAsEOF guards issue #16: the direct-I/O
// reader inferred end-of-stream from a short read (bufEnd < bufferSize). A short
// read is legal mid-stream on md/loop/network devices, so this silently
// truncated the volume backup. EOF must come only from an actual io.EOF on the
// underlying file.
//
// A pipe is used as the underlying "device": reading it when only part of the
// data has been written yields a genuine short read that is NOT EOF.
func TestAlignedReadDoesNotTreatShortReadAsEOF(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()

	const bufSize = 4096
	r := &Reader{
		file:       pr,
		directIO:   true,
		bufferSize: bufSize,
		alignBuf:   make([]byte, bufSize),
	}
	buf := make([]byte, bufSize)

	// Put exactly 100 bytes in the pipe and leave it open: the first read sees a
	// short read that is NOT the end of the stream.
	if _, err := pw.Write(bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatalf("write 1: %v", err)
	}

	n1, err1 := r.Read(buf)
	if err1 != nil {
		t.Fatalf("first Read returned err %v; a mid-stream short read must not be reported as EOF", err1)
	}
	if n1 != 100 {
		t.Fatalf("first Read n=%d, want 100", n1)
	}

	// Now supply the rest and close (the real EOF).
	if _, err := pw.Write(bytes.Repeat([]byte{2}, 100)); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	pw.Close()

	total := n1
	for {
		n, err := r.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	if total != 200 {
		t.Fatalf("read %d bytes total, want 200 (short read was treated as EOF, truncating the stream)", total)
	}
}
