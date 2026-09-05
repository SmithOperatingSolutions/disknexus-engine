// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// dnm_writer.go — writes a complete .dnm manifest file.
//
// The writer:
//  1. Writes a placeholder file header (32 bytes).
//  2. Streams the METADATA, CATALOG, and ENTRIES sections in order, tracking
//     byte offsets as it goes.
//  3. Writes the section index at the end of the file.
//  4. Seeks back to offset 12 and patches the SectionIndexOffset field in the
//     file header.
//
// The file is written to a temp path then renamed atomically so a concurrent
// reader never sees a partial file.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// saveDNM writes a .dnm manifest file for the given backup.
//
// If b.Entries is non-nil, those entries are encoded into the ENTRIES section.
// If b.Entries is nil, the existing .entries sidecar at EntriesPath(repoPath,
// b.BackupID) is copied verbatim (same 45-byte record format), so the pipeline's
// streaming EntryWriter path works without modification.
//
// The directory is assumed to already exist (Save calls MkdirAll before saveDNM).
func saveDNM(repoPath string, b *Backup) error {
	finalPath := DNMPath(repoPath, b.BackupID)
	tmpPath := finalPath + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("creating dnm tmp file: %w", err)
	}

	if writeErr := writeDNM(f, b, repoPath); writeErr != nil {
		f.Close()
		os.Remove(tmpPath)
		return writeErr
	}

	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("syncing dnm file: %w", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing dnm file: %w", err)
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming dnm file: %w", err)
	}
	return nil
}

// writeDNM writes the full .dnm content to f.
func writeDNM(f *os.File, b *Backup, repoPath string) error {
	// Write placeholder header — will be patched with SectionIndexOffset after
	// all sections have been written.
	var placeholder [fileHeaderSize]byte
	if _, err := f.Write(placeholder[:]); err != nil {
		return fmt.Errorf("writing header placeholder: %w", err)
	}
	pos := uint64(fileHeaderSize)

	bw := bufio.NewWriterSize(f, 1<<20)

	// --- METADATA section ------------------------------------------------
	metaOffset := pos
	metaBytes := encodeMetadata(b)
	if _, err := bw.Write(metaBytes); err != nil {
		return fmt.Errorf("writing metadata section: %w", err)
	}
	pos += uint64(len(metaBytes))

	// --- CATALOG section -------------------------------------------------
	catalogOffset := pos
	var catalogCount uint64
	if b.CatalogSidecarPath != "" {
		// Catalog was pre-serialized to a sidecar file by the pipeline to avoid
		// holding the full []FileEntry in RAM during the chunk phase. Stream it
		// directly into the DNM without re-encoding.
		n, count, err := streamCatalogSidecar(bw, b.CatalogSidecarPath)
		if err != nil {
			return fmt.Errorf("streaming catalog sidecar: %w", err)
		}
		catalogCount = count
		pos += uint64(n)
	} else {
		catalogCount = uint64(len(b.FileCatalog))
		for i, fe := range b.FileCatalog {
			rec := encodeFileEntry(fe)
			var lenBuf [4]byte
			binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(rec)))
			if _, err := bw.Write(lenBuf[:]); err != nil {
				return fmt.Errorf("writing catalog record %d length: %w", i, err)
			}
			if _, err := bw.Write(rec); err != nil {
				return fmt.Errorf("writing catalog record %d: %w", i, err)
			}
			pos += 4 + uint64(len(rec))
		}
	}
	catalogLen := pos - catalogOffset

	// --- ENTRIES section -------------------------------------------------
	entriesOffset := pos
	entriesCount, entriesLen, err := writeEntriesSection(bw, b, repoPath)
	if err != nil {
		return err
	}
	pos += entriesLen

	// Flush all section bytes to the underlying *os.File before writing the
	// section index directly.
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flushing section data: %w", err)
	}

	// --- Section index ---------------------------------------------------
	sectionIndexOffset := pos
	idxEntries := [numSections][sectionIndexSize]byte{
		encodeSectionIndex(sectionMetadata, metaOffset, uint64(len(metaBytes)), 1),
		encodeSectionIndex(sectionCatalog, catalogOffset, catalogLen, catalogCount),
		encodeSectionIndex(sectionEntries, entriesOffset, entriesLen, entriesCount),
	}
	for _, ie := range idxEntries {
		if _, err := f.Write(ie[:]); err != nil {
			return fmt.Errorf("writing section index: %w", err)
		}
	}

	// --- Patch file header -----------------------------------------------
	hdr := encodeHeader(sectionIndexOffset)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seeking to file header: %w", err)
	}
	if _, err := f.Write(hdr[:]); err != nil {
		return fmt.Errorf("patching file header: %w", err)
	}

	return nil
}

// writeEntriesSection writes Entry records into bw and returns (count, totalBytes, error).
//
// If b.Entries is non-nil, the entries are encoded one by one. Otherwise the
// binary .entries sidecar file is copied verbatim — both use the identical
// 45-byte record layout, so a direct byte copy is correct and efficient.
func writeEntriesSection(bw *bufio.Writer, b *Backup, repoPath string) (count, length uint64, err error) {
	if len(b.Entries) > 0 {
		var buf [EntryRecordSize]byte
		for _, e := range b.Entries {
			binary.LittleEndian.PutUint64(buf[0:8], uint64(e.VolumeOffset))
			copy(buf[8:40], e.ChunkHash[:])
			binary.LittleEndian.PutUint32(buf[40:44], uint32(e.ChunkLength))
			if e.IsExcluded {
				buf[44] = 1
			} else {
				buf[44] = 0
			}
			if _, err = bw.Write(buf[:]); err != nil {
				return 0, 0, fmt.Errorf("writing entry record: %w", err)
			}
			count++
		}
		length = count * EntryRecordSize
		return count, length, nil
	}

	// No in-memory entries — copy from the sidecar if it exists.
	sidecarsPath := EntriesPath(repoPath, b.BackupID)
	sf, openErr := os.Open(sidecarsPath)
	if openErr != nil {
		if os.IsNotExist(openErr) {
			return 0, 0, nil // empty ENTRIES section
		}
		return 0, 0, fmt.Errorf("opening entries sidecar: %w", openErr)
	}
	defer sf.Close()

	info, statErr := sf.Stat()
	if statErr != nil {
		return 0, 0, fmt.Errorf("stat entries sidecar: %w", statErr)
	}

	n, copyErr := io.Copy(bw, sf)
	if copyErr != nil {
		return 0, 0, fmt.Errorf("copying entries sidecar to dnm: %w", copyErr)
	}

	count = uint64(info.Size()) / EntryRecordSize
	length = uint64(n)
	return count, length, nil
}
