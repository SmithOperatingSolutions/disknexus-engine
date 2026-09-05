// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport_test

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/exportimport"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// Regression tests for issue #14 (exportimport correctness):
//   1. Export must work on encrypted repos (index is stored encrypted).
//   2. Export must include ancestor backups referenced via DataBackupID.
//   4. Import must rebuild the index before making backups visible.

// setupEncryptedRepo creates a passphrase-encrypted repo and returns its path,
// config, and the master key.
func setupEncryptedRepo(t *testing.T) (string, config.Config, *crypto.MasterKey) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	cfg := config.Default()
	if err := store.InitRepo(repoPath, store.RepoConfig{
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
		Encrypted:        true,
		EncryptionMode:   store.EncryptPassphrase,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	t.Cleanup(mk.Destroy)
	return repoPath, cfg, mk
}

// doBackupKey runs a volume-mode backup with an encryption key.
func doBackupKey(t *testing.T, repoPath string, data []byte, cfg config.Config, mk *crypto.MasterKey) string {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "source.img")
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	p := pipeline.New(cfg, newLogger(), pipeline.MustBind(store.RepoConfig{EncryptionMode: store.EncryptPassphrase}, mk))
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return result.BackupID
}

// TestExportEncryptedRepo proves Export succeeds on an encrypted repo, whose
// dedup index exists only as hash-index.db.enc. Before the fix, Export opened
// the index with no key, got a fresh empty index, and every chunk lookup missed
// with "referenced in backup but not found in index". It also proves the key is
// actually required: exporting the same repo with a nil key fails.
func TestExportEncryptedRepo(t *testing.T) {
	repoPath, cfg, mk := setupEncryptedRepo(t)

	sourceData := make([]byte, 128*1024)
	rand.Read(sourceData)
	backupID := doBackupKey(t, repoPath, sourceData, cfg, mk)

	// With the key, Export must succeed.
	zipPath := filepath.Join(t.TempDir(), "enc.zip")
	if err := exportimport.Export(repoPath, []string{backupID}, zipPath, mk); err != nil {
		t.Fatalf("Export of encrypted repo with key failed: %v", err)
	}
	if info, err := os.Stat(zipPath); err != nil || info.Size() == 0 {
		t.Fatalf("expected a non-empty zip, stat err=%v", err)
	}

	// Without the key, the encrypted index can't be read, so Export must fail
	// rather than silently produce an archive missing every chunk.
	nilKeyZip := filepath.Join(t.TempDir(), "enc-nokey.zip")
	if err := exportimport.Export(repoPath, []string{backupID}, nilKeyZip, nil); err == nil {
		t.Fatal("Export of an encrypted repo with a nil key unexpectedly succeeded; the chunks would be unresolvable")
	}
}

// TestExportIncludesDataBackupIDAncestors proves Export follows
// FileEntry.DataBackupID (watcher-mode unchanged files) and pulls the ancestor
// backup's manifest + chunks into the archive. Backup B references backup A's
// chunk data but contains no chunks of its own; exporting only B must still
// yield an archive from which A (and thus B's unchanged files) is restorable.
func TestExportIncludesDataBackupIDAncestors(t *testing.T) {
	ctx := context.Background()
	repoPath, cfg := setupRepo(t)

	// Backup A: real volume backup, holds the actual chunk data.
	sourceData := make([]byte, 256*1024)
	rand.Read(sourceData)
	aID := doBackup(t, repoPath, sourceData, cfg)

	aManifest, err := manifest.Load(repoPath, aID)
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	if len(aManifest.Entries) == 0 {
		t.Fatal("backup A has no chunk entries")
	}

	// Backup B: a watcher-style file-mode manifest whose single file is
	// Unchanged and points at A for its data. B carries no chunks of its own.
	bID := "00000000-0000-0000-0000-0000000000b2"
	b := &manifest.Backup{
		BackupID:       bID,
		Timestamp:      time.Unix(1700000000, 0),
		SourceVolume:   "files",
		BackupMode:     "file",
		BackupType:     "incremental",
		ParentBackupID: aID,
		SourcePaths:    []string{"src"},
		FileCatalog: []manifest.FileEntry{{
			Path:         "unchanged.dat",
			SourceIndex:  0,
			Size:         int64(len(sourceData)),
			StreamOffset: 0,
			StreamLength: int64(len(sourceData)),
			Unchanged:    true,
			DataBackupID: aID,
		}},
	}
	if err := b.Save(repoPath); err != nil {
		t.Fatalf("save B: %v", err)
	}

	// Export ONLY B.
	zipPath := filepath.Join(t.TempDir(), "b.zip")
	if err := exportimport.Export(repoPath, []string{bID}, zipPath, nil); err != nil {
		t.Fatalf("Export(B): %v", err)
	}

	// Import into a fresh repo and confirm A's manifest and every one of A's
	// chunks came along — that is exactly what B's unchanged file needs.
	repoPath2, _ := setupRepo(t)
	if err := exportimport.Import(ctx, repoPath2, zipPath, nil); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if _, err := manifest.Load(repoPath2, aID); err != nil {
		t.Fatalf("ancestor backup A was not included in the export (manifest.Load: %v); B's unchanged files would be unrestorable", err)
	}

	// Use the htab-building constructor: LookupDirect needs the in-memory hash
	// table (the read-only variant skips it).
	idx, err := index.NewDedupIndex(filepath.Join(repoPath2, "index"), 0, cfg.BloomFPRate, 0)
	if err != nil {
		t.Fatalf("open dest index: %v", err)
	}
	defer idx.Close()
	for _, e := range aManifest.Entries {
		if e.IsExcluded {
			continue
		}
		_, found, err := idx.LookupDirect(e.ChunkHash)
		if err != nil {
			t.Fatalf("LookupDirect: %v", err)
		}
		if !found {
			t.Fatalf("chunk %x from ancestor A missing in imported repo; ancestor chunks were not exported", e.ChunkHash)
		}
	}
}

// TestImportRebuildsIndexBeforeInstallingManifests proves Import installs
// manifests only AFTER a successful index rebuild. A crafted archive carries a
// valid manifest but a corrupt chunk frame that makes the rebuild fail; the
// import must error and leave NO listable backup (whose chunks would be
// unresolvable). Before the fix, manifests were copied first, so the backup
// became visible despite the failed rebuild.
func TestImportRebuildsIndexBeforeInstallingManifests(t *testing.T) {
	ctx := context.Background()
	repoPath, cfg := setupRepo(t)

	// A real backup so we have a valid .dnm to smuggle into the crafted zip.
	sourceData := make([]byte, 64*1024)
	rand.Read(sourceData)
	aID := doBackup(t, repoPath, sourceData, cfg)
	dnmBytes, err := os.ReadFile(manifest.DNMPath(repoPath, aID))
	if err != nil {
		t.Fatalf("read A .dnm: %v", err)
	}

	// A corrupt chunk frame: an 8-byte header claiming a ~4GB payload followed
	// by only a few bytes. StoreRaw writes it verbatim; the rebuild's streamPack
	// then fails reading the (missing) payload.
	var badFrame [16]byte
	binary.LittleEndian.PutUint32(badFrame[0:4], 0xFFFFFFF0)
	var badHash [32]byte
	badHash[0] = 0xAB

	craftedZip := filepath.Join(t.TempDir(), "crafted.zip")
	writeZip(t, craftedZip, map[string][]byte{
		"manifests/" + aID + ".dnm":                           dnmBytes,
		"chunks/" + hex.EncodeToString(badHash[:]) + ".frame": badFrame[:],
	})

	repoPath2, _ := setupRepo(t)
	if err := exportimport.Import(ctx, repoPath2, craftedZip, nil); err == nil {
		t.Fatal("expected Import to fail on the corrupt chunk frame during index rebuild")
	}

	// The rebuild failed, so the manifest must NOT have been installed.
	if _, err := manifest.Load(repoPath2, aID); err == nil {
		t.Fatal("backup manifest was installed despite the index rebuild failing; a listable backup with unresolvable chunks")
	}
}

// writeZip writes a zip archive containing the given name→content entries.
func writeZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	defer f.Close()
	w := zip.NewWriter(f)
	for name, content := range entries {
		zf, err := w.Create(name)
		if err != nil {
			t.Fatalf("zip Create %s: %v", name, err)
		}
		if _, err := zf.Write(content); err != nil {
			t.Fatalf("zip Write %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
}
