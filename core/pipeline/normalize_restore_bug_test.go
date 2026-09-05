// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// pePayload builds a buffer that starts with a valid-enough PE header
// (recognized by PENormalizer's hardened validation) carrying a non-zero
// TimeDateStamp and CheckSum, padded to size with deterministic filler.
func pePayload(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	data[0], data[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:0x40], 0x80) // e_lfanew -> 0x80
	data[0x80], data[0x81], data[0x82], data[0x83] = 'P', 'E', 0, 0
	binary.LittleEndian.PutUint16(data[0x84:0x86], 0x8664)     // Machine amd64
	binary.LittleEndian.PutUint16(data[0x86:0x88], 3)          // NumberOfSections
	binary.LittleEndian.PutUint32(data[0x88:0x8C], 0xDEADBEEF) // TimeDateStamp (volatile)
	binary.LittleEndian.PutUint16(data[0x94:0x96], 112)        // SizeOfOptionalHeader
	binary.LittleEndian.PutUint16(data[0x98:0x9A], 0x20b)      // opt magic PE32+
	binary.LittleEndian.PutUint32(data[0xD8:0xDC], 0xCAFEBABE) // CheckSum (volatile)
	return data
}

// TestNormalizedBackupRestores proves that a --normalize backup is
// restorable. The pipeline hashes the NORMALIZED bytes for chunk identity
// but stores the ORIGINAL bytes; restore must re-normalize before verifying
// against the manifest's chunk hash. Before the fix, restore aborts with a
// "chunk integrity error" because sha256(original) != normalized ChunkHash.
func TestNormalizedBackupRestores(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	source := pePayload(4096)
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatal(err)
	}

	norm := &preprocess.PENormalizer{}
	// Sanity: the normalizer actually alters these bytes, so the bug is live.
	if bytes.Equal(norm.Normalize(source), source) {
		t.Fatal("test payload is not affected by the normalizer; would not exercise the bug")
	}

	logger := newLogger()
	p := pipeline.New(cfg, logger, noEncNorm(preprocess.NameePE))

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	backup, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dedupIdx.Close()
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer chunkStore.Close()

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	restorer := restore.NewRestorer(dedupIdx, chunkStore, logger)
	restorer.SetNormalizer(norm) // read path must know the repo's normalizer
	_, err = restorer.Restore(context.Background(), backup, writer)
	writer.Close()
	if err != nil {
		t.Fatalf("Restore of a --normalize backup failed: %v", err)
	}

	got, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	// The stored bytes are the originals, so a single-chunk restore is exact.
	if !bytes.Equal(got, source) {
		t.Fatalf("restored data does not match source (%d vs %d bytes)", len(got), len(source))
	}
}

// TestVerifyNormalizedBackup proves the same for the Verify path, which also
// recomputes sha256 of the stored bytes and must re-normalize first.
func TestVerifyNormalizedBackup(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	source := pePayload(4096)
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatal(err)
	}

	norm := &preprocess.PENormalizer{}
	logger := newLogger()
	p := pipeline.New(cfg, logger, noEncNorm(preprocess.NameePE))

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	backup, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dedupIdx.Close()
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer chunkStore.Close()

	vr, err := restore.VerifyWithNormalizer(context.Background(), backup, dedupIdx, chunkStore, norm)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !vr.OK() {
		t.Fatalf("Verify reported a healthy normalized backup as corrupt: %+v", vr.Errors)
	}
}

// TestNormalizedBackupRebuildThenRestore proves index rebuild keys the index
// by the NORMALIZED hash (recorded in RepoConfig.Normalizers), so restore's
// LookupDirect still finds every chunk. Before the fix, rebuild hashed the
// stored (original) bytes, so lookups missed and restore failed with "chunk
// not found in index".
func TestNormalizedBackupRebuildThenRestore(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	// Record the repo-wide normalizer as the CLI would on the first backup.
	repoCfg, err := store.LoadRepoConfig(repoPath)
	if err != nil {
		t.Fatal(err)
	}
	repoCfg.Normalizers = []string{preprocess.NameePE}
	if err := store.SaveRepoConfig(repoPath, repoCfg); err != nil {
		t.Fatal(err)
	}

	source := pePayload(4096)
	if err := os.WriteFile(sourcePath, source, 0644); err != nil {
		t.Fatal(err)
	}
	norm := &preprocess.PENormalizer{}

	p := pipeline.New(cfg, newLogger(), noEncNorm(preprocess.NameePE))
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Blow away the index and rebuild it purely from the packs + repo config.
	if err := os.RemoveAll(filepath.Join(repoPath, "index")); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:         repoPath,
		RebuildBloom:     true,
		RebuildHashIndex: true,
	}); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	backup, err := manifest.Load(repoPath, result.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	dedupIdx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, cfg.IndexCacheMB, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dedupIdx.Close()
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer chunkStore.Close()

	restorePath := filepath.Join(t.TempDir(), "restored.img")
	writer, err := volume.NewWriter(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	restorer := restore.NewRestorer(dedupIdx, chunkStore, newLogger())
	restorer.SetNormalizer(norm)
	if _, err := restorer.Restore(context.Background(), backup, writer); err != nil {
		writer.Close()
		t.Fatalf("Restore after rebuild failed (index keyed by wrong hash?): %v", err)
	}
	writer.Close()

	got, err := os.ReadFile(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("restored data mismatch after rebuild")
	}
}
