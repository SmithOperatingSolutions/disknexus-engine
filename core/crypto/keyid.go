// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
)

// Key identity and passphrase rotation.
//
// A key file wraps a master key under a passphrase. Rotating the passphrase
// means a NEW key file for the SAME master key — nothing encrypted with it
// changes. Two key files that wrap one master key must be recognizable as
// such without either passphrase (a local copy of a repository whose wrap
// is stale is the case): KeyID is that recognition. It is a hash over a
// fixed domain string and the key, so it names the key without carrying
// any of it; it is not a secret.

// keyIDDomain separates the key ID from every other use of the key.
var keyIDDomain = []byte("disknexus-key-id-v1")

// KeyIDSize is the length of a key ID in bytes.
const KeyIDSize = 32

// ID returns the master key's identity.
func (mk *MasterKey) ID() []byte {
	h := sha256.New()
	h.Write(keyIDDomain)
	h.Write(mk.key[:])
	return h.Sum(nil)
}

// RewrapKeyFile wraps mk under passphrase in a fresh key file: new salt,
// new nonce, the default Argon2 parameters, and mk's KeyID. This is the
// rotation primitive — a repository rotates by replacing its key file with
// this one; no data is re-encrypted.
func RewrapKeyFile(mk *MasterKey, passphrase string) (*KeyFile, error) {
	if mk == nil {
		return nil, fmt.Errorf("rewrap needs a master key")
	}
	if passphrase == "" {
		return nil, fmt.Errorf("an empty passphrase is not a passphrase")
	}
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, fmt.Errorf("generating salt: %w", err)
	}
	params := Argon2Params{Time: DefaultArgonTime, Memory: DefaultArgonMemory, Threads: DefaultArgonThreads}
	kek := DeriveKEK(passphrase, salt, params)
	defer zeroBytes(kek)
	nonce, wrapped, err := WrapKey(kek, mk)
	if err != nil {
		return nil, err
	}
	return &KeyFile{Params: params, Salt: salt, Nonce: nonce, WrappedKey: wrapped, KeyID: mk.ID()}, nil
}

// SameKey reports whether both key files name the same master key. It is
// false when either predates key IDs — absence is "unknown", never "same".
func (kf *KeyFile) SameKey(other *KeyFile) bool {
	if kf == nil || other == nil || len(kf.KeyID) != KeyIDSize || len(other.KeyID) != KeyIDSize {
		return false
	}
	for i := range kf.KeyID {
		if kf.KeyID[i] != other.KeyID[i] {
			return false
		}
	}
	return true
}
