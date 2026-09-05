// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestExistingChunksReadBackAtAnyCompressionLevel: #259 changes what
// compression level a repo with unset fields writes at from now on, which is
// only safe if the level is a write-side knob and nothing else. Chunks written
// at every level must read back byte-identical through a store opened at any
// other level — including packs that already mix levels, which is exactly what
// a repo touched by both the old and the new resolution ends up holding.
func TestExistingChunksReadBackAtAnyCompressionLevel(t *testing.T) {
	levels := []int{0, 1, 3, 6, 9}

	// Payloads compressible enough that the level genuinely changes the
	// bytes on disk; random data would be a no-op at every level.
	payload := func(seed byte) []byte {
		b := make([]byte, 96*1024)
		for i := range b {
			b[i] = byte(i%7) ^ byte(i/1024%13) ^ seed
		}
		return b
	}

	dir := t.TempDir()
	type loc struct {
		pack   uint32
		offset int64
		want   []byte
	}
	var written []loc
	var sizes []int

	// Write one chunk per level into the SAME repo, reopening the store at
	// each level so the packs end up holding frames from several levels.
	for _, lvl := range levels {
		cs, err := store.NewChunkStore(dir, config.DefaultPackFileMaxSize, lvl)
		if err != nil {
			t.Fatalf("NewChunkStore(level %d): %v", lvl, err)
		}
		data := payload(byte(lvl))
		pack, off, n, err := cs.Store(data)
		if err != nil {
			t.Fatalf("Store(level %d): %v", lvl, err)
		}
		if err := cs.Close(); err != nil {
			t.Fatalf("Close(level %d): %v", lvl, err)
		}
		written = append(written, loc{pack, off, data})
		sizes = append(sizes, n)
	}

	// Sanity: the levels really did produce different bytes, or this test
	// would prove nothing about mixed-level packs.
	if sizes[0] == sizes[3] {
		t.Fatalf("test is blind: levels %d and %d stored identical byte counts (%d)",
			levels[0], levels[3], sizes[0])
	}

	// Every reader level must retrieve every chunk exactly.
	for _, readLvl := range levels {
		cs, err := store.NewChunkStore(dir, config.DefaultPackFileMaxSize, readLvl)
		if err != nil {
			t.Fatalf("reopen at level %d: %v", readLvl, err)
		}
		for i, l := range written {
			got, err := cs.Retrieve(l.pack, l.offset)
			if err != nil {
				t.Fatalf("Retrieve chunk %d (written at level %d) with a level-%d store: %v",
					i, levels[i], readLvl, err)
			}
			if !bytes.Equal(got, l.want) {
				t.Errorf("chunk %d (written at level %d) read back wrong through a level-%d store",
					i, levels[i], readLvl)
			}
		}
		cs.Close()
	}
}

// TestRepoConfigEffective covers the resolution rule itself: a zero storage
// field is "not persisted" and falls back to the default; anything explicit
// survives untouched.
func TestRepoConfigEffective(t *testing.T) {
	full := store.RepoConfig{
		Version: 1, ChunkMinSize: 4096, ChunkAvgSize: 8192, ChunkMaxSize: 65536,
		BuzhashMask: 8191, PackFileMaxSize: 1 << 20, CompressionLevel: 9,
		Normalizers: []string{"ntfs"},
	}
	if got := full.Effective(); got.ChunkMinSize != 4096 || got.ChunkAvgSize != 8192 ||
		got.ChunkMaxSize != 65536 || got.BuzhashMask != 8191 ||
		got.PackFileMaxSize != 1<<20 || got.CompressionLevel != 9 {
		t.Fatalf("explicit values were not preserved: %+v", got)
	}

	empty := store.RepoConfig{}.Effective()
	if empty.ChunkMinSize != config.DefaultChunkMinSize ||
		empty.ChunkAvgSize != config.DefaultChunkAvgSize ||
		empty.ChunkMaxSize != config.DefaultChunkMaxSize ||
		empty.BuzhashMask != config.DefaultBuzhashMask ||
		empty.PackFileMaxSize != config.DefaultPackFileMaxSize ||
		empty.CompressionLevel != config.DefaultCompressionLevel {
		t.Fatalf("unset fields did not fall back to defaults: %+v", empty)
	}

	// Resolution must be idempotent: the agent hands ConfigHash a config
	// rebuilt from an already-resolved config.Config, the CLI hands it the
	// raw stored one, and both must land on the same values.
	if again := empty.Effective(); !reflect.DeepEqual(again, empty) {
		t.Fatalf("Effective is not idempotent: %+v vs %+v", again, empty)
	}

	// Encryption and normalizers are not storage geometry and must not be
	// touched: an empty normalizer list is a legitimate value, not an unset
	// one, and inventing a default there would change chunk identity.
	if len(store.RepoConfig{}.Effective().Normalizers) != 0 {
		t.Fatal("Effective invented a normalizer")
	}
	if full.Effective().EffectiveEncryptionMode() != store.EncryptNone {
		t.Fatal("Effective changed the encryption mode")
	}
	if n := full.Effective().Normalizers; len(n) != 1 || n[0] != "ntfs" {
		t.Fatalf("Effective dropped the recorded normalizers: %v", n)
	}
}

// TestRepoConfigApplyToLeavesUnrelatedFieldsAlone: ApplyTo overlays storage
// geometry onto a config that also carries worker counts, buffer sizes and
// index tuning. Those are process-level, not repo-level, and a repo config
// must not reach them.
func TestRepoConfigApplyToLeavesUnrelatedFieldsAlone(t *testing.T) {
	cfg := config.Default()
	cfg.HashWorkers = 3
	cfg.MemoryBudgetMB = 77
	cfg.ReadBufferSize = 4096
	cfg.IndexCacheMB = 11
	cfg.BloomFPRate = 0.5
	cfg.RepoPath = "/somewhere"
	cfg.NormalizeNTFS = true
	cfg.ExcludeVolatileFiles = false
	before := cfg

	store.RepoConfig{ChunkAvgSize: 8192, CompressionLevel: 6}.ApplyTo(&cfg)

	if cfg.HashWorkers != before.HashWorkers || cfg.MemoryBudgetMB != before.MemoryBudgetMB ||
		cfg.ReadBufferSize != before.ReadBufferSize || cfg.IndexCacheMB != before.IndexCacheMB ||
		cfg.BloomFPRate != before.BloomFPRate || cfg.RepoPath != before.RepoPath ||
		cfg.NormalizeNTFS != before.NormalizeNTFS || cfg.ExcludeVolatileFiles != before.ExcludeVolatileFiles {
		t.Fatalf("ApplyTo touched non-storage config:\n before %+v\n after  %+v", before, cfg)
	}
	if cfg.ChunkAvgSize != 8192 || cfg.CompressionLevel != 6 {
		t.Fatalf("ApplyTo did not apply the explicit values: %+v", cfg)
	}
}

// TestRepoConfigFromRecord pins the contract every reader of a controller repo
// record now depends on (#262): the CLI's cloud reader, the agent's cloud
// reader and the controller's own restore-zip reader all go through this one
// function, so what it decides is what they all decide.
func TestRepoConfigFromRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configJSON string
		recordMode string
		wantMode   store.EncryptionMode
		wantAvg    int
		wantNorm   []string
	}{
		{
			name:     "no blob at all resolves to defaults, not an error",
			wantMode: store.EncryptNone,
			wantAvg:  config.DefaultChunkAvgSize,
		},
		{
			name:       "empty blob is the same as no blob",
			configJSON: `{}`,
			wantMode:   store.EncryptNone,
			wantAvg:    config.DefaultChunkAvgSize,
		},
		{
			name:       "geometry and normalizers come from the blob",
			configJSON: `{"chunk_avg_size":131072,"normalizers":["pe","ntfs"]}`,
			wantMode:   store.EncryptNone,
			wantAvg:    131072,
			wantNorm:   []string{"pe", "ntfs"},
		},
		{
			name:       "the record column is authoritative when the blob agrees",
			configJSON: `{"chunk_avg_size":8192,"encryption_mode":"managed"}`,
			recordMode: "managed",
			wantMode:   store.EncryptManaged,
			wantAvg:    8192,
		},
		{
			// The shape the divergence would take: a blob written before the
			// mode was mirrored into it. The column decides.
			name:       "the record column is authoritative when the blob is silent",
			configJSON: `{"chunk_avg_size":8192}`,
			recordMode: "managed",
			wantMode:   store.EncryptManaged,
			wantAvg:    8192,
		},
		{
			// The v1 spelling: only the bool, no mode anywhere.
			name:       "the legacy Encrypted bool still resolves to passphrase",
			configJSON: `{"chunk_avg_size":8192,"encrypted":true}`,
			wantMode:   store.EncryptPassphrase,
			wantAvg:    8192,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := store.RepoConfigFromRecord([]byte(tc.configJSON), tc.recordMode)
			if err != nil {
				t.Fatalf("RepoConfigFromRecord: %v", err)
			}
			if got := rc.EffectiveEncryptionMode(); got != tc.wantMode {
				t.Errorf("effective encryption mode = %q, want %q", got, tc.wantMode)
			}
			if got := rc.Effective().ChunkAvgSize; got != tc.wantAvg {
				t.Errorf("effective chunk_avg_size = %d, want %d", got, tc.wantAvg)
			}
			if !reflect.DeepEqual(rc.Normalizers, tc.wantNorm) {
				t.Errorf("normalizers = %v, want %v", rc.Normalizers, tc.wantNorm)
			}
		})
	}

	// A blob that will not parse is an error on every path — falling back to
	// the defaults would rechunk the repo at a geometry that is not its own.
	if _, err := store.RepoConfigFromRecord([]byte(`{"chunk_avg_size":`), ""); err == nil {
		t.Error("malformed config blob accepted")
	}
}
