// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func buildTestCatalog(t *testing.T) (*Catalog, []byte) {
	t.Helper()
	dir := t.TempDir()
	var full []byte
	files := map[string]int{"a.bin": 10_000, "b/c.bin": 1, "d.bin": 65_536, "e.bin": 3_333}
	for name, size := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0755)
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i*7 + len(name))
		}
		if err := os.WriteFile(p, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	cat, err := Walk(t.Context(), []string{dir}, NewMatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewMultiFileReader(cat)
	full, err = io.ReadAll(r)
	r.Close()
	if err != nil || int64(len(full)) != cat.TotalSize {
		t.Fatalf("full read %d bytes (err=%v), want %d", len(full), err, cat.TotalSize)
	}
	return cat, full
}

// TestMultiFileReaderSeekTo: reading from SeekTo(off) must equal the tail of a
// full sequential read, at file boundaries, intra-file offsets, and EOF.
func TestMultiFileReaderSeekTo(t *testing.T) {
	cat, full := buildTestCatalog(t)
	offsets := []int64{0, 1, 9_999, 10_000, 10_001, 10_002, 50_000, cat.TotalSize - 1, cat.TotalSize}
	for _, off := range offsets {
		r := NewMultiFileReader(cat)
		if err := r.SeekTo(off); err != nil {
			t.Fatalf("SeekTo(%d): %v", off, err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatalf("read after SeekTo(%d): %v", off, err)
		}
		if !bytes.Equal(got, full[off:]) {
			t.Fatalf("SeekTo(%d): tail mismatch (%d vs %d bytes)", off, len(got), len(full)-int(off))
		}
	}
	// Out of range refused.
	r := NewMultiFileReader(cat)
	if err := r.SeekTo(cat.TotalSize + 1); err == nil {
		t.Fatal("SeekTo beyond stream must error")
	}
	r.Close()
}

// TestCatalogHashSensitivity: any layout-affecting change flips the hash.
func TestCatalogHashSensitivity(t *testing.T) {
	cat, _ := buildTestCatalog(t)
	base := CatalogHash(cat)

	// Deterministic.
	if CatalogHash(cat) != base {
		t.Fatal("hash not deterministic")
	}
	mut := func(f func(c *Catalog)) [32]byte {
		cc := *cat
		cc.Files = append(cc.Files[:0:0], cat.Files...)
		f(&cc)
		return CatalogHash(&cc)
	}
	if mut(func(c *Catalog) { c.Files[0].StreamLength++ }) == base {
		t.Fatal("insensitive to stream length")
	}
	if mut(func(c *Catalog) { c.Files[1].Path += "x" }) == base {
		t.Fatal("insensitive to path")
	}
	if mut(func(c *Catalog) { c.Files[2].ModTime = c.Files[2].ModTime.Add(time.Second) }) == base {
		t.Fatal("insensitive to mtime")
	}
	if mut(func(c *Catalog) { c.Files = c.Files[:len(c.Files)-1] }) == base {
		t.Fatal("insensitive to removed file")
	}
}
