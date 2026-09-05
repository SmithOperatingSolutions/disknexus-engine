// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func TestDeletePacksAbove(t *testing.T) {
	repo := t.TempDir()
	chunks := filepath.Join(repo, "chunks")
	if err := os.MkdirAll(chunks, 0755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"0000.pack", "0001.pack", "0002.pack", "0003.pack"} {
		if err := os.WriteFile(filepath.Join(chunks, n), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Keep 0..1, delete 2..3.
	if err := store.DeletePacksAbove(repo, 1); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"0000.pack", "0001.pack"} {
		if _, err := os.Stat(filepath.Join(chunks, n)); err != nil {
			t.Fatalf("%s should survive: %v", n, err)
		}
	}
	for _, n := range []string{"0002.pack", "0003.pack"} {
		if _, err := os.Stat(filepath.Join(chunks, n)); !os.IsNotExist(err) {
			t.Fatalf("%s should be deleted", n)
		}
	}
	// Idempotent / missing dir is fine.
	if err := store.DeletePacksAbove(filepath.Join(repo, "nope"), 0); err != nil {
		t.Fatalf("missing chunks dir should be a no-op: %v", err)
	}
}

func TestMaxPackNum(t *testing.T) {
	repo := t.TempDir()
	chunks := filepath.Join(repo, "chunks")
	if err := os.MkdirAll(chunks, 0755); err != nil {
		t.Fatal(err)
	}

	// No packs yet.
	if n, found, err := store.MaxPackNum(repo); err != nil || found || n != 0 {
		t.Fatalf("empty repo: got (%d,%v,%v), want (0,false,nil)", n, found, err)
	}

	for _, name := range []string{"0000.pack", "0005.pack", "0002.pack"} {
		if err := os.WriteFile(filepath.Join(chunks, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	n, found, err := store.MaxPackNum(repo)
	if err != nil || !found || n != 5 {
		t.Fatalf("got (%d,%v,%v), want (5,true,nil)", n, found, err)
	}

	// Missing chunks dir → not found, no error.
	if n, found, err := store.MaxPackNum(filepath.Join(repo, "nope")); err != nil || found || n != 0 {
		t.Fatalf("missing dir: got (%d,%v,%v), want (0,false,nil)", n, found, err)
	}
}
