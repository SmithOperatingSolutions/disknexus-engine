// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package e2e is the engine's own end-to-end suite: every scenario drives
// the public engine API only — chunker, packs, index, manifest, restore,
// prune, verify, the byte readers — against a LOCAL repository, and judges
// the result against an authority the engine did not produce (a SHA-256 of
// the source bytes, a file count from the OS, a byte the test itself
// flipped). No cloud, no controller, no agent: this is the proof that a
// repository written by this module restores with this module alone.
//
// Written to docs/TESTING.md: every fixture is interrogated before it is
// trusted (§2), every verdict is against an authority (§3), and every
// guard that could pass vacuously is fenced (§8).
package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// smallGeometry is the product's chunking shape scaled so a ~1 MB source
// spans many packs: the scenarios below need pack boundaries, dedup hits,
// and multi-pack restores to be REAL, not incidental.
func smallGeometry() config.Config {
	cfg := config.Default()
	cfg.ChunkMinSize = 2 << 10
	cfg.ChunkAvgSize = 8 << 10
	cfg.ChunkMaxSize = 64 << 10
	cfg.BuzhashMask = uint64(8<<10) - 1
	cfg.PackFileMaxSize = 128 << 10
	return cfg
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// world is one scenario's repository.
type world struct {
	t    *testing.T
	repo string
	cfg  config.Config
	rc   store.RepoConfig
	key  *crypto.MasterKey // nil unless encrypted
}

func newWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, nil)
}

func newWorldWith(t *testing.T, key *crypto.MasterKey) *world {
	t.Helper()
	cfg := smallGeometry()
	rc := store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize, ChunkMaxSize: cfg.ChunkMaxSize,
		BuzhashMask: cfg.BuzhashMask, PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}
	if key != nil {
		rc.Encrypted = true
		rc.EncryptionMode = store.EncryptPassphrase
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repo, rc); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	return &world{t: t, repo: repo, cfg: cfg, rc: rc, key: key}
}

func (w *world) binding() pipeline.Binding { return pipeline.MustBind(w.rc, w.key) }

func (w *world) pipeline() *pipeline.Pipeline { return pipeline.New(w.cfg, quiet(), w.binding()) }

// open returns the index and store for reading, with the world's key.
func (w *world) open() (*index.DedupIndex, *store.ChunkStore) {
	return w.openWithKey(w.key)
}

func (w *world) openWithKey(key *crypto.MasterKey) (*index.DedupIndex, *store.ChunkStore) {
	w.t.Helper()
	idx, cs, err := w.tryOpen(key)
	if err != nil {
		w.t.Fatalf("open repo: %v", err)
	}
	w.t.Cleanup(func() { cs.Close(); idx.Close() })
	return idx, cs
}

// tryOpen opens the index and store with key, returning the engine's
// refusal instead of failing the test: a wrong key is rejected at OPEN
// (the index's bloom filter fails authentication), which is the right
// place — and a scenario about refusal must be able to observe it.
func (w *world) tryOpen(key *crypto.MasterKey) (*index.DedupIndex, *store.ChunkStore, error) {
	w.t.Helper()
	b := pipeline.MustBind(w.rc, key)
	idx, err := index.NewDedupIndex(filepath.Join(w.repo, "index"), 10000, w.cfg.BloomFPRate, w.cfg.IndexCacheMB, b.IndexKey())
	if err != nil {
		return nil, nil, err
	}
	var cs *store.ChunkStore
	if key != nil {
		cs, err = store.NewChunkStore(w.repo, w.cfg.PackFileMaxSize, w.cfg.CompressionLevel, key)
	} else {
		cs, err = store.NewChunkStore(w.repo, w.cfg.PackFileMaxSize, w.cfg.CompressionLevel)
	}
	if err != nil {
		idx.Close()
		return nil, nil, err
	}
	return idx, cs, nil
}

// backupBytes runs a full volume-mode backup of data.
func (w *world) backupBytes(data []byte, source string) *pipeline.Result {
	w.t.Helper()
	res, err := w.pipeline().Backup(context.Background(), bytes.NewReader(data), source, int64(len(data)), w.repo)
	if err != nil {
		w.t.Fatalf("Backup: %v", err)
	}
	return res
}

func (w *world) backupIncremental(data []byte, source, parent string) *pipeline.Result {
	w.t.Helper()
	res, err := w.pipeline().BackupIncremental(context.Background(), bytes.NewReader(data), source, int64(len(data)), w.repo, parent)
	if err != nil {
		w.t.Fatalf("BackupIncremental: %v", err)
	}
	return res
}

func (w *world) load(id string) *manifest.Backup {
	w.t.Helper()
	b, err := manifest.Load(w.repo, id)
	if err != nil {
		w.t.Fatalf("manifest.Load(%s): %v", id, err)
	}
	return b
}

// restoreBytes restores a volume backup through a real volume.Writer and
// returns the bytes on disk — the same door the product's restore uses.
func (w *world) restoreBytes(id string) []byte {
	w.t.Helper()
	data, err := w.tryRestore(id, w.key)
	if err != nil {
		w.t.Fatalf("Restore(%s): %v", id, err)
	}
	return data
}

func (w *world) tryRestore(id string, key *crypto.MasterKey) ([]byte, error) {
	w.t.Helper()
	b := w.load(id)
	idx, cs, err := w.tryOpen(key)
	if err != nil {
		return nil, err
	}
	// Closed HERE, not at test cleanup: a later backup in the same scenario
	// renames a new hash index over the old one, and Windows refuses a
	// rename onto a file this reader still holds open (Unit/windows-latest
	// on the first push of this suite; the §6 cross-OS trap).
	defer func() { cs.Close(); idx.Close() }()
	out := filepath.Join(w.t.TempDir(), "restored.img")
	wr, err := volume.NewWriter(out)
	if err != nil {
		w.t.Fatal(err)
	}
	_, rerr := restore.NewRestorer(idx, cs, quiet()).Restore(context.Background(), b, wr)
	wr.Close()
	if rerr != nil {
		return nil, rerr
	}
	return os.ReadFile(out)
}

func (w *world) packFiles() []string {
	w.t.Helper()
	m, _ := filepath.Glob(filepath.Join(w.repo, "chunks", "*.pack"))
	return m
}

func (w *world) packBytes() int64 {
	var n int64
	for _, p := range w.packFiles() {
		if st, err := os.Stat(p); err == nil {
			n += st.Size()
		}
	}
	return n
}

// requirePacks fences the fixture (§2): a scenario about multi-pack
// behavior is meaningless on a one-pack repository.
func (w *world) requirePacks(min int) {
	w.t.Helper()
	if n := len(w.packFiles()); n < min {
		w.t.Fatalf("fixture defect: only %d pack file(s), the scenario needs at least %d to exercise pack boundaries", n, min)
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// noise returns size bytes with a repeated 64 KB motif every third block,
// so content-defined chunking produces real dedup hits alongside unique
// data — a source that dedups nothing tests nothing about the index.
func noise(seed int64, size int) []byte {
	rng := rand.New(rand.NewSource(seed))
	out := make([]byte, size)
	rng.Read(out)
	motif := make([]byte, 64<<10)
	rand.New(rand.NewSource(seed ^ 0x5eed)).Read(motif)
	for off := 2 * len(motif); off+len(motif) <= size; off += 3 * len(motif) {
		copy(out[off:], motif)
	}
	return out
}
