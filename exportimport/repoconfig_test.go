// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport_test

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/exportimport"
)

func packFileCount(t *testing.T, repoPath string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "chunks"))
	if err != nil {
		t.Fatalf("ReadDir chunks: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pack") {
			n++
		}
	}
	return n
}

// importInto builds a destination repo whose persisted pack_file_max_size and
// compression_level are the given values, imports zipPath into it, and returns
// the resulting pack file count.
func importInto(t *testing.T, zipPath string, packMax int64, level int) int {
	t.Helper()
	repoPath := filepath.Join(t.TempDir(), "dest")
	base := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		Version:          1,
		ChunkMinSize:     base.ChunkMinSize,
		ChunkAvgSize:     base.ChunkAvgSize,
		ChunkMaxSize:     base.ChunkMaxSize,
		BuzhashMask:      base.BuzhashMask,
		PackFileMaxSize:  packMax,
		CompressionLevel: level,
	}); err != nil {
		t.Fatal(err)
	}
	if err := exportimport.Import(context.Background(), repoPath, zipPath, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}
	return packFileCount(t, repoPath)
}

// TestImportResolvesStoredZeros: import appends chunks to the destination
// repo, so it is a writer and must read that repo's stored config the way the
// backup path does. It applied the stored values literally, so importing into
// a repo that never persisted a pack_file_max_size handed the chunk store a
// bound of 0 — a fresh pack sealed for every chunk. #259.
func TestImportResolvesStoredZeros(t *testing.T) {
	srcRepo, cfg := setupRepo(t)
	data := make([]byte, 4*1024*1024)
	rand.Read(data)
	backupID := doBackup(t, srcRepo, data, cfg)

	zipPath := filepath.Join(t.TempDir(), "backup.zip")
	if err := exportimport.Export(srcRepo, []string{backupID}, zipPath, nil); err != nil {
		t.Fatalf("Export: %v", err)
	}

	got := importInto(t, zipPath, 0, 0)
	want := importInto(t, zipPath, config.DefaultPackFileMaxSize, config.DefaultCompressionLevel)
	if got != want {
		t.Errorf("import into a repo with no persisted pack_file_max_size produced %d pack files, "+
			"a repo with an explicit one produced %d", got, want)
	}
}
