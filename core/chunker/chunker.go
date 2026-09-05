// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker

import (
	"io"
)

// Fallback geometry: what a Chunker built with NO options uses.
//
// This is emphatically NOT the product's chunk geometry, and it used to say it
// was (#356 item 10). disknexus chunks at whatever the REPO's stored config
// says — 16/64/512 KB with mask 0xFFFF since v0.7.5's #83 field decision, the
// "fine-grained" profile below that — and pipeline passes it explicitly on
// every backup via WithMinSize/WithMaxSize/WithMask. No production path ever
// reaches these values.
//
// They kept the name Default for several releases after the geometry moved,
// so the constant that answers "what does disknexus chunk at?" answered four
// times too small. #354 was one package building config.Default() instead of
// reading the repo's stored geometry, and every backup it wrote deduped
// against nothing; a mislabelled constant here is the same trap with a
// shorter fuse. Renamed rather than realigned, because aligning would write
// the geometry down a SECOND time in a package that must not own it.
//
// The fourth constant — an 8 KB average — is deleted outright rather than
// renamed: nothing in the tree ever read it, and this chunker's average is a
// consequence of the mask, so a constant asserting one was a lie by existence.
const (
	FallbackMinSize = 4 * 1024  // 4 KB
	FallbackMaxSize = 64 * 1024 // 64 KB
	FallbackMask    = 0x1FFF    // 13 bits
	WindowSize      = 48        // Buzhash sliding window
)

// Chunk represents a variable-length chunk produced by the CDC chunker.
type Chunk struct {
	Data   []byte // raw bytes (original, not normalized)
	Offset int64  // byte offset on source volume
	Length int    // len(Data)
}

// Chunker implements content-defined chunking using Buzhash rolling hash
// with FastCDC-style normalized chunking for good shift-resync behavior.
//
// Instead of a hard minimum size (which creates a "dead zone" that cascades
// after insertions), we use two masks:
//   - Below minSize: use a harder mask (maskHard = mask << 2) — boundaries are
//     unlikely but not impossible, preserving shift-resync.
//   - Above minSize: use an easier mask (maskEasy = mask >> 2) — boundaries are
//     more likely, biasing toward the target average size.
//
// This gives a smooth chunk size distribution centered on the target average
// while avoiding the min-size cascade problem of traditional CDC.
//
// NOT safe for concurrent use — chunking must be single-threaded
// to guarantee deterministic boundaries.
type Chunker struct {
	reader io.Reader
	buf    []byte // accumulates current chunk data
	offset int64  // current position in the stream

	// Buzhash state (continuous across chunk boundaries)
	window    [WindowSize]byte
	windowPos int
	hash      uint64

	// Parameters
	minSize  int
	maxSize  int
	mask     uint64
	maskHard uint64 // mask << 2: used below minSize (hard to match)
	maskEasy uint64 // mask >> 2: used above minSize (easy to match)

	// Internal buffered read state
	readBuf []byte
	readPos int
	readEnd int
	eof     bool
	readErr error // pending non-EOF read error, surfaced after buffered bytes are consumed
}

// Option configures a Chunker.
type Option func(*Chunker)

// WithMinSize sets the minimum chunk size.
func WithMinSize(n int) Option {
	return func(c *Chunker) { c.minSize = n }
}

// WithMaxSize sets the maximum chunk size.
func WithMaxSize(n int) Option {
	return func(c *Chunker) { c.maxSize = n }
}

// WithMask sets the Buzhash boundary mask.
func WithMask(m uint64) Option {
	return func(c *Chunker) { c.mask = m }
}

// zeroWindowHash is the correct Buzhash value for an all-zero window.
// The incremental update formula requires this to be correct; initializing
// to 0 introduces a rotating error that never dissipates.
var zeroWindowHash = func() uint64 {
	var h uint64
	for i := 0; i < WindowSize; i++ {
		h ^= rotateLeft(buzhashTable[0], WindowSize-1-i)
	}
	return h
}()

// New creates a new Chunker that reads from r.
func New(r io.Reader, opts ...Option) *Chunker {
	c := &Chunker{
		reader:  r,
		buf:     make([]byte, 0, FallbackMaxSize),
		hash:    zeroWindowHash,
		minSize: FallbackMinSize,
		maxSize: FallbackMaxSize,
		mask:    FallbackMask,
		readBuf: make([]byte, 64*1024), // 64 KB read buffer
	}
	for _, opt := range opts {
		opt(c)
	}
	// Derive the normalized masks from the base mask
	c.maskHard = c.mask<<2 | c.mask // harder to match (more bits required)
	c.maskEasy = c.mask >> 2        // easier to match (fewer bits required)
	return c
}

// Next returns the next chunk from the stream.
// Returns io.EOF when no more data is available.
//
// The rolling hash state is maintained continuously across chunk boundaries
// to preserve CDC's shift-resync property: a single byte insertion only
// affects 1-2 chunks near the insertion point.
func (c *Chunker) Next() (Chunk, error) {
	c.buf = c.buf[:0]

	chunkStart := c.offset

	for {
		b, err := c.readByte()
		if err != nil {
			if err == io.EOF && len(c.buf) > 0 {
				chunk := Chunk{
					Data:   make([]byte, len(c.buf)),
					Offset: chunkStart,
					Length: len(c.buf),
				}
				copy(chunk.Data, c.buf)
				return chunk, nil
			}
			return Chunk{}, err
		}

		c.buf = append(c.buf, b)
		n := len(c.buf)

		// Always update rolling hash to maintain continuous state
		c.updateHash(b)

		// FastCDC-style normalized chunking:
		// - Below minSize: use hard mask (very unlikely boundary)
		// - Between minSize and maxSize: use easy mask (likely boundary)
		// - At maxSize: force boundary
		if n < c.minSize {
			// Hard mask: allows tiny chunks but very rarely.
			// This preserves shift-resync across the min-size zone.
			if (c.hash & c.maskHard) == 0 {
				return c.emitChunk(chunkStart), nil
			}
		} else if n < c.maxSize {
			// Easy mask: biases toward target average
			if (c.hash & c.maskEasy) == 0 {
				return c.emitChunk(chunkStart), nil
			}
		} else {
			// Force boundary at max size
			return c.emitChunk(chunkStart), nil
		}
	}
}

// updateHash performs a Buzhash rolling hash update.
func (c *Chunker) updateHash(b byte) {
	// The byte leaving the window
	outByte := c.window[c.windowPos]

	// Place new byte in window
	c.window[c.windowPos] = b
	c.windowPos = (c.windowPos + 1) % WindowSize

	// Buzhash: hash = rotl(hash, 1) ^ rotl(table[outByte], windowSize) ^ table[inByte]
	c.hash = rotateLeft(c.hash, 1) ^
		rotateLeft(buzhashTable[outByte], WindowSize) ^
		buzhashTable[b]
}

func rotateLeft(val uint64, n int) uint64 {
	s := uint(n) & 63
	return (val << s) | (val >> (64 - s))
}

func (c *Chunker) emitChunk(startOffset int64) Chunk {
	chunk := Chunk{
		Data:   make([]byte, len(c.buf)),
		Offset: startOffset,
		Length: len(c.buf),
	}
	copy(chunk.Data, c.buf)
	return chunk
}

// maxConsecutiveEmptyReads bounds retries of (0, nil) reads, mirroring bufio.
const maxConsecutiveEmptyReads = 100

// readByte returns the next byte from the buffered reader.
//
// Per the io.Reader contract, a Read may return data together with a non-EOF
// error, and is not obligated to return that error again on the next call.
// Buffered bytes are always served first; the latched error is surfaced once
// the buffer is drained. A (0, nil) return means "nothing happened" and is
// retried, never treated as data or EOF.
func (c *Chunker) readByte() (byte, error) {
	for empty := 0; c.readPos >= c.readEnd; {
		if c.eof {
			return 0, io.EOF
		}
		if c.readErr != nil {
			err := c.readErr
			c.readErr = nil
			c.eof = true // no further reads after a terminal error
			return 0, err
		}
		n, err := c.reader.Read(c.readBuf)
		if n > 0 {
			c.readPos = 0
			c.readEnd = n
		}
		if err != nil {
			if err == io.EOF {
				c.eof = true
			} else {
				c.readErr = err
			}
		} else if n == 0 {
			empty++
			if empty >= maxConsecutiveEmptyReads {
				return 0, io.ErrNoProgress
			}
		}
	}

	b := c.readBuf[c.readPos]
	c.readPos++
	c.offset++
	return b, nil
}

// Reset resets the chunker to read from a new reader, starting at the given offset.
func (c *Chunker) Reset(r io.Reader, offset int64) {
	c.reader = r
	c.offset = offset
	c.buf = c.buf[:0]
	c.hash = zeroWindowHash
	c.windowPos = 0
	c.readPos = 0
	c.readEnd = 0
	c.eof = false
	c.readErr = nil
	c.window = [WindowSize]byte{}
}
