// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// MultiFileReader concatenates files in catalog order into a single io.Reader.
// Opens one file at a time and transitions seamlessly on EOF.
type MultiFileReader struct {
	catalog     *Catalog
	fileEntries []fileRef // only regular files with data
	totalSize   int64

	currentIdx   int      // index into fileEntries
	currentFile  *os.File // currently open file
	bytesRead    int64    // bytes read from current file
	padRemaining int64    // zero bytes owed for a source file that shrank mid-backup
}

type fileRef struct {
	absPath    string
	streamLen  int64
	catalogIdx int // index into catalog.Files
}

// NewMultiFileReader creates a reader that concatenates all regular files
// in the catalog into a single byte stream.
func NewMultiFileReader(catalog *Catalog) *MultiFileReader {
	var refs []fileRef

	for i, f := range catalog.Files {
		if f.IsDir || f.IsSymlink || f.Size == 0 {
			continue
		}

		srcPath := catalog.SourcePaths[f.SourceIndex]
		absPath := filepath.Join(srcPath, filepath.FromSlash(f.Path))

		refs = append(refs, fileRef{
			absPath:    absPath,
			streamLen:  f.StreamLength,
			catalogIdx: i,
		})
	}

	return &MultiFileReader{
		catalog:     catalog,
		fileEntries: refs,
		totalSize:   catalog.TotalSize,
	}
}

// Read implements io.Reader. Reads bytes from the concatenated file stream.
func (r *MultiFileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	totalRead := 0

	for totalRead < len(p) {
		// Emit zero padding owed for a file that shrank after the catalog
		// walk. Every entry's StreamOffset was fixed at walk time, so the
		// stream must stay exactly streamLen bytes per file — otherwise all
		// subsequent files shift and restore reconstructs them from the
		// wrong bytes.
		if r.padRemaining > 0 {
			want := len(p) - totalRead
			if int64(want) > r.padRemaining {
				want = int(r.padRemaining)
			}
			clear(p[totalRead : totalRead+want])
			totalRead += want
			r.padRemaining -= int64(want)
			continue
		}

		// Open next file if needed
		if r.currentFile == nil {
			if r.currentIdx >= len(r.fileEntries) {
				if totalRead > 0 {
					return totalRead, nil
				}
				return 0, io.EOF
			}

			ref := r.fileEntries[r.currentIdx]
			f, err := os.Open(ref.absPath)
			if err != nil {
				return totalRead, fmt.Errorf("opening %s: %w", ref.absPath, err)
			}
			r.currentFile = f
			r.bytesRead = 0
		}

		ref := r.fileEntries[r.currentIdx]
		remaining := ref.streamLen - r.bytesRead

		// How much to read from this file
		want := len(p) - totalRead
		if int64(want) > remaining {
			want = int(remaining)
		}

		n, err := r.currentFile.Read(p[totalRead : totalRead+want])
		totalRead += n
		r.bytesRead += int64(n)

		if r.bytesRead >= ref.streamLen || err == io.EOF {
			// Done with this file. If it hit EOF short of its catalogued
			// stream length (shrunk since the walk), owe the difference as
			// zero padding to preserve downstream StreamOffsets.
			if err == io.EOF && r.bytesRead < ref.streamLen {
				r.padRemaining = ref.streamLen - r.bytesRead
			}
			r.currentFile.Close()
			r.currentFile = nil
			r.currentIdx++
			continue
		}

		if err != nil {
			return totalRead, err
		}
	}

	return totalRead, nil
}

// SeekTo repositions the stream so the next Read returns the byte at global
// offset off (#51 resume). It locates the file containing off by cumulative
// stream lengths, opens it, and seeks to the intra-file remainder. A file that
// shrank since the walk still pads with zeros exactly like sequential reading:
// Read's EOF handling owes the remainder as padding because bytesRead reflects
// the seeked position.
func (r *MultiFileReader) SeekTo(off int64) error {
	if off < 0 || off > r.totalSize {
		return fmt.Errorf("SeekTo %d out of range [0,%d]", off, r.totalSize)
	}
	// Close any open file; reset state.
	if r.currentFile != nil {
		r.currentFile.Close()
		r.currentFile = nil
	}
	r.padRemaining = 0

	var cum int64
	for i, ref := range r.fileEntries {
		if off < cum+ref.streamLen {
			delta := off - cum
			f, err := os.Open(ref.absPath)
			if err != nil {
				return fmt.Errorf("opening %s for seek: %w", ref.absPath, err)
			}
			if _, err := f.Seek(delta, io.SeekStart); err != nil {
				f.Close()
				return fmt.Errorf("seeking %s to %d: %w", ref.absPath, delta, err)
			}
			r.currentIdx = i
			r.currentFile = f
			r.bytesRead = delta
			return nil
		}
		cum += ref.streamLen
	}
	// off == totalSize: position at clean EOF.
	r.currentIdx = len(r.fileEntries)
	return nil
}

// Size returns the total size of the concatenated stream.
func (r *MultiFileReader) Size() int64 {
	return r.totalSize
}

// Close closes any open file handle.
func (r *MultiFileReader) Close() error {
	if r.currentFile != nil {
		err := r.currentFile.Close()
		r.currentFile = nil
		return err
	}
	return nil
}
