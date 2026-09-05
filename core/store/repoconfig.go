// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"encoding/json"
	"fmt"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
)

// RepoConfigFromRecord turns a controller repo record — the stored config blob
// plus the record's own encryption_mode column — into the RepoConfig every
// cloud reader must use.
//
// This is the single place a cloud repo record becomes a RepoConfig (#262).
// It used to be done twice: the CLI (newCloudConfig) unmarshaled the blob and
// then overlaid the record column, while the agent (realCloud.session)
// unmarshaled the blob and kept only the geometry and compression, dropping
// the encryption mode and the normalizers on the floor — because the type it
// carried onward, config.Config, cannot express either. The resume identity
// hash (checkpoint.ConfigHash) covers both, so on any repo that used them the
// two paths hashed differently and a backup suspended by one was refused by
// the other, silently restarting from zero.
//
// The record column is authoritative and overwrites whatever the blob said:
// the controller writes the mode to both at create time, but the column is
// what the repo's key material was generated against, so it is the one that
// describes how the bytes are actually encrypted. The Encrypted bool is only
// ever raised, never cleared — a v1 blob that carries the bool and no mode
// still resolves to passphrase through EffectiveEncryptionMode.
//
// A repo with no config blob at all is not an error: it resolves to the
// built-in defaults, exactly like an empty one. Malformed JSON is an error on
// every path — falling back to the defaults there would silently rechunk the
// repo at a different geometry.
func RepoConfigFromRecord(configJSON []byte, recordEncryptionMode string) (RepoConfig, error) {
	var rc RepoConfig
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &rc); err != nil {
			return RepoConfig{}, fmt.Errorf("parsing repo config: %w", err)
		}
	}
	rc.EncryptionMode = EncryptionMode(recordEncryptionMode)
	if rc.EncryptionMode != "" {
		rc.Encrypted = true
	}
	return rc, nil
}

// Effective resolves a stored repo config into the values the engine actually
// runs with: a zero-valued storage field means "not persisted", so it falls
// back to the built-in default rather than being applied literally.
//
// This is the single place that decision is made (#259). It used to be made
// independently by each reader, and they disagreed: the CLI treated a stored
// compression_level of 0 as unset and compressed at the default level 3,
// while the agent applied the 0 literally and compressed at zstd
// SpeedFastest. Every cloud repo created before #257 stored a 0 there,
// because the controller's create handler copied an omitted request field
// straight through, so the two paths wrote the same repo at different
// strengths. Reading a zero as "unset" is what config.Default() already
// documents, what the CLI already did, and the direction that strengthens
// rather than silently weakens compression.
//
// Only the storage/chunker geometry is resolved. Encryption has its own
// resolution (EffectiveEncryptionMode) and Normalizers has none: an empty
// normalizer list is a legitimate value, not an unset one.
//
// Nothing about this affects reading existing data. The zstd decoder takes no
// level, and chunk locations are recorded per chunk, so resolving a stored
// zero differently only changes how *subsequent* writes are encoded.
func (rc RepoConfig) Effective() RepoConfig {
	if rc.ChunkMinSize <= 0 {
		rc.ChunkMinSize = config.DefaultChunkMinSize
	}
	if rc.ChunkAvgSize <= 0 {
		rc.ChunkAvgSize = config.DefaultChunkAvgSize
	}
	if rc.ChunkMaxSize <= 0 {
		rc.ChunkMaxSize = config.DefaultChunkMaxSize
	}
	if rc.BuzhashMask == 0 {
		rc.BuzhashMask = config.DefaultBuzhashMask
	}
	if rc.PackFileMaxSize <= 0 {
		rc.PackFileMaxSize = config.DefaultPackFileMaxSize
	}
	if rc.CompressionLevel <= 0 {
		rc.CompressionLevel = config.DefaultCompressionLevel
	}
	return rc
}

// ApplyTo overlays the repository's persisted chunker and storage parameters
// onto cfg, so the write path (backup / analyze / prune) honors the settings
// the repo was created with instead of the process defaults (#57).
//
// The chunk-affecting parameters (min/avg/max size and the buzhash mask) are
// the important ones: chunk identity is the hash of normalized bytes at
// content-defined boundaries, so using different boundaries than prior
// backups silently degrades dedup.
//
// Every reader of a stored repo config must go through here (#259) — see
// Effective for why. Fields cfg holds that a repo config does not describe
// (worker counts, buffers, index tuning) are left alone.
func (rc RepoConfig) ApplyTo(cfg *config.Config) {
	eff := rc.Effective()
	cfg.ChunkMinSize = eff.ChunkMinSize
	cfg.ChunkAvgSize = eff.ChunkAvgSize
	cfg.ChunkMaxSize = eff.ChunkMaxSize
	cfg.BuzhashMask = eff.BuzhashMask
	cfg.PackFileMaxSize = eff.PackFileMaxSize
	cfg.CompressionLevel = eff.CompressionLevel
}

// IndexKeyFor returns the key a repo's dedup index and bloom filter are
// encrypted under, given the key its chunks are encrypted under.
//
// Under managed encryption the index is written in the CLEAR on purpose: the
// controller's server-side restore (internal/controller/restorezip.go) opens
// the index with a nil key while opening the chunk store with the DEK, so a
// Web Restore needs nothing from the operator. Passing the DEK here instead
// re-encrypts bloom.bin and hash-index.db to .enc and deletes the plaintext,
// leaving every other command staring at an empty index.
//
// This is the single place that rule lives (#265). It used to be hand-rolled
// at four CLI call sites and implicitly violated by the variadic
// pipeline.New(cfg, logger, key) form, which defaulted the index key to the
// chunk key.
func IndexKeyFor(rc RepoConfig, key *crypto.MasterKey) *crypto.MasterKey {
	if rc.EffectiveEncryptionMode() == EncryptManaged {
		return nil
	}
	return key
}

// IndexEncryptedAtRest is IndexKeyFor's rule as a predicate (#470): would
// this repo's index objects carry the .enc suffix? Derived HERE, beside the
// rule, because deriving it anywhere else is how the first cut of the
// encryption hint skipped the PLAIN index of every managed repo — managed
// repos encrypt their chunks and deliberately not their index, and a hint
// built on the chunk mode broke Web Restore against exactly them.
func (rc RepoConfig) IndexEncryptedAtRest() bool {
	mode := rc.EffectiveEncryptionMode()
	return mode != EncryptNone && mode != EncryptManaged
}
