// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestPruneKeepsLegacyEmbeddedEntryManifest proves that prune deletes
// every chunk of an old-format backup whose entries are embedded in the
// .manifest JSON (format 3 in manifest.Load's own documentation — no .dnm,
// no .entries sidecar). streamHashes only knows formats 1 and 2:
// with no .dnm it calls manifest.ReadEntries, which returns (nil, nil)
// for a missing sidecar, so the backup contributes zero referenced hashes
// without any error. All of its chunks are then classified as orphans and
// permanently deleted — a still-restorable backup is destroyed by prune.
func TestPruneKeepsLegacyEmbeddedEntryManifest(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	data := make([]byte, 128*1024)
	rand.Read(data)
	id := backupData(t, repoPath, cfg, data)

	// Convert the backup to the old format: embed entries in the JSON
	// manifest and drop the .dnm / .entries sidecars.
	b, err := manifest.Load(repoPath, id)
	if err != nil {
		t.Fatalf("loading manifest: %v", err)
	}
	if len(b.Entries) == 0 {
		t.Fatal("expected loaded manifest to have entries")
	}
	buf, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshaling manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "manifests", id+".manifest"), buf, 0644); err != nil {
		t.Fatalf("writing legacy manifest: %v", err)
	}
	os.Remove(manifest.DNMPath(repoPath, id))
	os.Remove(manifest.EntriesPath(repoPath, id))

	// Sanity: the legacy-format backup is fully loadable.
	legacy, err := manifest.Load(repoPath, id)
	if err != nil {
		t.Fatalf("loading legacy manifest: %v", err)
	}
	if len(legacy.Entries) == 0 {
		t.Fatal("legacy manifest lost its embedded entries")
	}

	// Prune. Every chunk is referenced by this backup, so nothing may be
	// reclaimed.
	result, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if result.OrphanedChunks != 0 {
		t.Errorf("prune classified %d referenced chunks of a live legacy backup as orphans", result.OrphanedChunks)
	}

	// The backup must still restore bit-for-bit.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1)
	if err != nil {
		t.Fatalf("opening index: %v", err)
	}
	defer dedupIdx.Close()

	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	defer chunkStore.Close()

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatalf("opening writer: %v", err)
	}
	defer writer.Close()

	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	if _, err := restorer.Restore(context.Background(), legacy, writer); err != nil {
		t.Fatalf("legacy backup no longer restores after prune: %v", err)
	}

	restored, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatalf("reading restored file: %v", err)
	}
	if !bytes.Equal(restored, data) {
		t.Error("restored data does not match original after prune")
	}
}
