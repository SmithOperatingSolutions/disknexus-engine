// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Opening a chunk store must not claim a pack file it may never write to.
//
// Both constructors called openCurrentPack eagerly, so every store — including
// the read-only one a RESTORE builds — created chunks/0000.pack (empty) and
// held it open. A restore then downloads the real pack 0 and renames it into
// exactly that name: POSIX replaces the open empty file and nobody notices,
// Windows denies the rename outright ("Access is denied"), and #357's
// cross-device dedup restore failed on CI with
//
//	destination present at 0 bytes (this download wrote 258302)
//
// A reader creating a writer's artifact is wrong on every platform; Windows is
// just the one that says so.
//
// BOTH CONSTRUCTORS, and NewChunkStoreAt above all (#378 item 6). This test
// used to open only NewChunkStore, while the restore scenario the comment
// above describes goes through NewChunkStoreAt at BOTH cloud read paths —
// captureflow/restore.go openReadStores and controller/restorezip.go — so
// reinstating the eager open on NewChunkStoreAt alone left engine/core/store,
// internal/cloudsync, internal/captureflow and internal/controller all green.
// The one constructor no restore uses was the one that was covered.
//
// storeConstructors is the shared table: a third constructor gets the rule by
// construction or is deliberately left out of it.
var storeConstructors = []struct {
	name string
	open func(dir string) (*ChunkStore, error)
	who  string
}{
	{
		name: "NewChunkStore",
		open: func(dir string) (*ChunkStore, error) { return NewChunkStore(dir, 1<<20, 3) },
		who:  "local backup and local restore",
	},
	{
		name: "NewChunkStoreAt",
		open: func(dir string) (*ChunkStore, error) { return NewChunkStoreAt(dir, 1<<20, 3, 0) },
		who: "EVERY cloud restore — captureflow/restore.go openReadStores and controller/restorezip.go " +
			"both open a read-only store this way, into the very directory a concurrent pack download " +
			"renames its result into",
	},
}

func TestOpeningAStoreDoesNotCreateAPackFile(t *testing.T) {
	for _, c := range storeConstructors {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()

			cs, err := c.open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()

			entries, err := os.ReadDir(filepath.Join(dir, "chunks"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("%s created %s before anything was stored — a read-only user (%s) leaves an "+
					"empty pack behind, and a concurrent download of the real pack cannot then take "+
					"that name", c.name, e.Name(), c.who)
			}
		})
	}
}

// The pack must still appear the moment something is actually stored, and hold
// what was written: laziness may not cost a write its file. Both constructors
// again — the write side of the same change.
func TestStoringOpensThePackOnDemand(t *testing.T) {
	for _, c := range storeConstructors {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()

			cs, err := c.open(dir)
			if err != nil {
				t.Fatal(err)
			}
			packNum, _, _, err := cs.Store([]byte("chunk bytes that must land on disk"))
			if err != nil {
				t.Fatal(err)
			}
			if err := cs.Close(); err != nil {
				t.Fatal(err)
			}

			st, err := os.Stat(filepath.Join(dir, "chunks", fmt.Sprintf("%04d.pack", packNum)))
			if err != nil {
				t.Fatalf("after storing a chunk %s's pack must exist: %v", c.name, err)
			}
			if st.Size() == 0 {
				t.Errorf("%s's pack exists but is empty — the deferred open lost the write", c.name)
			}
		})
	}
}
