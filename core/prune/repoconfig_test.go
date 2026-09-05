// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// compressiblePattern returns bytes that zstd compresses to visibly different
// sizes at different levels, so a test can tell which level actually wrote a
// pack. Random data is incompressible at every level and would prove nothing.
func compressiblePattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%7) ^ byte(i/1024%13) ^ seed
	}
	return b
}

func packFileCount(t *testing.T, repoPath string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(repoPath, "chunks"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".pack") {
			n++
		}
	}
	return n
}

// prunedRepo builds a repo whose persisted config is rc, writes two backups
// with the config a reader resolves from it, orphans the first, and prunes.
func prunedRepo(t *testing.T, rc store.RepoConfig) string {
	t.Helper()
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repoPath, rc); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	rc.ApplyTo(&cfg)

	id1 := backupData(t, repoPath, cfg, compressiblePattern(4*1024*1024, 0x11))
	backupData(t, repoPath, cfg, compressiblePattern(4*1024*1024, 0x77))
	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatal(err)
	}
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.OrphanedChunks == 0 {
		t.Fatal("test setup: prune found no orphans, so it never rewrote a pack")
	}
	return repoPath
}

func geometry(packMax int64, level int) store.RepoConfig {
	base := config.Default()
	return store.RepoConfig{
		Version:          1,
		ChunkMinSize:     base.ChunkMinSize,
		ChunkAvgSize:     base.ChunkAvgSize,
		ChunkMaxSize:     base.ChunkMaxSize,
		BuzhashMask:      base.BuzhashMask,
		PackFileMaxSize:  packMax,
		CompressionLevel: level,
	}
}

// TestPruneResolvesStoredZeros: prune rewrites every surviving chunk into a
// fresh pack, so it is a writer and must read the stored repo config the same
// way the backup path does. It read repoCfg.PackFileMaxSize literally, so a
// repo that never persisted one handed the staging store a max pack size of
// 0 — which rotates the pack on every single chunk. #259.
func TestPruneResolvesStoredZeros(t *testing.T) {
	zeroPackMax := prunedRepo(t, geometry(0, 0))
	explicitPackMax := prunedRepo(t, geometry(config.DefaultPackFileMaxSize, config.DefaultCompressionLevel))

	gotPacks := packFileCount(t, zeroPackMax)
	wantPacks := packFileCount(t, explicitPackMax)
	if gotPacks != wantPacks {
		t.Errorf("prune of a repo with no persisted pack_file_max_size produced %d pack files, "+
			"a repo with an explicit one produced %d", gotPacks, wantPacks)
	}
}

// TestPruneRewriteIsAByteCopy: prune moves surviving chunks with
// RetrieveRaw/StoreRaw — already-compressed frames copied verbatim — so it
// never recompresses and the compression level it passes the staging store is
// inert. This pins that down: changing how a stored compression level
// resolves must not change what prune leaves behind, and the sizes must stay
// level-sensitive or the check above would be blind.
func TestPruneRewriteIsAByteCopy(t *testing.T) {
	storedZero := dirSize(filepath.Join(prunedRepo(t, geometry(config.DefaultPackFileMaxSize, 0)), "chunks"))
	storedDefault := dirSize(filepath.Join(prunedRepo(t, geometry(config.DefaultPackFileMaxSize, config.DefaultCompressionLevel)), "chunks"))
	if storedZero != storedDefault {
		t.Errorf("a stored compression_level of 0 survived prune as %d bytes, an explicit %d as %d bytes",
			storedZero, config.DefaultCompressionLevel, storedDefault)
	}

	storedFastest := dirSize(filepath.Join(prunedRepo(t, geometry(config.DefaultPackFileMaxSize, 1)), "chunks"))
	if storedFastest == storedDefault {
		t.Fatalf("test is blind: level 1 and level %d left identical chunk bytes (%d)",
			config.DefaultCompressionLevel, storedDefault)
	}
}
