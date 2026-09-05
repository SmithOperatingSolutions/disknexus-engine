// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

// A real NTFS image: the filesystem scanner produces a file catalog, the
// image backs up as a volume with that catalog attached, the whole image
// restores byte-identical, and one file is extractable from the image by
// path. The authority for the image is its SHA-256 on disk; for the file,
// its catalog size (the scanner's number, checked against what the
// engine wrote back).
func TestNTFSImageBacksUpWithCatalogAndRestores(t *testing.T) {
	img := filepath.Join("..", "..", "volumefs", "testdata", "ntfs.img")
	src, err := os.ReadFile(img)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	w := newWorld(t)

	scan, err := volumefs.ScanVolume(context.Background(), img, int64(len(src)), nil, "")
	if err != nil {
		t.Fatalf("ScanVolume: %v", err)
	}
	if scan.Filesystem != "ntfs" {
		t.Fatalf("fixture defect: scanner says %q, expected ntfs (§2)", scan.Filesystem)
	}
	var pick *manifest.FileEntry
	for i := range scan.Files {
		f := &scan.Files[i]
		if !f.IsDir && !f.IsSymlink && f.Size > 0 && (pick == nil || f.Size > pick.Size) {
			pick = f
		}
	}
	if pick == nil {
		t.Fatal("fixture defect: the image has no non-empty regular file to extract (§2)")
	}

	p := w.pipeline()
	p.SetFileCatalog("volume", []string{img}, scan.Files)
	rd, err := volume.NewReader(img, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Backup(context.Background(), rd, img, int64(len(src)), w.repo)
	rd.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	w.requirePacks(2)

	if got := sum(w.restoreBytes(res.BackupID)); got != sum(src) {
		t.Fatalf("restored image differs from the fixture on disk")
	}

	b := w.load(res.BackupID)
	if len(b.FileCatalog) != len(scan.Files) {
		t.Fatalf("manifest carries %d catalog entries, scanner produced %d", len(b.FileCatalog), len(scan.Files))
	}
	idx, cs := w.open()
	out := filepath.Join(t.TempDir(), "one.file")
	if _, err := restore.NewFileRestorer(idx, cs, w.repo, quiet()).ExtractFile(context.Background(), b, pick.Path, out); err != nil {
		t.Fatalf("ExtractFile(%s): %v", pick.Path, err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != pick.Size {
		t.Fatalf("%s: extracted %d bytes, catalog says %d", pick.Path, st.Size(), pick.Size)
	}
}
