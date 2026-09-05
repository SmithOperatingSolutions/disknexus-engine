// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"path/filepath"
	"testing"
)

// #482: CacheFrame retained every fetched frame for the life of the
// process. On a full verify whose entries scatter across pack history,
// that is one frame retained per entry — the RSS ratchet that climbed
// under a healthy GC sawtooth until the 512Mi pod was OOMkilled. The cache
// exists to serve dedup re-reads within a batch window; it must be a
// CACHE, with a ceiling and eviction, not an append-only map.

func TestTheFrameCacheIsBoundedNotAppendOnly(t *testing.T) {
	dir := t.TempDir()
	if err := InitRepo(filepath.Join(dir, "r"), RepoConfig{
		ChunkMinSize: 4096, ChunkAvgSize: 8192, ChunkMaxSize: 16384,
		BuzhashMask: 8191, PackFileMaxSize: 1 << 20, CompressionLevel: 3,
	}); err != nil {
		t.Fatal(err)
	}
	cs, err := NewChunkStore(filepath.Join(dir, "r"), 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	frame := make([]byte, 256<<10) // 256 KB per frame
	for i := 0; i < 1024; i++ {    // 256 MB offered
		cs.CacheFrame(uint32(i), int64(i), frame)
	}
	if got := cs.FrameCacheBytes(); got > FrameCacheMaxBytes {
		t.Fatalf("the frame cache holds %d MB after 256 MB was offered (ceiling %d MB) — it is "+
			"append-only, and a full verify walking scattered dedup references retains one frame per "+
			"entry until the pod is OOMkilled (#482: RSS 405→512 MB under a healthy GC)",
			got>>20, FrameCacheMaxBytes>>20)
	}
	// Positive control (§4): the ceiling is a ceiling, not a disabled cache
	// — recent frames are still served.
	cs.CacheFrame(9999, 42, frame)
	if _, ok := cs.frameCache.Load(frameKey{9999, 42}); !ok {
		t.Fatal("a just-cached frame is not retrievable — the bound turned the cache off entirely, and " +
			"every dedup re-read inside a batch window becomes a refetch")
	}
}
