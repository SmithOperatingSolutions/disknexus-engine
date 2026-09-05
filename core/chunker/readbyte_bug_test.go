// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/chunker"
)

// zeroReadThenDataReader returns (0, nil) on its first Read call, then
// delegates to the underlying reader. A (0, nil) return is explicitly
// permitted by the io.Reader contract ("Implementations of Read are
// discouraged from returning a zero byte count with a nil error, except
// when len(p) == 0... Callers should treat a return of 0 and nil as
// indicating that nothing happened").
type zeroReadThenDataReader struct {
	r        io.Reader
	returned bool
}

func (z *zeroReadThenDataReader) Read(p []byte) (int, error) {
	if !z.returned {
		z.returned = true
		return 0, nil
	}
	return z.r.Read(p)
}

// TestChunkerZeroNilReadDoesNotCorruptStream proves that a (0, nil) read
// causes readByte to serve a byte that was never read from the source:
// the refill block in readByte only updates readPos/readEnd when n > 0,
// but execution still falls through to c.readBuf[c.readPos], emitting a
// stale/zero byte and shifting every subsequent chunk offset by one.
func TestChunkerZeroNilReadDoesNotCorruptStream(t *testing.T) {
	payload := []byte("hello world, this is the payload")

	c := chunker.New(&zeroReadThenDataReader{r: bytes.NewReader(payload)})

	var got []byte
	for {
		chunk, err := c.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, chunk.Data...)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("chunked output does not match source:\n got:  %q\n want: %q", got, payload)
	}
}

// errWithDataReader returns its payload together with a non-EOF error on
// the first call, then clean EOF. The io.Reader contract allows an
// implementation to return data and an error from the same call and does
// NOT guarantee the error will be returned again by a subsequent call —
// the caller must handle it.
type errWithDataReader struct {
	payload []byte
	err     error
	done    bool
}

func (r *errWithDataReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.payload)
	return n, r.err
}

// TestChunkerSurfacesErrorReturnedWithData proves that a read error
// accompanying data is silently dropped: readByte latches only io.EOF,
// so a device error (e.g. a bad sector) reported alongside the final
// bytes never surfaces and a truncated stream is chunked as if complete.
func TestChunkerSurfacesErrorReturnedWithData(t *testing.T) {
	errBadSector := errors.New("I/O error: bad sector")

	c := chunker.New(&errWithDataReader{
		payload: []byte("data read just before the device failed"),
		err:     errBadSector,
	})

	var sawErr error
	for {
		_, err := c.Next()
		if err == nil {
			continue
		}
		if err != io.EOF {
			sawErr = err
		}
		break
	}

	if !errors.Is(sawErr, errBadSector) {
		t.Fatalf("read error was swallowed: chunker terminated with clean EOF, want %v surfaced", errBadSector)
	}
}
