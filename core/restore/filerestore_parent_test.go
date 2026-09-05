// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// An incremental file backup records an unchanged file as a reference to
// the backup that holds its data (DataBackupID). Restoring from the child
// must follow that reference — load the parent's entries, find the SAME
// file there by (source, path) — and write the parent's bytes. A child
// whose parent is gone must say which parent, not restore nothing.
func TestRestoreFilesFollowsUnchangedFilesToTheParentBackup(t *testing.T) {
	repoPath, idx, cs, chunkData, chunkHash := setupTestRepo(t)
	t.Cleanup(func() { idx.Close(); cs.Close() })

	// A second source holds a doc.txt of its own, with different bytes: the
	// parent's catalog has the same path twice, told apart by source index.
	data2 := make([]byte, 4096)
	for i := range data2 {
		data2[i] = byte(i * 7)
	}
	pack2, off2, _, err := cs.Store(data2)
	if err != nil {
		t.Fatal(err)
	}
	hash2 := sha256.Sum256(data2)
	idx.Insert(hasher.Sum(data2), pack2, uint64(off2), 4096)

	parent := &manifest.Backup{BackupID: "parent-0000-0000-0000-000000000001", BackupMode: "file", Timestamp: time.Now(),
		TotalBytes: 8192, SourcePaths: []string{"/src", "/other"},
		FileCatalog: []manifest.FileEntry{
			{Path: "doc.txt", SourceIndex: 0, Size: 4096, Mode: 0o644, StreamOffset: 0, StreamLength: 4096, ContentHash: coveringHash(chunkHash)},
			{Path: "doc.txt", SourceIndex: 1, Size: 4096, Mode: 0o644, StreamOffset: 4096, StreamLength: 4096, ContentHash: coveringHash(hash2)},
		},
		Entries: []manifest.Entry{{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: 4096}, {VolumeOffset: 4096, ChunkHash: hash2, ChunkLength: 4096}}}
	if err := parent.Save(repoPath); err != nil {
		t.Fatal(err)
	}
	child := &manifest.Backup{BackupID: "child0-0000-0000-0000-000000000002", BackupMode: "file", Timestamp: time.Now(),
		ParentBackupID: parent.BackupID, SourcePaths: []string{"/src", "/other"},
		FileCatalog: []manifest.FileEntry{{Path: "doc.txt", SourceIndex: 0, Size: 4096, Mode: 0o644, StreamOffset: 0, StreamLength: 4096,
			ContentHash: coveringHash(chunkHash), Unchanged: true, DataBackupID: parent.BackupID}}}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	r := NewFileRestorer(idx, cs, repoPath, logger)
	target := t.TempDir()
	res, err := r.RestoreFiles(context.Background(), child, target, nil)
	if err != nil {
		t.Fatalf("restoring an unchanged file from the child: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(chunkData) {
		t.Fatalf("restored %d bytes that differ from the parent's data", len(got))
	}
	if res.RestoredFiles != 1 {
		t.Fatalf("result = %+v, want 1 file restored", res)
	}

	// The parent referenced does not exist: the failure names it.
	orphan := *child
	orphan.FileCatalog = []manifest.FileEntry{child.FileCatalog[0]}
	orphan.FileCatalog[0].DataBackupID = "gone00-0000-0000-0000-000000000009"
	if _, err := r.RestoreFiles(context.Background(), &orphan, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "gone00") {
		t.Fatalf("a reference to a missing parent restored or did not name it: %v", err)
	}
	// The same path under the OTHER source resolves to that source's data:
	// (source, path) is the identity, and the parent holds both. (When the
	// parent has the path under one source only, the lookup falls back to
	// it by path — legacy catalogs carry no source index; a priced trade,
	// not exercised here.)
	other := *child
	other.FileCatalog = []manifest.FileEntry{child.FileCatalog[0]}
	other.FileCatalog[0].SourceIndex = 1
	other.FileCatalog[0].ContentHash = coveringHash(hash2)
	otherDir := t.TempDir()
	if _, err := r.RestoreFiles(context.Background(), &other, otherDir, nil); err != nil {
		t.Fatalf("restoring source 1's doc.txt through the parent: %v", err)
	}
	got2, err := os.ReadFile(filepath.Join(otherDir, "doc.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got2) != string(data2) {
		t.Fatal("source 1's doc.txt restored source 0's bytes — the parent lookup matched on path alone")
	}
}
