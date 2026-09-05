// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
)

// The pack-layout generation is how a resumed backup learns its pack
// references are stale after a prune renumbered packs (#55/#56): absent on
// a never-pruned repo, 32 hex characters once bumped, and different after
// every bump.
func TestPacksGenerationChangesOnEveryBump(t *testing.T) {
	repo := t.TempDir()
	if err := InitRepo(repo, RepoConfig{}); err != nil {
		t.Fatal(err)
	}
	if g := PacksGeneration(repo); g != "" {
		t.Fatalf("a fresh repo reports generation %q, want none", g)
	}
	if err := BumpPacksGeneration(repo); err != nil {
		t.Fatal(err)
	}
	g1 := PacksGeneration(repo)
	if len(g1) != 32 {
		t.Fatalf("generation %q is not 32 hex characters", g1)
	}
	if err := BumpPacksGeneration(repo); err != nil {
		t.Fatal(err)
	}
	if g2 := PacksGeneration(repo); g2 == g1 || len(g2) != 32 {
		t.Fatalf("a second bump left the generation at %q (was %q)", g2, g1)
	}
	// Staging: the generation is written INTO a staging chunks dir so the
	// atomic swap installs it together with the renumbered packs.
	staging := filepath.Join(t.TempDir(), "chunks-staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteGenerationFile(staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staging, packsGenerationFile)); err != nil {
		t.Fatalf("no generation file in the staging dir: %v", err)
	}
	if err := WriteGenerationFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("writing a generation into a missing dir returned no error")
	}
}

// RepoExists and SaveRepoConfig are the doors every command opens a repo
// through: a config written by SaveRepoConfig is what LoadRepoConfig reads
// back, atomically (no half-written config on disk).
func TestRepoConfigSavesAtomicallyAndRepoExistsFollowsIt(t *testing.T) {
	repo := t.TempDir()
	if RepoExists(repo) {
		t.Fatal("an empty directory reports as a repo")
	}
	if err := InitRepo(repo, RepoConfig{ChunkAvgSize: 65536}); err != nil {
		t.Fatal(err)
	}
	if !RepoExists(repo) {
		t.Fatal("InitRepo did not make RepoExists true")
	}
	cfg, err := LoadRepoConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Normalizers = []string{"pe-headers"}
	cfg.EncryptionMode = EncryptPassphrase
	if err := SaveRepoConfig(repo, cfg); err != nil {
		t.Fatal(err)
	}
	back, err := LoadRepoConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Normalizers) != 1 || back.Normalizers[0] != "pe-headers" || back.EncryptionMode != EncryptPassphrase || back.ChunkAvgSize != 65536 {
		t.Fatalf("round trip = %+v", back)
	}
	entries, _ := os.ReadDir(repo)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("SaveRepoConfig left a temp file behind: %s", e.Name())
		}
	}
	if err := SaveRepoConfig(filepath.Join(repo, "nope"), cfg); err == nil {
		t.Fatal("saving into a missing repo dir returned no error")
	}
}

// The index-key rule (#265) has one home: a managed repo's index is PLAIN
// (its chunks are encrypted with the DEK, its index deliberately not) and a
// passphrase repo's index is encrypted with the same key as its chunks.
// IndexEncryptedAtRest is the same rule from the reader's side — the
// encryption hint that skips the impossible index variant (#470).
func TestIndexKeyRuleAndItsReadSideTwin(t *testing.T) {
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Destroy()
	// The rule singles out MANAGED: its index key is nil whatever key the
	// chunks use. Every other mode indexes with the chunk key — which for a
	// plain repo is nil to begin with.
	cases := []struct {
		mode       EncryptionMode
		wantKey    bool
		wantAtRest bool
	}{
		{EncryptNone, true, false},
		{EncryptPassphrase, true, true},
		{EncryptManaged, false, false},
	}
	for _, c := range cases {
		rc := RepoConfig{EncryptionMode: c.mode}
		got := IndexKeyFor(rc, key)
		if (got != nil) != c.wantKey {
			t.Errorf("mode %q: IndexKeyFor returned key=%v, want key=%v", c.mode, got != nil, c.wantKey)
		}
		if c.wantKey && got != key {
			t.Errorf("mode %q: the index key is not the chunk key", c.mode)
		}
		if rc.IndexEncryptedAtRest() != c.wantAtRest {
			t.Errorf("mode %q: IndexEncryptedAtRest = %v, want %v", c.mode, rc.IndexEncryptedAtRest(), c.wantAtRest)
		}
	}
	// No key at all (an unencrypted open) never yields an index key.
	if IndexKeyFor(RepoConfig{EncryptionMode: EncryptPassphrase}, nil) != nil {
		t.Fatal("a nil chunk key produced an index key")
	}
}

// The frame cache is the batch pre-fetch's memory: a cached frame is
// visible to HasFrame, served by Retrieve without a pack read, and gone
// after DropFrames for its pack only. FetchBatch is a pass-through to the
// wired fetcher and a clean nil when none is wired.
func TestFrameCacheAndBatchFetchSeams(t *testing.T) {
	repo := t.TempDir()
	if err := InitRepo(repo, RepoConfig{}); err != nil {
		t.Fatal(err)
	}
	cs, err := NewChunkStore(repo, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.CanBatchFetch() {
		t.Fatal("CanBatchFetch is true with no fetcher wired")
	}
	if got, err := cs.FetchBatch([]ChunkRef{{ChunkLoc: ChunkLoc{PackNum: 1}, RawLen: 10}}); got != nil || err != nil {
		t.Fatalf("FetchBatch without a fetcher = %v, %v; want nil, nil", got, err)
	}

	data := []byte("frame cache round trip payload, long enough to matter")
	pack, off, _, err := cs.Store(data)
	if err != nil {
		t.Fatal(err)
	}
	frame, _, err := cs.RetrieveRaw(pack, off)
	if err != nil {
		t.Fatal(err)
	}
	if cs.HasFrame(pack, off) {
		t.Fatal("HasFrame reports a frame nobody cached")
	}
	cs.CacheFrame(pack, off, frame)
	if !cs.HasFrame(pack, off) {
		t.Fatal("HasFrame does not see a cached frame")
	}
	got, err := cs.Retrieve(pack, off)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Retrieve through the frame cache = %q, %v", got, err)
	}
	cs.DropFrames(pack + 1)
	if !cs.HasFrame(pack, off) {
		t.Fatal("DropFrames of another pack evicted this pack's frame")
	}
	cs.DropFrames(pack)
	if cs.HasFrame(pack, off) {
		t.Fatal("DropFrames left the pack's frame cached")
	}

	// A wired fetcher is called with the refs and its answer returned as is.
	var seen []ChunkRef
	want := errors.New("fetcher says no")
	cs.OnChunkFetchBatch = func(refs []ChunkRef) (map[ChunkLoc][]byte, error) { seen = refs; return nil, want }
	if !cs.CanBatchFetch() {
		t.Fatal("CanBatchFetch is false with a fetcher wired")
	}
	refs := []ChunkRef{{ChunkLoc: ChunkLoc{PackNum: pack, StoreOffset: off}, RawLen: len(data)}}
	if _, err := cs.FetchBatch(refs); err != want || len(seen) != 1 || seen[0] != refs[0] {
		t.Fatalf("FetchBatch did not pass the refs through: err=%v seen=%v", err, seen)
	}
	if got, err := cs.FetchBatch(nil); got != nil || err != nil {
		t.Fatalf("FetchBatch with no refs called the fetcher: %v %v", got, err)
	}
}
