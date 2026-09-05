// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/binary"
	"fmt"
)

// dnm_stream.go — assemble a .dnm as a sequence of parts with a bounded
// buffer (low-footprint lever 4). The entries section never exists as a whole
// on the producing host: each filled window is handed to the sink (an S3 part
// upload) and dropped; the tail part carries METADATA + CATALOG + section
// index. Because the section-index offset is unknowable when the first part
// ships to an immutable store, the streamed header holds a ZERO sentinel and
// the real offset rides in an 8-byte little-endian trailer at end of file —
// OpenDNMReader understands both forms.
//
// Streamed section order is ENTRIES, METADATA, CATALOG (the classic writer
// emits METADATA, CATALOG, ENTRIES); readers locate sections purely through
// the section index, so order is a non-event for them.
type DNMStreamer struct {
	sink       func(part []byte) error
	window     int
	buf        []byte
	entriesLen uint64
	count      uint64
	finished   bool
}

// NewDNMStreamer returns a streamer that flushes to sink whenever the buffer
// reaches window bytes. For S3 multipart composition the window must be at
// least the 5 MB minimum-part size (tests use smaller windows).
func NewDNMStreamer(window int, sink func([]byte) error) *DNMStreamer {
	hdr := encodeHeader(0) // zero sentinel: index offset lives in the trailer
	return &DNMStreamer{sink: sink, window: window, buf: hdr[:]}
}

// Buffered reports the bytes currently held (bounded by window + one record).
func (d *DNMStreamer) Buffered() int { return len(d.buf) }

// WriteEntry appends one 45-byte entry record, flushing a part when the
// window fills. Mirrors EntryWriter.WriteEntry's encoding exactly.
func (d *DNMStreamer) WriteEntry(e Entry) error {
	if d.finished {
		return fmt.Errorf("streamer already finished")
	}
	var rec [EntryRecordSize]byte
	binary.LittleEndian.PutUint64(rec[0:8], uint64(e.VolumeOffset))
	copy(rec[8:40], e.ChunkHash[:])
	binary.LittleEndian.PutUint32(rec[40:44], uint32(e.ChunkLength))
	if e.IsExcluded {
		rec[44] = 1
	}
	d.buf = append(d.buf, rec[:]...)
	d.entriesLen += EntryRecordSize
	d.count++
	if len(d.buf) >= d.window {
		if err := d.sink(d.buf); err != nil {
			return err
		}
		d.buf = d.buf[:0]
	}
	return nil
}

// Finish emits the final part: any remaining entry bytes, then METADATA,
// CATALOG (in-memory catalog only), the section index, and the offset
// trailer. b's catalog-sidecar path must be empty — streamed runs are
// restricted to captures that keep their catalog in memory.
func (d *DNMStreamer) Finish(b *Backup) error {
	if d.finished {
		return fmt.Errorf("streamer already finished")
	}
	if b.CatalogSidecarPath != "" {
		return fmt.Errorf("streamed manifests do not support catalog sidecars")
	}
	d.finished = true

	entriesOffset := uint64(fileHeaderSize)
	pos := entriesOffset + d.entriesLen

	metaOffset := pos
	metaBytes := encodeMetadata(b)
	d.buf = append(d.buf, metaBytes...)
	pos += uint64(len(metaBytes))

	catalogOffset := pos
	for _, fe := range b.FileCatalog {
		rec := encodeFileEntry(fe)
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(rec)))
		d.buf = append(d.buf, lenBuf[:]...)
		d.buf = append(d.buf, rec...)
		pos += 4 + uint64(len(rec))
	}
	catalogLen := pos - catalogOffset

	sectionIndexOffset := pos
	for _, ie := range [numSections][sectionIndexSize]byte{
		encodeSectionIndex(sectionMetadata, metaOffset, uint64(len(metaBytes)), 1),
		encodeSectionIndex(sectionCatalog, catalogOffset, catalogLen, uint64(len(b.FileCatalog))),
		encodeSectionIndex(sectionEntries, entriesOffset, d.entriesLen, d.count),
	} {
		d.buf = append(d.buf, ie[:]...)
	}

	var trailer [8]byte
	binary.LittleEndian.PutUint64(trailer[:], sectionIndexOffset)
	d.buf = append(d.buf, trailer[:]...)

	err := d.sink(d.buf)
	d.buf = nil
	return err
}
