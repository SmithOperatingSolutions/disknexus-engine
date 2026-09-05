// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// dnm_reader.go — reads a .dnm manifest file with section-level seeking.
//
// Opening a DNMReader reads only the 32-byte file header and the section index
// (3 × 36 = 108 bytes). No section data is read until the caller explicitly
// requests it, enabling metadata-only loads (e.g., for the `list` command).

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// DNMReader reads a .dnm manifest file. The caller is responsible for calling
// Close() when done.
type DNMReader struct {
	f       *os.File
	meta    sectionInfo
	catalog sectionInfo
	entries sectionInfo
}

// OpenDNMReader opens a .dnm file and reads its header and section index.
// Only 32 + numSections×36 bytes are read; no section data is touched.
func OpenDNMReader(path string) (*DNMReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening dnm file: %w", err)
	}

	var hdrBuf [fileHeaderSize]byte
	if _, err := io.ReadFull(f, hdrBuf[:]); err != nil {
		f.Close()
		return nil, fmt.Errorf("reading dnm header: %w", err)
	}
	if string(hdrBuf[0:8]) != dnmMagic {
		f.Close()
		return nil, fmt.Errorf("invalid dnm magic bytes")
	}
	version := binary.LittleEndian.Uint16(hdrBuf[8:10])
	if version != dnmVersion {
		f.Close()
		return nil, fmt.Errorf("unsupported dnm version %d", version)
	}

	sectionIndexOffset := binary.LittleEndian.Uint64(hdrBuf[12:20])
	sectionIndexCount := binary.LittleEndian.Uint32(hdrBuf[20:24])

	if sectionIndexOffset == 0 {
		// Streamed .dnm (see DNMStreamer): the header offset is a zero
		// sentinel because the file was assembled on an immutable store; the
		// real section-index offset is the 8-byte LE trailer at end of file.
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("stat streamed dnm: %w", err)
		}
		var trailer [8]byte
		if _, err := f.ReadAt(trailer[:], st.Size()-8); err != nil {
			f.Close()
			return nil, fmt.Errorf("reading streamed dnm trailer: %w", err)
		}
		sectionIndexOffset = binary.LittleEndian.Uint64(trailer[:])
		// The index must sit exactly between the sections and the trailer.
		if want := uint64(st.Size()) - 8 - uint64(sectionIndexCount)*sectionIndexSize; sectionIndexOffset != want {
			f.Close()
			return nil, fmt.Errorf("streamed dnm trailer offset %d inconsistent with file size %d", sectionIndexOffset, st.Size())
		}
	}

	if _, err := f.Seek(int64(sectionIndexOffset), io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("seeking to section index: %w", err)
	}

	r := &DNMReader{f: f}
	var idxBuf [sectionIndexSize]byte
	for i := range sectionIndexCount {
		if _, err := io.ReadFull(f, idxBuf[:]); err != nil {
			f.Close()
			return nil, fmt.Errorf("reading section index entry %d: %w", i, err)
		}
		s := decodeSectionIndex(idxBuf)
		switch s.typ {
		case sectionMetadata:
			r.meta = s
		case sectionCatalog:
			r.catalog = s
		case sectionEntries:
			r.entries = s
		}
	}
	return r, nil
}

// Close closes the underlying file.
func (r *DNMReader) Close() error {
	return r.f.Close()
}

// EntriesCount returns the number of entry records in the ENTRIES section.
func (r *DNMReader) EntriesCount() int64 {
	return int64(r.entries.count)
}

// CatalogCount returns the number of file-catalog records without decoding
// them, read from the CATALOG section header.
func (r *DNMReader) CatalogCount() int64 {
	return int64(r.catalog.count)
}

// Metadata decodes the METADATA section and nothing else: no catalog, no
// entries. Callers that need specific catalog records pair it with
// StreamCatalog (#419) instead of Load, which materializes both sections.
func (r *DNMReader) Metadata() (Backup, error) {
	return r.readMetadata()
}

// StreamCatalog calls visit with each FileEntry of the CATALOG section, in
// stored order, holding one record at a time. visit returns false to stop.
//
// This is the bounded-memory way to resolve a handful of paths out of a
// whole-volume catalog: a 63.8 GB NTFS member has ~580k records and
// materializing them costs ~180 MB (#419).
func (r *DNMReader) StreamCatalog(visit func(FileEntry) bool) error {
	if r.catalog.count == 0 {
		return nil
	}
	cr, err := r.streamCatalog()
	if err != nil {
		return err
	}
	for i := uint64(0); i < r.catalog.count; i++ {
		fe, err := cr.next()
		if err != nil {
			return fmt.Errorf("reading catalog record %d of %d: %w", i, r.catalog.count, err)
		}
		if !visit(fe) {
			return nil
		}
	}
	return nil
}

// EntryAt seeks to the i-th entry record and decodes it (45-byte read).
func (r *DNMReader) EntryAt(i uint64) (Entry, error) {
	if i >= r.entries.count {
		return Entry{}, fmt.Errorf("entry index %d out of range [0,%d)", i, r.entries.count)
	}
	offset := int64(r.entries.offset) + int64(i)*EntryRecordSize
	if _, err := r.f.Seek(offset, io.SeekStart); err != nil {
		return Entry{}, fmt.Errorf("seeking to entry %d: %w", i, err)
	}
	var buf [EntryRecordSize]byte
	if _, err := io.ReadFull(r.f, buf[:]); err != nil {
		return Entry{}, fmt.Errorf("reading entry %d: %w", i, err)
	}
	return decodeEntryRecord(buf), nil
}

// EntriesRange seeks once to entry start and reads (end-start) records sequentially.
func (r *DNMReader) EntriesRange(start, end uint64) ([]Entry, error) {
	if start >= end {
		return nil, nil
	}
	if end > r.entries.count {
		return nil, fmt.Errorf("entry range [%d,%d) out of bounds (count=%d)", start, end, r.entries.count)
	}
	count := end - start
	offset := int64(r.entries.offset) + int64(start)*EntryRecordSize
	if _, err := r.f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to entry range [%d,%d): %w", start, end, err)
	}
	buf := make([]byte, count*EntryRecordSize)
	if _, err := io.ReadFull(r.f, buf); err != nil {
		return nil, fmt.Errorf("reading entry range [%d,%d): %w", start, end, err)
	}
	entries := make([]Entry, count)
	for i := range count {
		var rec [EntryRecordSize]byte
		copy(rec[:], buf[i*EntryRecordSize:(i+1)*EntryRecordSize])
		entries[i] = decodeEntryRecord(rec)
	}
	return entries, nil
}

// decodeEntryRecord decodes a 45-byte fixed-size entry record.
func decodeEntryRecord(buf [EntryRecordSize]byte) Entry {
	var e Entry
	e.VolumeOffset = int64(binary.LittleEndian.Uint64(buf[0:8]))
	copy(e.ChunkHash[:], buf[8:40])
	e.ChunkLength = int(binary.LittleEndian.Uint32(buf[40:44]))
	e.IsExcluded = buf[44] != 0
	return e
}

// readMetadata seeks to the METADATA section and decodes all scalar Backup
// fields. Entries and FileCatalog are left nil.
func (r *DNMReader) readMetadata() (Backup, error) {
	if _, err := r.f.Seek(int64(r.meta.offset), io.SeekStart); err != nil {
		return Backup{}, fmt.Errorf("seeking to metadata section: %w", err)
	}
	data := make([]byte, r.meta.length)
	if _, err := io.ReadFull(r.f, data); err != nil {
		return Backup{}, fmt.Errorf("reading metadata section: %w", err)
	}
	return decodeMetadata(data)
}

// readAllCatalog loads the entire CATALOG section into a []FileEntry slice.
func (r *DNMReader) readAllCatalog() ([]FileEntry, error) {
	if r.catalog.count == 0 {
		return nil, nil
	}
	cr, err := r.streamCatalog()
	if err != nil {
		return nil, err
	}
	files := make([]FileEntry, 0, r.catalog.count)
	for {
		fe, err := cr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		files = append(files, fe)
	}
	return files, nil
}

// streamCatalog returns a catalogReader positioned at the start of the CATALOG
// section. The caller streams records by calling next() until io.EOF.
func (r *DNMReader) streamCatalog() (*catalogReader, error) {
	if _, err := r.f.Seek(int64(r.catalog.offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to catalog section: %w", err)
	}
	return &catalogReader{
		br:        bufio.NewReaderSize(r.f, 1<<20),
		remaining: r.catalog.count,
	}, nil
}

// readAllEntries loads the entire ENTRIES section into a []Entry slice.
func (r *DNMReader) readAllEntries() ([]Entry, error) {
	if r.entries.count == 0 {
		return nil, nil
	}
	if _, err := r.f.Seek(int64(r.entries.offset), io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to entries section: %w", err)
	}
	br := bufio.NewReaderSize(r.f, 1<<20)
	entries := make([]Entry, 0, r.entries.count)
	var buf [EntryRecordSize]byte
	for range r.entries.count {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return nil, fmt.Errorf("reading entry record: %w", err)
		}
		entries = append(entries, decodeEntryRecord(buf))
	}
	return entries, nil
}

// StreamChunkHashes seeks to the ENTRIES section and calls fn with each
// ChunkHash in sequence, without building a []Entry slice in memory.
// Stops and returns the first non-nil error returned by fn.
func (r *DNMReader) StreamChunkHashes(fn func([32]byte) error) error {
	if r.entries.count == 0 {
		return nil
	}
	if _, err := r.f.Seek(int64(r.entries.offset), io.SeekStart); err != nil {
		return fmt.Errorf("seeking to entries section: %w", err)
	}
	br := bufio.NewReaderSize(r.f, 1<<20)
	var buf [EntryRecordSize]byte
	for range r.entries.count {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			return fmt.Errorf("reading entry record: %w", err)
		}
		var h [32]byte
		copy(h[:], buf[8:40]) // bytes 8–39 are the ChunkHash
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// catalogReader — streaming FileEntry reader for the CATALOG section
// ---------------------------------------------------------------------------

// catalogReader streams FileEntry records from the CATALOG section one at a time.
type catalogReader struct {
	br        *bufio.Reader
	remaining uint64
}

// next reads the next FileEntry from the catalog. Returns io.EOF when all
// records have been consumed.
func (cr *catalogReader) next() (FileEntry, error) {
	if cr.remaining == 0 {
		return FileEntry{}, io.EOF
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(cr.br, lenBuf[:]); err != nil {
		return FileEntry{}, fmt.Errorf("reading catalog record length: %w", err)
	}
	recLen := binary.LittleEndian.Uint32(lenBuf[:])
	data := make([]byte, recLen)
	if _, err := io.ReadFull(cr.br, data); err != nil {
		return FileEntry{}, fmt.Errorf("reading catalog record data: %w", err)
	}
	cr.remaining--
	return decodeFileEntry(data)
}
