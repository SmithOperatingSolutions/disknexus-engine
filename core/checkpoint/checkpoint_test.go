// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package checkpoint_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func sample() *checkpoint.Checkpoint {
	c := &checkpoint.Checkpoint{
		Version:             checkpoint.Version,
		Mode:                "volume",
		BackupID:            "a1b2",
		SourceKind:          "input",
		SourcePath:          "/srv/disk.img",
		TotalSize:           1 << 30,
		SourceMTimeUnixNano: 1234567890,
		Volume:              "vol0",
	}
	c.LastSealedPack = 811
	c.ResumeOffset = 4_200_000_000
	c.EntriesLen = 90
	c.EntriesCount = 2
	c.BoundaryChunkHash[0] = 0xAB
	c.BoundaryChunkOffset = 4_199_000_000
	c.BoundaryChunkLength = 1_000_000
	c.ConfigHash[0] = 0xCD
	c.CreatedUnixNano = 999
	return c
}

func TestWriteLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}
	c := sample()
	if err := checkpoint.Write(dir, c); err != nil {
		t.Fatal(err)
	}
	got, err := checkpoint.Load(dir, "a1b2")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSealedPack != 811 || got.ResumeOffset != 4_200_000_000 || got.EntriesLen != 90 {
		t.Fatalf("progress round-trip mismatch: %+v", got.Progress)
	}
	if got.BoundaryChunkHash != c.BoundaryChunkHash || got.ConfigHash != c.ConfigHash {
		t.Fatal("hash fields did not round-trip")
	}
	if got.SourcePath != "/srv/disk.img" || got.SourceMTimeUnixNano != 1234567890 {
		t.Fatal("identity fields did not round-trip")
	}
}

// TestCRCTamperRejected: a flipped body byte must make Load fail (bad CRC), and
// Find must ignore it as no-valid-checkpoint.
func TestCRCTamperRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Write(dir, sample()); err != nil {
		t.Fatal(err)
	}
	p := checkpoint.Path(dir, "a1b2")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a byte well inside the file (part of the base64/body), leaving JSON parseable.
	raw[len(raw)/2] ^= 0xFF
	if err := os.WriteFile(p, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoint.Load(dir, "a1b2"); err == nil {
		t.Fatal("Load accepted a CRC-corrupted checkpoint")
	}
	got, err := checkpoint.Find(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatal("Find returned a CRC-corrupted checkpoint; must treat as absent")
	}
}

func TestFindExistsRemove(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "manifests"), 0755); err != nil {
		t.Fatal(err)
	}
	if ex, _ := checkpoint.Exists(dir); ex {
		t.Fatal("Exists true on empty repo")
	}
	if got, _ := checkpoint.Find(dir); got != nil {
		t.Fatal("Find non-nil on empty repo")
	}
	if err := checkpoint.Write(dir, sample()); err != nil {
		t.Fatal(err)
	}
	if ex, _ := checkpoint.Exists(dir); !ex {
		t.Fatal("Exists false after Write")
	}
	got, _ := checkpoint.Find(dir)
	if got == nil || got.BackupID != "a1b2" {
		t.Fatalf("Find = %+v", got)
	}
	if err := checkpoint.Remove(dir, "a1b2"); err != nil {
		t.Fatal(err)
	}
	if ex, _ := checkpoint.Exists(dir); ex {
		t.Fatal("Exists true after Remove")
	}
	// Remove of a missing checkpoint is a no-op.
	if err := checkpoint.Remove(dir, "nope"); err != nil {
		t.Fatalf("Remove(missing) = %v", err)
	}
}

func TestVerifyIdentity(t *testing.T) {
	c := sample()
	base := checkpoint.Identity{
		SourceKind:          "input",
		SourcePath:          "/srv/disk.img",
		Volume:              "vol0",
		TotalSize:           1 << 30,
		SourceMTimeUnixNano: 1234567890,
		ConfigHash:          c.ConfigHash,
	}
	if reason := c.VerifyIdentity(base); reason != "" {
		t.Fatalf("matching identity rejected: %s", reason)
	}
	cases := []struct {
		name string
		mut  func(*checkpoint.Identity)
	}{
		{"size", func(id *checkpoint.Identity) { id.TotalSize++ }},
		{"mtime", func(id *checkpoint.Identity) { id.SourceMTimeUnixNano++ }},
		{"path", func(id *checkpoint.Identity) { id.SourcePath = "/other" }},
		{"kind", func(id *checkpoint.Identity) { id.SourceKind = "no-vss" }},
		{"config", func(id *checkpoint.Identity) { id.ConfigHash[1] ^= 0xFF }},
	}
	for _, tc := range cases {
		id := base
		tc.mut(&id)
		if reason := c.VerifyIdentity(id); reason == "" {
			t.Fatalf("%s mismatch not detected", tc.name)
		}
	}
	// mtime is NOT checked for non-input sources.
	dev := sample()
	dev.SourceKind = "no-vss"
	idNoVSS := base
	idNoVSS.SourceKind = "no-vss"
	idNoVSS.SourceMTimeUnixNano = 999999 // differs, must be ignored
	if reason := dev.VerifyIdentity(idNoVSS); reason != "" {
		t.Fatalf("no-vss mtime should be ignored, got: %s", reason)
	}
}

// TestConfigHashSensitivity: every setting that changes chunk boundaries or
// chunk identity must change the hash, so a resume against a re-configured
// repo is refused.
//
// This is the counterweight to the agreement tests (#262). Making two
// checkpoint writers hash the same is trivially achievable by hashing less —
// and that "fix" would pass every agreement test while quietly removing the
// protection resume depends on, letting a backup continue across a geometry
// or encryption change and splicing incompatible chunks into one manifest.
// Each mutation below is a change that must be caught.
func TestConfigHashSensitivity(t *testing.T) {
	rc := store.RepoConfig{
		ChunkMinSize: 4096, ChunkAvgSize: 8192, ChunkMaxSize: 65536,
		BuzhashMask: 0x1FFF, PackFileMaxSize: 1 << 20, CompressionLevel: 3,
		EncryptionMode: store.EncryptPassphrase, Normalizers: []string{"pe", "ntfs"},
	}
	base := checkpoint.ConfigHash(rc)
	for _, tc := range []struct {
		name string
		mut  func(*store.RepoConfig)
	}{
		{"chunk-min", func(c *store.RepoConfig) { c.ChunkMinSize++ }},
		{"chunk-avg", func(c *store.RepoConfig) { c.ChunkAvgSize++ }},
		{"chunk-max", func(c *store.RepoConfig) { c.ChunkMaxSize++ }},
		{"buzhash-mask", func(c *store.RepoConfig) { c.BuzhashMask++ }},
		{"pack-size", func(c *store.RepoConfig) { c.PackFileMaxSize++ }},
		{"compression-level", func(c *store.RepoConfig) { c.CompressionLevel++ }},
		{"encryption-mode", func(c *store.RepoConfig) { c.EncryptionMode = store.EncryptManaged }},
		{"encryption-off", func(c *store.RepoConfig) { c.EncryptionMode = store.EncryptNone }},
		{"normalizer-added", func(c *store.RepoConfig) { c.Normalizers = []string{"pe", "ntfs", "x"} }},
		{"normalizer-removed", func(c *store.RepoConfig) { c.Normalizers = []string{"pe"} }},
		{"normalizer-order", func(c *store.RepoConfig) { c.Normalizers = []string{"ntfs", "pe"} }},
		{"normalizers-cleared", func(c *store.RepoConfig) { c.Normalizers = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc2 := rc
			tc.mut(&rc2)
			if checkpoint.ConfigHash(rc2) == base {
				t.Fatalf("ConfigHash insensitive to %s — a resume across this change would be allowed", tc.name)
			}
		})
	}
	// Stable for identical config.
	if checkpoint.ConfigHash(rc) != base {
		t.Fatal("ConfigHash not deterministic")
	}
	// The Encrypted bool is the v1 spelling of "passphrase": a repo that
	// carries only the bool must hash as the repo that carries only the mode,
	// or a checkpoint written against one config.json is refused after an
	// unrelated rewrite of the same repo's config.
	v1 := rc
	v1.EncryptionMode = store.EncryptNone
	v1.Encrypted = true
	if checkpoint.ConfigHash(v1) != base {
		t.Fatal("ConfigHash treats the v1 Encrypted bool as a different repo than encryption_mode=passphrase")
	}
	// Effective is idempotent: hashing an already-resolved config must give
	// the same answer as hashing the stored form it came from.
	if checkpoint.ConfigHash(rc.Effective()) != base {
		t.Fatal("ConfigHash not idempotent under Effective")
	}
}
