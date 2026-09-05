// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline

import (
	"errors"
	"fmt"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Refusals a pipeline can raise before it writes anything.
var (
	// ErrUnbound: a Pipeline reached the write path without going through
	// New/Bind. Only reachable via a struct literal, which compiles from any
	// package because Pipeline has exported fields.
	ErrUnbound = errors.New("pipeline is unbound")
	// ErrKeyRequired: the repo says it is encrypted and no key was resolved.
	ErrKeyRequired = errors.New("encryption key required")
	// ErrUnexpectedKey: a key was resolved for a repo the config says is
	// unencrypted — the mode came from the wrong source.
	ErrUnexpectedKey = errors.New("encryption key supplied for an unencrypted repo")
	// ErrUnknownEncryptionMode: the repo's mode is not one this build can
	// produce a key for.
	ErrUnknownEncryptionMode = errors.New("unknown encryption mode")
)

// Binding is one repo's stored config resolved into the key material and
// normalizer a write pipeline must use. The zero value is invalid, and
// Pipeline.run refuses it.
//
// It exists because of #265: the agent's three cloud write paths called
// pipeline.New(cfg, logger) with no key at all, so a managed-encryption repo's
// chunks went to S3 as PLAINTEXT and nothing refused. The fix is not three
// checks at three call sites — it is that a pipeline cannot be constructed
// without naming the store.RepoConfig it is writing for, and that the one
// constructor refuses any repo it cannot encrypt for.
//
// The same constructor carries the normalizer list, which used to be an
// optional SetNormalizer call that exactly one of the fifteen write paths in
// the module actually made. Chunk identity is the hash of NORMALIZED bytes
// while ORIGINAL bytes are stored, so a write path that forgets the
// normalizer does not merely miss dedup — it writes chunks whose recorded
// hashes no read path can reproduce.
//
// On-disk frames carry no encryption marker ([4B payload len][4B raw len]
// [payload]), so a plaintext chunk is indistinguishable from a ciphertext one
// except by trying to decrypt it. There is no detection pass and no repair for
// a repo that has already been written wrong: write-time refusal is the entire
// defense.
type Binding struct {
	mode     store.EncryptionMode
	key      *crypto.MasterKey
	indexKey *crypto.MasterKey
	norm     preprocess.Normalizer
	bound    bool
}

// Bind is the only constructor of a Binding.
//
// rc MUST be a config actually read from the repo — store.RepoConfigFromRecord
// for a cloud repo, store.LoadRepoConfig for a local one. Bind's honesty is
// exactly as good as rc's provenance: a caller that synthesizes an rc saying
// "unencrypted" makes every check here vacuous, and nothing downstream can
// tell.
func Bind(rc store.RepoConfig, key *crypto.MasterKey) (Binding, error) {
	mode := rc.EffectiveEncryptionMode()
	switch mode {
	case store.EncryptNone:
		if key != nil {
			return Binding{}, fmt.Errorf("%w: repo encryption mode is none but a key was supplied — "+
				"the mode was resolved from the wrong source", ErrUnexpectedKey)
		}
	case store.EncryptManaged, store.EncryptPassphrase:
		if key == nil {
			return Binding{}, fmt.Errorf("%w: repo encryption mode %q, no key was resolved", ErrKeyRequired, mode)
		}
	default:
		// Not hypothetical. The controller stores encryption_mode verbatim
		// with no validation and only the exact literal "managed" mints key
		// material, so "Managed" or a typo creates a repo whose record calls
		// it encrypted and for which no key can ever exist. RepoConfigFromRecord
		// raises Encrypted for ANY non-empty mode, so every reader agrees it
		// is encrypted. Such a repo must fail the backup, never fall through
		// to plaintext.
		return Binding{}, fmt.Errorf("%w: %q", ErrUnknownEncryptionMode, mode)
	}
	norm, err := preprocess.FromNames(rc.Normalizers)
	if err != nil {
		return Binding{}, fmt.Errorf("repo normalizers: %w", err)
	}
	return Binding{
		mode:     mode,
		key:      key,
		indexKey: store.IndexKeyFor(rc, key),
		norm:     norm,
		bound:    true,
	}, nil
}

// MustBind is Bind for tests, where the repo config is a literal and an error
// is a bug in the test rather than a condition to handle. It panics on error.
//
// TestMustBindIsTestOnly forbids it in any non-_test.go file in the module:
// an escape hatch production code can reach is the hole this whole change
// closes.
func MustBind(rc store.RepoConfig, key *crypto.MasterKey) Binding {
	b, err := Bind(rc, key)
	if err != nil {
		panic("pipeline.MustBind: " + err.Error())
	}
	return b
}

// Mode is the repo's effective encryption mode.
func (b Binding) Mode() store.EncryptionMode { return b.mode }

// Key is the chunk encryption key, nil only for an unencrypted repo.
func (b Binding) Key() *crypto.MasterKey { return b.key }

// IndexKey is the key the dedup index and bloom filter are written under —
// nil under managed encryption, where the index stays in the clear so the
// controller can serve a server-side restore. See store.IndexKeyFor.
func (b Binding) IndexKey() *crypto.MasterKey { return b.indexKey }

// Normalizer is the repo's chunk normalizer, nil when the repo normalizes
// nothing.
func (b Binding) Normalizer() preprocess.Normalizer { return b.norm }

// NormalizerNames is the normalizer list this binding resolved, for tests and
// diagnostics.
func (b Binding) NormalizerNames() []string { return preprocess.Names(b.norm) }

// Destroy zeroes the key material. Nil-safe and idempotent; the index key is
// either the same key or nil, so it is never destroyed twice.
func (b Binding) Destroy() {
	if b.key != nil {
		b.key.Destroy()
	}
}
