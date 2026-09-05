// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// A managed-encryption repo keeps its dedup index in the CLEAR on purpose:
// the controller's server-side restore opens the index with a nil key while
// opening the chunk store with the DEK, so a Web Restore needs nothing from
// the operator. store.IndexKeyFor is the one place that rule lives (#265).
//
// prune was the call site that missed it. cmdPrune hands prune.Options.Key the
// managed DEK, and prune fed that straight into BOTH index opens. With a
// non-nil key the dedup index treats bloom.bin/hash-index.db as decrypted
// working copies and DELETES them on close — on a managed repo those are the
// real files, and no .enc replacement is written. prune then exits 0 while
// every backup in the repo becomes unrestorable, and the documented repair
// (`index --rebuild-all`) refuses managed repos.

// managedRepo creates a managed-encryption repo and returns it with its DEK.
func managedRepo(t *testing.T) (string, config.Config, store.RepoConfig, *crypto.MasterKey) {
	t.Helper()
	cfg := config.Default()
	rc := store.RepoConfig{
		Version:          1,
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
		Encrypted:        true,
		EncryptionMode:   store.EncryptManaged,
	}
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repoPath, rc); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return repoPath, cfg, rc, key
}

// managedBackup runs a real keyed backup, bound exactly as the CLI binds a
// managed repo: chunks encrypted with the DEK, index plaintext.
func managedBackup(t *testing.T, repoPath string, cfg config.Config, rc store.RepoConfig, key *crypto.MasterKey, data []byte) string {
	t.Helper()
	sourcePath := filepath.Join(t.TempDir(), "source.img")
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	p := pipeline.New(cfg, logger, pipeline.MustBind(rc, key))
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	res, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	return res.BackupID
}

func indexFiles(t *testing.T, repoPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "index"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func hasFile(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// assertManagedIndexIntact is the load-bearing assertion: a managed repo's
// index must still be the two plaintext files the controller opens with a nil
// key, and must not have sprouted .enc copies.
func assertManagedIndexIntact(t *testing.T, repoPath, when string) {
	t.Helper()
	names := indexFiles(t, repoPath)
	if !hasFile(names, "bloom.bin") {
		t.Errorf("%s: managed repo's index has no plaintext bloom.bin (index dir: %v) — "+
			"the controller opens this index with a nil key, so deleting it strands every backup", when, names)
	}
	if !hasFile(names, "hash-index.db") {
		t.Errorf("%s: managed repo's index has no plaintext hash-index.db (index dir: %v)", when, names)
	}
	for _, n := range names {
		if filepath.Ext(n) == ".enc" {
			t.Errorf("%s: managed repo's index contains %q — an ENCRYPTED index the controller cannot open", when, n)
		}
	}
}

// restoreOK restores a managed backup exactly as a reader must: chunk store
// keyed with the DEK, index opened with a nil key.
func restoreOK(t *testing.T, repoPath string, cfg config.Config, key *crypto.MasterKey, backupID string, want []byte) error {
	t.Helper()
	b, err := manifest.Load(repoPath, backupID)
	if err != nil {
		return err
	}
	idx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 10000, cfg.BloomFPRate, 1, nil)
	if err != nil {
		return err
	}
	defer idx.CloseDiscard()
	cs, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel, key)
	if err != nil {
		return err
	}
	defer cs.Close()
	out := filepath.Join(t.TempDir(), "out.img")
	w, err := volume.NewWriter(out)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := restore.NewRestorer(idx, cs, logger).Restore(context.Background(), b, w); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	got, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	if len(got) != len(want) {
		t.Fatalf("restored %d bytes, want %d", len(got), len(want))
	}
	// Byte-compare against the original plaintext, not just the length:
	// volume.NewWriter pre-sizes the target, so zeros, stale chunks, or
	// wrongly-decrypted chunks are all length-identical to a correct restore.
	// The restorer's own per-chunk hash check is the code under test verifying
	// itself — this test's authority is the plaintext it backed up.
	if !bytes.Equal(got, want) {
		off := 0
		for off < len(got) && got[off] == want[off] {
			off++
		}
		lo, hi := off-8, off+8
		if lo < 0 {
			lo = 0
		}
		if hi > len(got) {
			hi = len(got)
		}
		t.Fatalf("restore reported success but the restored bytes are WRONG: first difference at offset %d of %d, got %x want %x (bytes %d..%d) — "+
			"an operator restoring this backup gets a correctly-sized image with corrupt content and no error",
			off, len(got), got[lo:hi], want[lo:hi], lo, hi)
	}
	return nil
}

// TestPruneOfAManagedRepoKeepsThePlaintextIndex: the no-orphans path. prune
// opens the index read-only, and with the DEK that open alone deletes
// bloom.bin and hash-index.db.
func TestPruneOfAManagedRepoKeepsThePlaintextIndex(t *testing.T) {
	repoPath, cfg, rc, key := managedRepo(t)
	data := make([]byte, 256*1024)
	rand.Read(data)
	id := managedBackup(t, repoPath, cfg, rc, key, data)

	assertManagedIndexIntact(t, repoPath, "before prune")
	if err := restoreOK(t, repoPath, cfg, key, id, data); err != nil {
		t.Fatalf("restore before prune failed: %v", err)
	}

	// Exactly what cmdPrune constructs for a local managed repo.
	if _, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath, Key: key}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	assertManagedIndexIntact(t, repoPath, "after prune")
	if err := restoreOK(t, repoPath, cfg, key, id, data); err != nil {
		t.Errorf("restore AFTER prune failed: %v — prune reported success and made the repo unrestorable", err)
	}
}

// TestPruneOfAManagedRepoWithOrphansKeepsThePlaintextIndex: the path that
// actually rewrites. The staging index is the mirror image of the same bug —
// it would write an ENCRYPTED index into a managed repo, which the controller
// then cannot open.
func TestPruneOfAManagedRepoWithOrphansKeepsThePlaintextIndex(t *testing.T) {
	repoPath, cfg, rc, key := managedRepo(t)
	data1 := make([]byte, 256*1024)
	rand.Read(data1)
	id1 := managedBackup(t, repoPath, cfg, rc, key, data1)

	data2 := make([]byte, 256*1024)
	rand.Read(data2)
	id2 := managedBackup(t, repoPath, cfg, rc, key, data2)

	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatal(err)
	}

	res, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath, Key: key})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if res.OrphanedChunks == 0 {
		t.Fatal("test is not exercising the rewrite path: no orphans were found")
	}

	assertManagedIndexIntact(t, repoPath, "after prune with orphans")
	if err := restoreOK(t, repoPath, cfg, key, id2, data2); err != nil {
		t.Errorf("restore of the SURVIVING backup after prune failed: %v", err)
	}
}

// TestPruneOfAPassphraseRepoStillEncryptsTheIndex is the regression guard on
// the other side of store.IndexKeyFor: a passphrase repo's index IS encrypted,
// and prune must keep it that way rather than leaving plaintext behind.
func TestPruneOfAPassphraseRepoStillEncryptsTheIndex(t *testing.T) {
	cfg := config.Default()
	rc := store.RepoConfig{
		Version:          1,
		ChunkMinSize:     cfg.ChunkMinSize,
		ChunkAvgSize:     cfg.ChunkAvgSize,
		ChunkMaxSize:     cfg.ChunkMaxSize,
		BuzhashMask:      cfg.BuzhashMask,
		PackFileMaxSize:  cfg.PackFileMaxSize,
		CompressionLevel: cfg.CompressionLevel,
		Encrypted:        true,
		EncryptionMode:   store.EncryptPassphrase,
	}
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repoPath, rc); err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}

	data1 := make([]byte, 256*1024)
	rand.Read(data1)
	id1 := managedBackup(t, repoPath, cfg, rc, key, data1)
	data2 := make([]byte, 256*1024)
	rand.Read(data2)
	managedBackup(t, repoPath, cfg, rc, key, data2)
	if err := manifest.Delete(repoPath, id1); err != nil {
		t.Fatal(err)
	}

	if _, err := prune.Run(context.Background(), prune.Options{RepoPath: repoPath, Key: key}); err != nil {
		t.Fatalf("prune: %v", err)
	}

	names := indexFiles(t, repoPath)
	if !hasFile(names, "bloom.bin.enc") || !hasFile(names, "hash-index.db.enc") {
		t.Errorf("passphrase repo's index after prune is %v — want the .enc pair", names)
	}
	if hasFile(names, "bloom.bin") || hasFile(names, "hash-index.db") {
		t.Errorf("passphrase repo's index after prune left PLAINTEXT files behind: %v", names)
	}
}
