// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"fmt"
	"io"
	"os"
	"sort"
	"unsafe"
)

const readSectorSize = 512

// Reader provides sequential reading of a volume or file for the backup pipeline.
type Reader struct {
	file       *os.File
	size       int64
	offset     int64
	bufferSize int

	// Direct I/O fields (used for device paths on supported platforms)
	directIO bool
	alignBuf []byte // oversized allocation for alignment
	alignOff int    // start offset within alignBuf for sector alignment
	bufPos   int    // current read position within the aligned region
	bufEnd   int    // valid bytes in the aligned region
	eof      bool   // underlying file has reported io.EOF
}

// NewReader opens a volume device path or file for reading.
// bufferSize controls the read chunk size (default 1 MB).
func NewReader(path string, bufferSize int) (*Reader, error) {
	if bufferSize <= 0 {
		bufferSize = 1024 * 1024 // 1 MB default
	}

	r := &Reader{
		bufferSize: bufferSize,
	}

	if isDevicePath(path) {
		f, err := openDeviceRead(path)
		if err != nil {
			return nil, fmt.Errorf("opening device %s: %w", path, err)
		}
		r.file = f
		r.directIO = true
		r.initDirectIO()
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		r.file = f
	}

	info, err := r.file.Stat()
	if err == nil {
		r.size = info.Size()
	}

	// Raw devices return 0 from stat on Windows. Query the kernel directly.
	if r.size == 0 && r.directIO {
		if sz, err := deviceSize(r.file); err == nil && sz > 0 {
			r.size = sz
		}
	}

	return r, nil
}

// initDirectIO prepares the sector-aligned refill buffer for direct-I/O reads.
// The buffer size is rounded UP to a sector multiple and — critically —
// r.bufferSize is updated to the rounded value: refills issue reads of
// r.bufferSize bytes, and O_DIRECT devices fail every read whose length is not
// a sector multiple (EINVAL). The user-supplied read_buffer_size is therefore
// a minimum, not an exact length.
func (r *Reader) initDirectIO() {
	alignedSize := ((r.bufferSize + readSectorSize - 1) / readSectorSize) * readSectorSize
	r.bufferSize = alignedSize

	// Allocate oversized buffer and find sector-aligned start.
	raw := make([]byte, alignedSize+readSectorSize)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	r.alignBuf = raw
	r.alignOff = int((readSectorSize - addr%readSectorSize) % readSectorSize)
}

// SetSize explicitly sets the volume size (needed for raw device reads
// where stat returns 0).
func (r *Reader) SetSize(size int64) {
	r.size = size
}

// Size returns the total size in bytes.
func (r *Reader) Size() int64 {
	return r.size
}

// Read implements io.Reader.
func (r *Reader) Read(p []byte) (int, error) {
	if r.directIO {
		return r.alignedRead(p)
	}
	n, err := r.file.Read(p)
	r.offset += int64(n)
	return n, err
}

// alignedRead serves reads from a sector-aligned buffer, refilling from the
// underlying file with sector-aligned reads when the buffer is empty.
func (r *Reader) alignedRead(p []byte) (int, error) {
	if r.bufPos >= r.bufEnd {
		if r.eof {
			return 0, io.EOF
		}
		// Refill buffer.
		buf := r.alignBuf[r.alignOff : r.alignOff+r.bufferSize]
		n, err := r.file.Read(buf)
		// Track the real EOF from the underlying file. A short read (n <
		// bufferSize) with a nil error is NOT EOF — it is legal on md/loop/
		// network devices — so we must not infer end-of-stream from the read
		// length, only from an actual io.EOF. Inferring EOF from a short read
		// silently truncated the volume backup.
		if err == io.EOF {
			r.eof = true
		} else if err != nil {
			return 0, err
		}
		if n == 0 {
			// No data and no error other than a possible EOF: end of stream.
			return 0, io.EOF
		}
		r.bufPos = 0
		r.bufEnd = n
	}

	// Serve from buffer.
	available := r.bufEnd - r.bufPos
	n := len(p)
	if n > available {
		n = available
	}
	copy(p[:n], r.alignBuf[r.alignOff+r.bufPos:r.alignOff+r.bufPos+n])
	r.bufPos += n
	r.offset += int64(n)

	// EOF is reported only once the buffer is drained AND the underlying file
	// has actually signaled io.EOF — never merely because a refill was short.
	if r.bufPos >= r.bufEnd && r.eof {
		return n, io.EOF
	}
	return n, nil
}

// Offset returns the current read position.
func (r *Reader) Offset() int64 {
	return r.offset
}

// SeekTo repositions the reader so the next Read returns the byte at off. It is
// used to resume an interrupted backup from a checkpoint offset (#42). For a
// buffered file it is a plain seek. For a direct-I/O device it seeks to the
// sector floor (O_DIRECT requires sector-aligned reads) and discards the
// sub-sector remainder, so the next Read still delivers byte off exactly.
func (r *Reader) SeekTo(off int64) error {
	if off < 0 {
		return fmt.Errorf("SeekTo: negative offset %d", off)
	}
	if !r.directIO {
		if _, err := r.file.Seek(off, io.SeekStart); err != nil {
			return fmt.Errorf("SeekTo %d: %w", off, err)
		}
		r.offset = off
		return nil
	}

	floor := off - off%readSectorSize
	if _, err := r.file.Seek(floor, io.SeekStart); err != nil {
		return fmt.Errorf("SeekTo %d (sector floor %d): %w", off, floor, err)
	}
	r.bufPos = 0
	r.bufEnd = 0
	r.eof = false
	r.offset = off

	rem := int(off - floor)
	if rem == 0 {
		return nil // sector-aligned: next Read refills from exactly off
	}
	// Refill one aligned block and drop the leading `rem` bytes so the next
	// served byte is off.
	buf := r.alignBuf[r.alignOff : r.alignOff+r.bufferSize]
	n, err := r.file.Read(buf)
	if err == io.EOF {
		r.eof = true
	} else if err != nil {
		return fmt.Errorf("SeekTo %d refill: %w", off, err)
	}
	if n <= rem {
		// off is at or past EOF within this sector; next Read reports EOF.
		r.bufPos, r.bufEnd = n, n
	} else {
		r.bufPos, r.bufEnd = rem, n
	}
	return nil
}

// Close closes the underlying file.
func (r *Reader) Close() error {
	return r.file.Close()
}

// ExclusionMap marks byte ranges to skip (write as zeros).
// Used for volatile files like pagefile.sys.
type ExclusionMap struct {
	ranges []excludedRange
	sorted bool
}

type excludedRange struct {
	start int64
	end   int64 // exclusive
}

// NewExclusionMap creates an empty exclusion map.
func NewExclusionMap() *ExclusionMap {
	return &ExclusionMap{}
}

// AddRange marks a byte range as excluded.
func (m *ExclusionMap) AddRange(start, length int64) {
	m.ranges = append(m.ranges, excludedRange{start: start, end: start + length})
	m.sorted = false
}

// Len returns the number of excluded ranges.
func (m *ExclusionMap) Len() int {
	return len(m.ranges)
}

// CoveredBytes is the number of distinct bytes the map excludes — overlaps
// between passes (volatile, subtree, live-extent) counted once. It is what an
// operator-facing "excluding X (N MB)" line reports (#468).
func (m *ExclusionMap) CoveredBytes() int64 {
	m.ensureSorted()
	var n int64
	for _, r := range m.ranges {
		n += r.end - r.start
	}
	return n
}

// ensureSorted sorts ranges by start offset and coalesces overlapping/adjacent
// ones on first access. Merging keeps range ends monotonic, which the binary
// search in IsExcluded depends on (the volatile, subtree and live-FSCTL
// passes can each add overlapping ranges for the same clusters).
func (m *ExclusionMap) ensureSorted() {
	if m.sorted {
		return
	}
	sort.Slice(m.ranges, func(i, j int) bool {
		return m.ranges[i].start < m.ranges[j].start
	})
	out := m.ranges[:0]
	for _, r := range m.ranges {
		if n := len(out); n > 0 && r.start <= out[n-1].end {
			if r.end > out[n-1].end {
				out[n-1].end = r.end
			}
			continue
		}
		out = append(out, r)
	}
	m.ranges = out
	m.sorted = true
}

// ZeroExcluded zeros the bytes of p that fall in an excluded range, where p
// represents the source bytes starting at bufStart. This is the same masking
// ExcludedReader applies to the stream, exposed so the resume boundary probe can
// reproduce the exact bytes the pipeline hashed (#54).
func (m *ExclusionMap) ZeroExcluded(p []byte, bufStart int64) {
	if m == nil || len(m.ranges) == 0 {
		return
	}
	m.ensureSorted()
	bufEnd := bufStart + int64(len(p))
	for _, ex := range m.ranges {
		if ex.start >= bufEnd {
			break
		}
		if ex.end <= bufStart {
			continue
		}
		zeroStart := max(ex.start, bufStart) - bufStart
		zeroEnd := min(ex.end, bufEnd) - bufStart
		clear(p[zeroStart:zeroEnd])
	}
}

// IsExcluded returns true if any byte in the range [offset, offset+length) is
// excluded. Binary search over the sorted ranges: catalog marking (#94) calls
// this once per extent across whole-volume catalogs, where a linear scan of a
// large repo-subtree map would be O(files × ranges).
func (m *ExclusionMap) IsExcluded(offset, length int64) bool {
	m.ensureSorted()
	end := offset + length
	// First range whose end is beyond offset; it is the only candidate that
	// could start before `end`.
	i := sort.Search(len(m.ranges), func(i int) bool { return m.ranges[i].end > offset })
	return i < len(m.ranges) && m.ranges[i].start < end
}

// ExcludedReader wraps a reader and zeros out excluded regions.
type ExcludedReader struct {
	inner      io.Reader
	exclusions *ExclusionMap
	offset     int64
}

// NewExcludedReader wraps a reader with an exclusion map.
func NewExcludedReader(r io.Reader, exclusions *ExclusionMap) *ExcludedReader {
	return &ExcludedReader{
		inner:      r,
		exclusions: exclusions,
	}
}

// NewExcludedReaderAt wraps a reader whose next byte is at startOffset on the
// source volume. Used to resume a volatile-exclusion backup (#54): the inner
// reader has been seeked to the resume offset, so the exclusion bookkeeping must
// start there rather than at 0.
func NewExcludedReaderAt(r io.Reader, exclusions *ExclusionMap, startOffset int64) *ExcludedReader {
	return &ExcludedReader{
		inner:      r,
		exclusions: exclusions,
		offset:     startOffset,
	}
}

// Read reads from the inner reader, zeroing excluded regions.
func (r *ExcludedReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 && r.exclusions != nil {
		r.exclusions.ZeroExcluded(p[:n], r.offset)
	}
	r.offset += int64(n)
	return n, err
}
