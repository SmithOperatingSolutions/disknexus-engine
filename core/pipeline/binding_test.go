// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store/repocfgcases"
)

func testKey(t *testing.T) *crypto.MasterKey {
	t.Helper()
	k, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(k.Destroy)
	return k
}

// TestBindRefusesEncryptedRepoWithoutKey is the refusal #265 calls
// load-bearing, at the level where it is a pure function.
//
// Unit level by construction: Bind takes a store.RepoConfig and a key and
// returns an error. Nothing about the property needs a repo, a network, or a
// job, so testing it anywhere higher would only make the diagnostic worse.
func TestBindRefusesEncryptedRepoWithoutKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		rc   store.RepoConfig
		key  *crypto.MasterKey
		want error
		says string
	}{
		{
			name: "managed-without-key",
			rc:   store.RepoConfig{EncryptionMode: store.EncryptManaged},
			want: ErrKeyRequired,
			says: "managed",
		},
		{
			name: "passphrase-without-key",
			rc:   store.RepoConfig{EncryptionMode: store.EncryptPassphrase},
			want: ErrKeyRequired,
			says: "passphrase",
		},
		{
			// A v1 blob that carries only the Encrypted bool resolves to
			// passphrase. A Bind that switched on EncryptionMode instead of
			// EffectiveEncryptionMode would treat it as unencrypted.
			name: "legacy-encrypted-bool-without-key",
			rc:   store.RepoConfig{Encrypted: true},
			want: ErrKeyRequired,
			says: "passphrase",
		},
		{
			// The controller stores encryption_mode verbatim with no
			// validation, so a typo creates a repo the record calls
			// encrypted and for which no key can ever exist. It must fail
			// the backup, never fall through to plaintext.
			name: "unknown-mode-without-key",
			rc:   store.RepoConfig{EncryptionMode: store.EncryptionMode("manged")},
			want: ErrUnknownEncryptionMode,
			says: "manged",
		},
		{
			// ...and it must stay a refusal even when a key IS somehow in
			// hand: nobody knows how those bytes are meant to be used.
			name: "unknown-mode-with-key",
			rc:   store.RepoConfig{EncryptionMode: store.EncryptionMode("Managed")},
			key:  testKey(t),
			want: ErrUnknownEncryptionMode,
			says: "Managed",
		},
		{
			// The inverse mistake: a key resolved for a repo the config says
			// is unencrypted means the mode came from the wrong source.
			// Proceeding would write ciphertext into a repo every reader
			// opens with no key.
			name: "none-with-key",
			rc:   store.RepoConfig{},
			key:  testKey(t),
			want: ErrUnexpectedKey,
			says: "none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Bind(tc.rc, tc.key)
			if err == nil {
				t.Fatalf("Bind(%+v, key=%v) succeeded — an agent that cannot encrypt must refuse, not proceed (#265)",
					tc.rc, tc.key != nil)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error %v does not wrap %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error %q does not name %q — a refusal must say what it could not do", err, tc.says)
			}
			if b != (Binding{}) {
				t.Error("a failed Bind returned a non-zero Binding")
			}
		})
	}
}

// TestBindAcceptsTheLegitimateCases guards the other half: the refusal must
// not be achieved by refusing everything.
func TestBindAcceptsTheLegitimateCases(t *testing.T) {
	k := testKey(t)
	for _, tc := range []struct {
		name string
		rc   store.RepoConfig
		key  *crypto.MasterKey
	}{
		{"none-without-key", store.RepoConfig{}, nil},
		{"managed-with-key", store.RepoConfig{EncryptionMode: store.EncryptManaged}, k},
		{"passphrase-with-key", store.RepoConfig{EncryptionMode: store.EncryptPassphrase}, k},
		{"legacy-encrypted-bool-with-key", store.RepoConfig{Encrypted: true}, k},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Bind(tc.rc, tc.key)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if b.Key() != tc.key {
				t.Errorf("Key() = %v, want the key passed in", b.Key())
			}
			if b.Mode() != tc.rc.EffectiveEncryptionMode() {
				t.Errorf("Mode() = %q, want %q", b.Mode(), tc.rc.EffectiveEncryptionMode())
			}
		})
	}
}

// TestBindIndexKeyIsNilOnlyForManaged pins the rule the old variadic
// constructor made easy to violate: pipeline.New(cfg, logger, dek) with ONE
// key defaulted indexKey to key, which under managed mode would encrypt the
// dedup index — contradicting the CLI, the controller's Web Restore, and every
// other reader, all of which open a managed repo's index with a nil key.
//
// After #265 that call does not compile; this states the rule that replaced it.
func TestBindIndexKeyIsNilOnlyForManaged(t *testing.T) {
	k := testKey(t)
	for _, tc := range []struct {
		name         string
		rc           store.RepoConfig
		key          *crypto.MasterKey
		wantIndexKey *crypto.MasterKey
	}{
		{"managed", store.RepoConfig{EncryptionMode: store.EncryptManaged}, k, nil},
		{"passphrase", store.RepoConfig{EncryptionMode: store.EncryptPassphrase}, k, k},
		{"legacy-encrypted-bool", store.RepoConfig{Encrypted: true}, k, k},
		{"none", store.RepoConfig{}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Bind(tc.rc, tc.key)
			if err != nil {
				t.Fatal(err)
			}
			if b.IndexKey() != tc.wantIndexKey {
				t.Errorf("IndexKey() = %v, want %v — the managed index is written in the clear on purpose "+
					"(the controller opens it with a nil key for server-side restore)", b.IndexKey(), tc.wantIndexKey)
			}
		})
	}
}

// TestBindDerivesNormalizerFromRepoConfig is driven over the shared corpus, so
// it cannot be satisfied by dropping the field: the corpus's normalizer
// expectations are hand-written and shared with every other reader's test.
//
// #265's second half: the agent never called SetNormalizer, and neither did
// any of the CLI's own cloud write paths, so chunk hashes were computed on
// un-normalized bytes while every cloud READ path applied the normalizer.
func TestBindDerivesNormalizerFromRepoConfig(t *testing.T) {
	k := testKey(t)
	for _, c := range repocfgcases.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			rc := c.WantRepoConfig()
			var key *crypto.MasterKey
			switch rc.EffectiveEncryptionMode() {
			case store.EncryptNone:
			case store.EncryptManaged, store.EncryptPassphrase:
				key = k
			default:
				// The unknown-mode case is a refusal, covered above.
				if _, err := Bind(rc, nil); err == nil {
					t.Fatalf("Bind accepted unknown mode %q", rc.EffectiveEncryptionMode())
				}
				return
			}
			b, err := Bind(rc, key)
			if err != nil {
				t.Fatalf("Bind: %v", err)
			}
			got := preprocess.Names(b.Normalizer())
			if !sameNames(got, c.WantNormalizers) {
				t.Errorf("normalizer names = %v, want %v", got, c.WantNormalizers)
			}
		})
	}
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestBindRejectsUnknownNormalizerNames: a normalizer list a reader cannot
// build is as fatal as a missing key — chunk identity would be computed on the
// wrong bytes for the rest of the repo's life.
func TestBindRejectsUnknownNormalizerNames(t *testing.T) {
	_, err := Bind(store.RepoConfig{Normalizers: []string{"no-such-normalizer"}}, nil)
	if err == nil {
		t.Fatal("Bind accepted an unknown normalizer name")
	}
	if !strings.Contains(err.Error(), "no-such-normalizer") {
		t.Errorf("error %q does not name the offending normalizer", err)
	}
}

// TestUnboundPipelineRefusesToRunAndWritesNothing closes the one hole the
// constructor cannot: Pipeline has exported fields, so a struct literal
// compiles from any package. The check in run() must fire before the chunker,
// before any file is created.
func TestUnboundPipelineRefusesToRunAndWritesNothing(t *testing.T) {
	repo := t.TempDir()
	p := &Pipeline{cfg: config.Default(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, err := p.Backup(context.Background(), bytes.NewReader(bytes.Repeat([]byte("x"), 1<<16)), "src", 1<<16, repo)
	if err == nil {
		t.Fatal("an unbound pipeline ran a backup")
	}
	if !errors.Is(err, ErrUnbound) {
		t.Errorf("error %v does not wrap ErrUnbound", err)
	}
	ents, rerr := os.ReadDir(repo)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("an unbound pipeline created %v in the repo before refusing", names)
	}
}

// TestKeyedPipelineWritesCiphertext fills a real hole: before #265 every one
// of this package's test files constructed pipelines with a logger only, so
// there was ZERO coverage that a keyed pipeline produces ciphertext.
//
// It asserts on the WRITTEN BYTES — the unit-level twin of the integration
// test that does the same against real S3.
func TestKeyedPipelineWritesCiphertext(t *testing.T) {
	key := testKey(t)
	repo := t.TempDir()
	cfg := config.Default()
	if err := store.InitRepo(repo, store.RepoConfig{EncryptionMode: store.EncryptManaged}); err != nil {
		t.Fatal(err)
	}

	needle := []byte("PLAINTEXT-CANARY-265")
	data := bytes.Repeat(append(needle, bytes.Repeat([]byte("q"), 44)...), 4000)

	b, err := Bind(store.RepoConfig{EncryptionMode: store.EncryptManaged}, key)
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), b)
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "src", int64(len(data)), repo); err != nil {
		t.Fatalf("backup: %v", err)
	}

	packs, _ := filepath.Glob(filepath.Join(repo, "chunks", "*.pack"))
	if len(packs) == 0 {
		t.Fatal("no pack files were written — the assertion below would be vacuous")
	}
	// zstd frame magic 0xFD2FB528, little-endian on the wire.
	zstdMagic := []byte{0x28, 0xB5, 0x2F, 0xFD}
	for _, pf := range packs {
		body, err := os.ReadFile(pf)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, needle) {
			t.Errorf("%s contains the plaintext needle verbatim — the chunk was not encrypted (#265)", pf)
		}
		if bytes.Contains(body, zstdMagic) {
			t.Errorf("%s contains raw zstd frame magic — the payload is compressed-but-unencrypted (#265)", pf)
		}
	}
}

// TestManagedPipelineLeavesTheIndexReadable is the write-side half of the
// index-key rule: a managed repo's bloom and hash index must stay openable
// with a NIL key, because that is how the controller's server-side restore
// opens them. Writing them under the DEK would rename them to .enc and leave
// every other command staring at an empty index.
func TestManagedPipelineLeavesTheIndexReadable(t *testing.T) {
	key := testKey(t)
	repo := t.TempDir()
	cfg := config.Default()
	if err := store.InitRepo(repo, store.RepoConfig{EncryptionMode: store.EncryptManaged}); err != nil {
		t.Fatal(err)
	}
	b, err := Bind(store.RepoConfig{EncryptionMode: store.EncryptManaged}, key)
	if err != nil {
		t.Fatal(err)
	}
	p := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), b)
	data := bytes.Repeat([]byte("INDEXED-265-"), 20000)
	if _, err := p.Backup(context.Background(), bytes.NewReader(data), "src", int64(len(data)), repo); err != nil {
		t.Fatalf("backup: %v", err)
	}
	for _, name := range []string{"bloom.bin", "hash-index.db"} {
		if _, err := os.Stat(filepath.Join(repo, "index", name)); err != nil {
			t.Errorf("managed repo has no plaintext %s: %v — the index was encrypted under the DEK, "+
				"which the controller's Web Restore opens with a nil key (#265)", name, err)
		}
		if _, err := os.Stat(filepath.Join(repo, "index", name+".enc")); err == nil {
			t.Errorf("managed repo wrote %s.enc — the index must stay in the clear", name)
		}
	}
}
