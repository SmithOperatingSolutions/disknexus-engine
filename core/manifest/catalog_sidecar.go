// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// WriteCatalogSidecar serializes files to a temporary sidecar file using the
// same length-prefixed record format as the DNM CATALOG section. The file
// begins with an 8-byte little-endian record count so the DNM writer can
// stream it without knowing the count in advance.
//
// Returns the number of records written. The caller is responsible for
// deleting the file when it is no longer needed.
func WriteCatalogSidecar(files []FileEntry, path string) (int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, fmt.Errorf("creating catalog sidecar: %w", err)
	}
	defer f.Close()

	// Reserve space for the 8-byte count header; patched after writing records.
	var countBuf [8]byte
	if _, err := f.Write(countBuf[:]); err != nil {
		return 0, fmt.Errorf("writing catalog sidecar count placeholder: %w", err)
	}

	bw := bufio.NewWriterSize(f, 1<<20)
	var count int64
	for _, fe := range files {
		rec := encodeFileEntry(fe)
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(rec)))
		if _, err := bw.Write(lenBuf[:]); err != nil {
			return 0, fmt.Errorf("writing catalog sidecar record length: %w", err)
		}
		if _, err := bw.Write(rec); err != nil {
			return 0, fmt.Errorf("writing catalog sidecar record: %w", err)
		}
		count++
	}
	if err := bw.Flush(); err != nil {
		return 0, fmt.Errorf("flushing catalog sidecar: %w", err)
	}

	// Patch count header.
	binary.LittleEndian.PutUint64(countBuf[:], uint64(count))
	if _, err := f.WriteAt(countBuf[:], 0); err != nil {
		return 0, fmt.Errorf("patching catalog sidecar count: %w", err)
	}

	return count, nil
}

// streamCatalogSidecar copies a catalog sidecar written by WriteCatalogSidecar
// into dst, skipping the 8-byte count header. Returns (bytesWritten, count, error).
func streamCatalogSidecar(dst *bufio.Writer, path string) (bytesWritten int64, count uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("opening catalog sidecar: %w", err)
	}
	defer f.Close()

	var countBuf [8]byte
	if _, err := io.ReadFull(f, countBuf[:]); err != nil {
		return 0, 0, fmt.Errorf("reading catalog sidecar count: %w", err)
	}
	count = binary.LittleEndian.Uint64(countBuf[:])

	n, err := io.Copy(dst, f)
	if err != nil {
		return 0, 0, fmt.Errorf("streaming catalog sidecar: %w", err)
	}
	return n, count, nil
}
