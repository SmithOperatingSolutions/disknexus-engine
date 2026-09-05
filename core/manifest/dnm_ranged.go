// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// dnm_ranged.go — catalog reading over a range-read primitive (#92 browse,
// prod fix 2026-08-19; #419 prod fix 2026-08-25). A physical-partition
// backup's .dnm is dominated by its ENTRIES section (one record per chunk);
// the panel's file browser only needs the CATALOG. Downloading the whole
// object to read a sliver of it pushed real-world browses past ingress
// timeouts (#92).
//
// #419 is the same defect one layer in: fetching only the catalog is not
// enough, because a whole-volume CATALOG SECTION is itself tens of megabytes,
// and reading it into one buffer and decoding it into one []FileEntry put
// both live at once — ~180 MB for a 63.8 GB NTFS member, against a 256Mi
// controller. So the section is read through a bounded WINDOW and decoded one
// record at a time; nothing here holds memory proportional to the number of
// files on the volume.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// RangeReadFunc returns n bytes at offset off of the manifest object.
type RangeReadFunc func(off, n int64) ([]byte, error)

// catalogRangeWindow is the most of the CATALOG section held at once. It is
// the whole memory bound of a ranged catalog read: one window, plus the
// decode buffer, plus the single record being decoded. Bigger means fewer
// round trips to object storage and a higher floor; 1 MiB is ~40 requests for
// the largest catalog seen in the field (147 MB .dnm, 580k files).
const catalogRangeWindow = 1 << 20

// catalogDecodeBuffer sits between the window and the record decoder so a
// record straddling a window boundary is reassembled without a second read.
const catalogDecodeBuffer = 64 << 10

// StreamCatalogRanged reads a manifest's file catalog through ranged reads
// only — the ENTRIES bulk is never transferred — and calls visit once per
// record, in stored order. Neither the catalog section nor a []FileEntry is
// ever materialized. visit returns false to stop early.
//
// Handles both the file layout and the streamed layout (zero header sentinel
// + 8-byte offset trailer, the norm for cloud-uploaded manifests).
func StreamCatalogRanged(size int64, readRange RangeReadFunc, visit func(FileEntry) bool) error {
	catalog, err := locateCatalogRanged(size, readRange)
	if err != nil {
		return err
	}
	if catalog.count == 0 {
		return nil
	}
	cr := &catalogReader{
		br: bufio.NewReaderSize(&rangeSectionReader{
			readRange: readRange,
			off:       int64(catalog.offset),
			end:       int64(catalog.offset) + int64(catalog.length),
			window:    catalogRangeWindow,
		}, catalogDecodeBuffer),
		remaining: catalog.count,
	}
	for i := uint64(0); i < catalog.count; i++ {
		fe, err := cr.next()
		if err != nil {
			return fmt.Errorf("decoding catalog record %d of %d: %w", i, catalog.count, err)
		}
		if !visit(fe) {
			return nil
		}
	}
	return nil
}

// locateCatalogRanged reads the header, the streamed-layout trailer and the
// section index — 148 bytes in three reads — and returns where the CATALOG
// section lives. No section data is touched.
func locateCatalogRanged(size int64, readRange RangeReadFunc) (sectionInfo, error) {
	var none sectionInfo
	if size < fileHeaderSize {
		return none, fmt.Errorf("manifest too small (%d bytes)", size)
	}
	hdr, err := readRange(0, fileHeaderSize)
	if err != nil {
		return none, fmt.Errorf("reading dnm header: %w", err)
	}
	if len(hdr) < fileHeaderSize || string(hdr[0:8]) != dnmMagic {
		return none, fmt.Errorf("invalid dnm magic bytes")
	}
	if v := binary.LittleEndian.Uint16(hdr[8:10]); v != dnmVersion {
		return none, fmt.Errorf("unsupported dnm version %d", v)
	}
	sectionIndexOffset := binary.LittleEndian.Uint64(hdr[12:20])
	sectionIndexCount := binary.LittleEndian.Uint32(hdr[20:24])

	if sectionIndexOffset == 0 {
		// Streamed layout: the real index offset is the trailing 8 bytes.
		tr, err := readRange(size-8, 8)
		if err != nil {
			return none, fmt.Errorf("reading streamed dnm trailer: %w", err)
		}
		sectionIndexOffset = binary.LittleEndian.Uint64(tr)
		if want := uint64(size) - 8 - uint64(sectionIndexCount)*sectionIndexSize; sectionIndexOffset != want {
			return none, fmt.Errorf("streamed dnm trailer offset %d inconsistent with size %d", sectionIndexOffset, size)
		}
	}
	idxLen := int64(sectionIndexCount) * sectionIndexSize
	if int64(sectionIndexOffset)+idxLen > size {
		return none, fmt.Errorf("section index out of bounds")
	}
	idxData, err := readRange(int64(sectionIndexOffset), idxLen)
	if err != nil {
		return none, fmt.Errorf("reading section index: %w", err)
	}
	var catalog sectionInfo
	for i := uint32(0); i < sectionIndexCount; i++ {
		var buf [sectionIndexSize]byte
		copy(buf[:], idxData[int64(i)*sectionIndexSize:])
		if s := decodeSectionIndex(buf); s.typ == sectionCatalog {
			catalog = s
		}
	}
	if catalog.count > 0 && int64(catalog.offset)+int64(catalog.length) > size {
		return none, fmt.Errorf("catalog section out of bounds")
	}
	return catalog, nil
}

// rangeSectionReader presents one section of a ranged object as an io.Reader,
// holding at most one window of it. This is what keeps a catalog read's
// memory independent of the catalog's size.
type rangeSectionReader struct {
	readRange RangeReadFunc
	off, end  int64
	window    int64
	buf       []byte
}

func (r *rangeSectionReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.off >= r.end {
			return 0, io.EOF
		}
		n := min(r.window, r.end-r.off)
		b, err := r.readRange(r.off, n)
		if err != nil {
			return 0, fmt.Errorf("reading catalog bytes [%d,%d): %w", r.off, r.off+n, err)
		}
		if len(b) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		if int64(len(b)) > n {
			b = b[:n] // a backend that over-serves must not push us past the section
		}
		r.off += int64(len(b))
		r.buf = b
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	if len(r.buf) == 0 {
		r.buf = nil // drop the window as soon as it is spent
	}
	return n, nil
}
