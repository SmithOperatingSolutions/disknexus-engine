// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

func pePayloadForPrune(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	data[0], data[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:0x40], 0x80)
	data[0x80], data[0x81], data[0x82], data[0x83] = 'P', 'E', 0, 0
	binary.LittleEndian.PutUint16(data[0x84:0x86], 0x8664)
	binary.LittleEndian.PutUint16(data[0x86:0x88], 3)
	binary.LittleEndian.PutUint32(data[0x88:0x8C], 0xDEADBEEF)
	binary.LittleEndian.PutUint16(data[0x94:0x96], 112)
	binary.LittleEndian.PutUint16(data[0x98:0x9A], 0x20b)
	binary.LittleEndian.PutUint32(data[0xD8:0xDC], 0xCAFEBABE)
	return data
}

func normalizedBackup(t *testing.T, repoPath string, cfg config.Config, data []byte) *pipeline.Result {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.img")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}
	p := pipeline.New(cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), pipeline.MustBind(store.RepoConfig{Normalizers: []string{"pe"}}, nil))
	reader, err := volume.NewReader(src, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	res, err := p.Backup(context.Background(), reader, src, reader.Size(), repoPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return res
}

// TestPrunePreservesDedupForNormalizedRepo proves prune rebuilds the bloom
// filter keyed on the NORMALIZED weak hash, matching how the pipeline probes
// dedup. Before the fix, prune keyed the bloom on the raw decompressed bytes,
// so after a prune every normalized chunk bloom-missed and a re-backup of the
// same data re-stored it all (UniqueChunks > 0), silently defeating dedup.
func TestPrunePreservesDedupForNormalizedRepo(t *testing.T) {
	repoPath, cfg := setupRepo(t)

	// Record the repo-wide normalizer, as the CLI does on first backup.
	repoCfg, err := store.LoadRepoConfig(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	repoCfg.Normalizers = []string{preprocess.NameePE}
	if err := store.SaveRepoConfig(repoPath, repoCfg); err != nil {
		t.Fatal(err)
	}

	data := pePayloadForPrune(64 * 1024)
	normalizedBackup(t, repoPath, cfg, data)

	// A throwaway backup of different data, deleted to create an orphan so
	// prune actually rewrites the chunk store and rebuilds the bloom.
	junk := make([]byte, 64*1024)
	for i := range junk {
		junk[i] = byte(i * 5)
	}
	junkID := normalizedBackup(t, repoPath, cfg, junk).BackupID
	if err := manifest.Delete(repoPath, junkID); err != nil {
		t.Fatal(err)
	}

	if _, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	// Re-backup the first data: every chunk should dedup against the pruned
	// store via the rebuilt bloom.
	res := normalizedBackup(t, repoPath, cfg, data)
	if res.UniqueChunks != 0 {
		t.Fatalf("dedup defeated after prune of a normalized repo: %d new unique chunks, want 0", res.UniqueChunks)
	}
}
