// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestSealFsyncsChunksDir is the durability guard for issue #42 §8-A: a sealed
// pack's *directory entry* must be fsynced (syncDir on chunks/) before
// OnPackSealed reports the pack durable — otherwise a power-loss can drop the
// dirent of a pack a resume checkpoint already named, making it unrestorable.
// Fsyncing only the pack file's contents (the pre-fix behavior) is insufficient.
//
// This is a white-box test: it swaps the package syncDir var to record the
// order of directory syncs relative to seal callbacks.
func TestSealFsyncsChunksDir(t *testing.T) {
	var mu sync.Mutex
	var events []string
	realSync := syncDir
	syncDir = func(dir string) error {
		mu.Lock()
		events = append(events, "syncdir:"+filepath.Base(dir))
		mu.Unlock()
		return realSync(dir)
	}
	defer func() { syncDir = realSync }()

	dir := t.TempDir()
	// Tiny max pack size so each stored chunk rotates (seals) a pack.
	cs, err := NewChunkStore(dir, 512, 3)
	if err != nil {
		t.Fatal(err)
	}
	cs.OnPackSealed = func(_ string, num uint32, _ int64) error {
		mu.Lock()
		events = append(events, "seal")
		mu.Unlock()
		return nil
	}

	// Store enough incompressible-ish chunks to force at least two rotations.
	for i := 0; i < 4; i++ {
		payload := make([]byte, 400)
		for j := range payload {
			payload[j] = byte(i*31 + j)
		}
		if _, _, _, err := cs.Store(payload); err != nil {
			t.Fatalf("Store: %v", err)
		}
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Must have synced chunks/ at least once.
	var syncedChunks int
	for _, e := range events {
		if e == "syncdir:chunks" {
			syncedChunks++
		}
	}
	if syncedChunks == 0 {
		t.Fatalf("chunks/ directory was never fsynced during pack seals; events=%v", events)
	}

	// Every seal must be immediately preceded by a chunks/ dir sync (the dirent
	// is durable before the pack is announced sealed).
	for i, e := range events {
		if e != "seal" {
			continue
		}
		if i == 0 || events[i-1] != "syncdir:chunks" {
			t.Fatalf("seal at index %d not preceded by a chunks/ dir sync; events=%v", i, events)
		}
	}
}
