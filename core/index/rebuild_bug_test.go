// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// TestRebuild_PackNumberGap is a regression test: Rebuild must derive the
// pack number from the pack filename, not the iteration index. When pack
// files have gaps in their numbering (e.g. after a prune deleted some packs),
// using the loop index would assign wrong pack numbers, breaking restore.
//
// Setup: create packs 0000.pack and 0002.pack (no 0001.pack).
// Expected: chunk from 0002.pack is indexed with PackNumber=2.
func TestRebuild_PackNumberGap(t *testing.T) {
	repoPath := makeTestRepo(t)
	chunksDir := filepath.Join(repoPath, "chunks")
	if err := os.MkdirAll(chunksDir, 0755); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}

	chunk0 := []byte("chunk-in-pack-zero")
	chunk2 := []byte("chunk-in-pack-two")

	writeTestPack(t, filepath.Join(chunksDir, "0000.pack"), [][]byte{chunk0})
	// Skip 0001.pack — simulates a gap from pruning.
	writeTestPack(t, filepath.Join(chunksDir, "0002.pack"), [][]byte{chunk2})

	result, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:         repoPath,
		RebuildBloom:     true,
		RebuildHashIndex: true,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if result.PacksScanned != 2 {
		t.Fatalf("PacksScanned = %d, want 2", result.PacksScanned)
	}

	// Reload the index and look up the chunk from pack 0002.
	indexDir := filepath.Join(repoPath, "index")
	idx, err := index.NewDedupIndex(indexDir, 100, 0.001, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer idx.Close()

	id2 := hasher.Sum(chunk2)
	res, err := idx.Check(id2)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.IsNew || res.Entry == nil {
		t.Fatal("chunk from pack 0002 not found in index")
	}

	// The chunk lives in file 0002.pack, so PackNumber must be 2.
	if res.Entry.PackNumber != 2 {
		t.Errorf("PackNumber = %d, want 2 (chunk is in 0002.pack)", res.Entry.PackNumber)
	}
}
