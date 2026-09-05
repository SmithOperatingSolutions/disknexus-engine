// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/filemode"
)

// A file-tree backup restores EVERY file byte-identical through the
// per-file restore door, including an empty file, a file larger than a
// pack, nested directories, and two files with identical content (which
// must dedup). The authority is the tree the test wrote.
func TestFileTreeBackupRestoresEveryFile(t *testing.T) {
	w := newWorld(t)
	src := filepath.Join(t.TempDir(), "src")
	want := map[string][]byte{
		"empty.txt":           {},
		"docs/readme.md":      []byte("hello, engine\n"),
		"docs/deep/a/b/c.bin": noise(11, 40<<10),
		"big/large.bin":       noise(12, 300<<10), // > PackFileMaxSize
		"dup/one.bin":         noise(13, 64<<10),
		"dup/two.bin":         noise(13, 64<<10), // identical content
	}
	for rel, data := range want {
		p := filepath.Join(src, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cat, err := filemode.Walk(context.Background(), []string{src}, filemode.NewMatcher(nil), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var regular int
	for _, f := range cat.Files {
		if !f.IsDir && !f.IsSymlink {
			regular++
		}
	}
	if regular != len(want) {
		t.Fatalf("fixture defect: walk found %d regular files, the tree has %d (§2)", regular, len(want))
	}
	p := w.pipeline()
	p.SetFileCatalog("file", cat.SourcePaths, cat.Files)
	r := filemode.NewMultiFileReader(cat)
	res, err := p.Backup(context.Background(), r, src, cat.TotalSize, w.repo)
	r.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	w.requirePacks(3)
	if res.DedupChunks == 0 {
		t.Fatal("two identical 64 KB files produced no dedup hits — the file stream is not being deduplicated")
	}

	b := w.load(res.BackupID)
	idx, cs := w.open()
	fr := restore.NewFileRestorer(idx, cs, w.repo, quiet())
	out := t.TempDir()
	for rel, data := range want {
		dst := filepath.Join(out, fmt.Sprintf("%d.out", len(rel))+filepath.Base(rel))
		if _, err := fr.ExtractFile(context.Background(), b, rel, dst); err != nil {
			t.Fatalf("ExtractFile(%s): %v", rel, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Errorf("%s: restored %d bytes differ from the %d written", rel, len(got), len(data))
		}
	}
}
