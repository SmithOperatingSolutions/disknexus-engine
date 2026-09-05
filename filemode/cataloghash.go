// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// CatalogHash fingerprints a walk result for resume identity (#51). A file-mode
// backup is a single concatenated stream whose layout is fixed by the catalog,
// so a resumed run is only valid if a fresh walk reproduces the EXACT same
// stream: any added/removed/renamed/resized/touched file changes this hash and
// the resume is refused. Covers walk order, paths, kinds, sizes, offsets, and
// modification times.
func CatalogHash(c *Catalog) [32]byte {
	h := sha256.New()
	for _, sp := range c.SourcePaths {
		fmt.Fprintf(h, "src:%s\x00", sp)
	}
	var buf [8]byte
	for _, f := range c.Files {
		fmt.Fprintf(h, "f:%d:%s\x00", f.SourceIndex, f.Path)
		var kind byte
		if f.IsDir {
			kind |= 1
		}
		if f.IsSymlink {
			kind |= 2
		}
		h.Write([]byte{kind})
		binary.LittleEndian.PutUint64(buf[:], uint64(f.Size))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(f.StreamOffset))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(f.StreamLength))
		h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(f.ModTime.UnixNano()))
		h.Write(buf[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
