// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// #153 slice A: recovery must not download a 512 MB pack to serve one chunk.
// When a pack is absent locally and OnChunkFetch is set, Retrieve gets the
// chunk's frame via the fetcher (an S3 range GET in production) — never
// invoking the whole-pack OnPackMissing path — and caches the frame so
// dedup'd re-reads don't re-fetch.
func TestRetrieveViaChunkFetch(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewChunkStore(dir, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cs.Close() }() // late-bound: cs is reassigned mid-test

	// Store a chunk normally, remember its location, then remove the pack
	// so retrieval must go through the fetch path.
	payload := []byte("recovery hardening: chunk-granular fetch")
	packNum, offset, _, err := cs.Store(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Flush(); err != nil {
		t.Fatal(err)
	}
	// Grab the frame bytes straight from the pack before deleting it.
	frame, _, err := cs.RetrieveRaw(packNum, offset)
	if err != nil {
		t.Fatal(err)
	}
	// Windows: the store holds its current pack open — close it (releasing
	// handles) before deleting, and retrieve through a FRESH store.
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	packs, _ := filepath.Glob(filepath.Join(dir, "chunks", "*.pack"))
	for _, p := range packs {
		if err := os.Remove(p); err != nil {
			t.Fatalf("removing %s: %v", p, err)
		}
	}
	cs, err = NewChunkStore(dir, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}

	var chunkFetches, packFetches atomic.Int32
	cs.OnChunkFetch = func(pn uint32, off int64) ([]byte, error) {
		chunkFetches.Add(1)
		if pn != packNum || off != offset {
			t.Errorf("fetch (%d,%d), want (%d,%d)", pn, off, packNum, offset)
		}
		out := make([]byte, len(frame))
		copy(out, frame)
		return out, nil
	}
	cs.OnPackMissing = func(pn uint32) error {
		packFetches.Add(1)
		t.Error("whole-pack download invoked despite OnChunkFetch")
		return os.ErrNotExist
	}

	got, err := cs.Retrieve(packNum, offset)
	if err != nil {
		t.Fatalf("Retrieve via chunk fetch: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch: %q", got)
	}
	// Dedup'd second read: served from the frame cache, no second fetch.
	if _, err := cs.Retrieve(packNum, offset); err != nil {
		t.Fatal(err)
	}
	if chunkFetches.Load() != 1 || packFetches.Load() != 0 {
		t.Fatalf("fetches: chunk=%d pack=%d, want 1/0", chunkFetches.Load(), packFetches.Load())
	}
}
