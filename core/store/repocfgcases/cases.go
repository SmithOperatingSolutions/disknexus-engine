// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package repocfgcases is the shared corpus behind the repo-config reader
// agreement tests (#259).
//
// A stored repo config is read by several independent code paths — the CLI's
// cloud path, the CLI's local path, the agent's cloud path, the agent's local
// path — and each one turns a store.RepoConfig into the config.Config the
// write pipeline actually runs with. #259 happened because two of those
// readers disagreed about what a stored compression_level of 0 meant (the CLI
// treated it as unset and used the default 3, the agent applied it literally
// as zstd SpeedFastest) and no test compared them.
//
// The expectations here are hand-written literals, deliberately NOT derived
// from any reader's implementation: each reader's test drives the real reader
// over this corpus and asserts the same answers. A new reader that diverges
// fails these tests instead of silently becoming a fifth interpretation.
//
// #262 widened the corpus past the geometry. The resume identity hash
// (checkpoint.ConfigHash) covers the encryption mode and the normalizer list
// as well, and the agent's cloud reader used to drop both on the floor — so a
// backup suspended by the CLI on a managed-encryption or normalized repo was
// refused by the agent and silently restarted from zero. WantRepoConfig is
// therefore the full resolved repo config, not just the geometry, and every
// reader that feeds the resume hash is driven over it.
package repocfgcases

import (
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Want is the effective storage geometry a reader must produce for a case.
type Want struct {
	ChunkMinSize     int
	ChunkAvgSize     int
	ChunkMaxSize     int
	BuzhashMask      uint64
	PackFileMaxSize  int64
	CompressionLevel int
}

// Case is one stored repo config plus the effective config every reader must
// derive from it.
type Case struct {
	// Name identifies the case in test output.
	Name string
	// StoredJSON is the repo config exactly as it is persisted — in
	// config.json for a local repo, in the controller's repo record for a
	// cloud repo.
	StoredJSON string
	// Absent means the repo has no stored config at all (the controller
	// returns no "config" key / the field is nil).
	Absent bool
	// RecordEncryptionMode is the cloud repo record's own encryption_mode
	// column, returned by GET /api/repos/{id} alongside the config blob.
	// The controller's create handler writes the mode to both places, but
	// the column is the authoritative one, so a cloud reader must overlay it
	// onto whatever the blob says. Empty for a local repo (which has no
	// record — the mode lives only in config.json).
	RecordEncryptionMode string
	// CloudOnly marks a case that only describes a cloud repo RECORD — the
	// blob and the encryption_mode column say different things, which a
	// local repo (one config.json, no record) cannot express. Local reader
	// tests skip it; the cloud readers must still agree on it.
	CloudOnly bool
	// LocalReaderRefuses marks a case the agent's keyless local-repo reader
	// cannot open at all: the shared local door (captureflow.OpenLocal, #333
	// stage 3) refuses any EFFECTIVELY encrypted repo without key material,
	// because jobs never prompt. Before the door this gate was the legacy
	// Encrypted bool, so a config whose mode said "passphrase" while the
	// bool said false slipped keyless past repoAccess; the door decides by
	// EffectiveEncryptionMode — the same resolution pipeline.Bind enforces.
	// The refusals themselves are tested elsewhere; the effective-geometry
	// assertion is simply unreachable for these cases.
	LocalReaderRefuses bool
	// Want is the effective config every reader must produce.
	Want Want
	// WantEncryptionMode is the effective encryption mode
	// (store.RepoConfig.EffectiveEncryptionMode) every reader must derive,
	// as a plain string so the corpus stays declarative.
	WantEncryptionMode string
	// WantNormalizers is the normalizer list every reader must carry
	// through. Nil and empty are the same value here (no normalization);
	// unlike the geometry, an empty list is never "unset".
	WantNormalizers []string
}

// WantRepoConfig is the fully resolved repo config every reader of this case
// must produce — the Want geometry plus the two identity-bearing settings the
// resume hash also covers. Callers hash it with checkpoint.ConfigHash to get
// the resume identity hash every checkpoint writer must agree on (#262).
func (c Case) WantRepoConfig() store.RepoConfig {
	return store.RepoConfig{
		Version:          1,
		ChunkMinSize:     c.Want.ChunkMinSize,
		ChunkAvgSize:     c.Want.ChunkAvgSize,
		ChunkMaxSize:     c.Want.ChunkMaxSize,
		BuzhashMask:      c.Want.BuzhashMask,
		PackFileMaxSize:  c.Want.PackFileMaxSize,
		CompressionLevel: c.Want.CompressionLevel,
		EncryptionMode:   store.EncryptionMode(c.WantEncryptionMode),
		Normalizers:      c.WantNormalizers,
	}
}

// defaults is the effective config for a repo that persisted nothing.
var defaults = Want{
	ChunkMinSize:     config.DefaultChunkMinSize,
	ChunkAvgSize:     config.DefaultChunkAvgSize,
	ChunkMaxSize:     config.DefaultChunkMaxSize,
	BuzhashMask:      config.DefaultBuzhashMask,
	PackFileMaxSize:  config.DefaultPackFileMaxSize,
	CompressionLevel: config.DefaultCompressionLevel,
}

// fullGeometry is a complete, well-formed geometry: every field explicitly
// non-zero, so every reader must apply it verbatim.
const fullGeometry = `"chunk_min_size": 4096, "chunk_avg_size": 8192, "chunk_max_size": 65536, ` +
	`"buzhash_mask": 8191, "pack_file_max_size": 536870912`

func withFullGeometry(level int) Want {
	return Want{
		ChunkMinSize:     4096,
		ChunkAvgSize:     8192,
		ChunkMaxSize:     65536,
		BuzhashMask:      8191,
		PackFileMaxSize:  536870912,
		CompressionLevel: level,
	}
}

// Cases returns the corpus. Every reader of a stored repo config must map
// each StoredJSON to the matching Want.
func Cases() []Case {
	return []Case{
		{
			// A repo the controller has no config row for at all.
			Name:   "config-absent",
			Absent: true,
			Want:   defaults,
		},
		{
			// A config that persisted no fields: every value is unset.
			Name:       "empty-object",
			StoredJSON: `{}`,
			Want:       defaults,
		},
		{
			// #259 itself: every cloud repo created before #257 stored
			// compression_level 0 because the create handler copied an
			// omitted request field straight through.
			Name:       "legacy-zero-compression",
			StoredJSON: `{"version": 1, ` + fullGeometry + `, "compression_level": 0}`,
			Want:       withFullGeometry(config.DefaultCompressionLevel),
		},
		{
			// The field is missing rather than explicitly 0 — the same
			// zero value arrives, and must be read the same way.
			Name:       "compression-field-omitted",
			StoredJSON: `{"version": 1, ` + fullGeometry + `}`,
			Want:       withFullGeometry(config.DefaultCompressionLevel),
		},
		// The four levels #257 offers. Each is explicit and must survive
		// verbatim on every path.
		{
			Name:       "compression-1",
			StoredJSON: `{"version": 1, ` + fullGeometry + `, "compression_level": 1}`,
			Want:       withFullGeometry(1),
		},
		{
			Name:       "compression-3",
			StoredJSON: `{"version": 1, ` + fullGeometry + `, "compression_level": 3}`,
			Want:       withFullGeometry(3),
		},
		{
			Name:       "compression-6",
			StoredJSON: `{"version": 1, ` + fullGeometry + `, "compression_level": 6}`,
			Want:       withFullGeometry(6),
		},
		{
			Name:       "compression-9",
			StoredJSON: `{"version": 1, ` + fullGeometry + `, "compression_level": 9}`,
			Want:       withFullGeometry(9),
		},
		{
			// Compression chosen, geometry left unset: the geometry must
			// fall back to the defaults, not to zeros. A reader gated on
			// "chunk_avg_size > 0" drops the compression choice here.
			Name:       "compression-only-no-geometry",
			StoredJSON: `{"version": 1, "compression_level": 6}`,
			Want: Want{
				ChunkMinSize:     config.DefaultChunkMinSize,
				ChunkAvgSize:     config.DefaultChunkAvgSize,
				ChunkMaxSize:     config.DefaultChunkMaxSize,
				BuzhashMask:      config.DefaultBuzhashMask,
				PackFileMaxSize:  config.DefaultPackFileMaxSize,
				CompressionLevel: 6,
			},
		},
		{
			// Partial geometry: chunk sizes persisted, mask and pack size
			// not. Applying the zeros literally would put the chunker on a
			// mask of 0 (a boundary at every byte) and packs with no size
			// bound — the same ambiguity as compression, with teeth.
			Name:       "partial-geometry",
			StoredJSON: `{"version": 1, "chunk_min_size": 32768, "chunk_avg_size": 131072, "chunk_max_size": 1048576, "compression_level": 3}`,
			Want: Want{
				ChunkMinSize:     32768,
				ChunkAvgSize:     131072,
				ChunkMaxSize:     1048576,
				BuzhashMask:      config.DefaultBuzhashMask,
				PackFileMaxSize:  config.DefaultPackFileMaxSize,
				CompressionLevel: 3,
			},
		},
		{
			// A fully-specified "coarse" repo: the everyday healthy case.
			// Nothing about it may change.
			Name: "coarse-profile-complete",
			StoredJSON: `{"version": 1, "chunk_min_size": 32768, "chunk_avg_size": 131072, "chunk_max_size": 1048576, ` +
				`"buzhash_mask": 131071, "pack_file_max_size": 268435456, "compression_level": 9}`,
			Want: Want{
				ChunkMinSize:     32768,
				ChunkAvgSize:     131072,
				ChunkMaxSize:     1048576,
				BuzhashMask:      131071,
				PackFileMaxSize:  268435456,
				CompressionLevel: 9,
			},
		},

		// --- #262: settings beyond the geometry that the resume identity
		// hash also covers. The agent's cloud reader dropped every one of
		// these, so a checkpoint written by one path was refused by the
		// other on exactly these repos.
		{
			// Managed encryption: the controller writes the mode to the
			// record column AND into the config blob at create time.
			//
			// Load-bearing pair, do not delete as redundant: this case and
			// "normalizers-explicitly-empty" share fullGeometry and
			// compression 3 and differ ONLY in encryption mode, so they are
			// what proves checkpoint.ConfigHash covers the mode
			// (TestResumeStillRefusesAReconfiguredRepo's pairwise check).
			// The passphrase-vs-managed pair cannot do that job — the agent
			// cloud session refuses passphrase outright (#265), so those
			// cases are excluded from the cross-product.
			Name: "managed-encryption",
			StoredJSON: `{"version": 1, "encryption_mode": "managed", ` + fullGeometry +
				`, "compression_level": 3}`,
			RecordEncryptionMode: "managed",
			LocalReaderRefuses:   true,
			Want:                 withFullGeometry(3),
			WantEncryptionMode:   "managed",
		},
		{
			// Passphrase encryption, same shape.
			Name: "passphrase-encryption",
			StoredJSON: `{"version": 1, "encryption_mode": "passphrase", ` + fullGeometry +
				`, "compression_level": 3}`,
			RecordEncryptionMode: "passphrase",
			LocalReaderRefuses:   true,
			Want:                 withFullGeometry(3),
			WantEncryptionMode:   "passphrase",
		},
		{
			// A v1 config that only has the Encrypted bool and no mode, with
			// the record column empty too: EffectiveEncryptionMode resolves
			// it to passphrase, and a reader that drops the bool resolves it
			// to none.
			Name: "legacy-encrypted-bool",
			StoredJSON: `{"version": 1, "encrypted": true, ` + fullGeometry +
				`, "compression_level": 3}`,
			LocalReaderRefuses: true,
			Want:               withFullGeometry(3),
			WantEncryptionMode: "passphrase",
		},
		{
			// The record column is authoritative: a repo whose blob predates
			// the mode being written into it still reads as managed.
			Name:                 "encryption-mode-only-on-record",
			StoredJSON:           `{"version": 1, ` + fullGeometry + `, "compression_level": 3}`,
			RecordEncryptionMode: "managed",
			CloudOnly:            true,
			Want:                 withFullGeometry(3),
			WantEncryptionMode:   "managed",
		},
		{
			// One normalizer. Chunk identity is the hash of normalized
			// bytes, so this is as boundary-affecting as the geometry.
			Name: "normalizer-pe",
			StoredJSON: `{"version": 1, ` + fullGeometry +
				`, "compression_level": 3, "normalizers": ["pe"]}`,
			Want:            withFullGeometry(3),
			WantNormalizers: []string{"pe"},
		},
		{
			// Two normalizers: order is part of the identity.
			Name: "normalizers-pe-ntfs",
			StoredJSON: `{"version": 1, ` + fullGeometry +
				`, "compression_level": 3, "normalizers": ["pe", "ntfs"]}`,
			Want:            withFullGeometry(3),
			WantNormalizers: []string{"pe", "ntfs"},
		},
		{
			// Both at once, on top of a non-default geometry — the fullest
			// repo the config type can describe.
			Name: "managed-encryption-and-normalizers",
			StoredJSON: `{"version": 1, "encryption_mode": "managed", "chunk_min_size": 32768, ` +
				`"chunk_avg_size": 131072, "chunk_max_size": 1048576, "buzhash_mask": 131071, ` +
				`"pack_file_max_size": 268435456, "compression_level": 9, "normalizers": ["ntfs"]}`,
			RecordEncryptionMode: "managed",
			LocalReaderRefuses:   true,
			Want: Want{
				ChunkMinSize:     32768,
				ChunkAvgSize:     131072,
				ChunkMaxSize:     1048576,
				BuzhashMask:      131071,
				PackFileMaxSize:  268435456,
				CompressionLevel: 9,
			},
			WantEncryptionMode: "managed",
			WantNormalizers:    []string{"ntfs"},
		},
		{
			// #265: a mode nobody can produce a key for. The controller
			// stores encryption_mode verbatim with no validation and only
			// the exact literal "managed" mints keys, so "manged" — a typo
			// in a curl body — creates a repo whose record claims encryption
			// and for which no key can ever exist. RepoConfigFromRecord
			// raises Encrypted for ANY non-empty mode, so every reader sees
			// an encrypted repo. Such a repo must fail the backup, never
			// fall through to plaintext.
			//
			// CloudOnly: a local config.json carrying this mode is a
			// different refusal (there is no record column to be
			// authoritative over), tested by the local reader's own cases.
			Name:                 "unknown-encryption-mode",
			StoredJSON:           `{"version": 1, ` + fullGeometry + `, "compression_level": 3}`,
			RecordEncryptionMode: "manged",
			CloudOnly:            true,
			Want:                 withFullGeometry(3),
			WantEncryptionMode:   "manged",
		},
		{
			// An explicitly empty normalizer list is the same value as no
			// list at all — a reader must not make it a third thing.
			Name: "normalizers-explicitly-empty",
			StoredJSON: `{"version": 1, ` + fullGeometry +
				`, "compression_level": 3, "normalizers": []}`,
			Want: withFullGeometry(3),
		},
	}
}
