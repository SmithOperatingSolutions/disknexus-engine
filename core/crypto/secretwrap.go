// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// Wrapping an arbitrary secret to a device's X25519 public key.
//
// WrapKeyAsymmetric wraps exactly one MasterKey. A product that distributes
// a repository passphrase to enrolled devices needs the same construction
// for bytes of any length — and a distinct HKDF domain, so a blob sealed
// as a secret can never be presented as a wrapped master key or the other
// way round. The layout is the one a browser can reproduce with WebCrypto
// alone (X25519 deriveBits, HKDF-SHA256, AES-256-GCM):
//
//	ephemeral X25519 public key (32) || nonce (12) || AES-GCM ciphertext+tag
//
// HKDF: SHA-256, no salt, info secretWrapInfo, 32-byte output.

// secretWrapInfo is the HKDF info for secret wrapping — deliberately not
// hkdfInfo (the DEK domain).
var secretWrapInfo = []byte("disknexus-secret-wrap-v1")

// SecretWrapOverhead is the number of bytes a wrapped secret carries beyond
// the secret itself.
const SecretWrapOverhead = 32 + NonceSize + TagSize

// WrapSecretAsymmetric seals secret to pub. An empty secret is refused: the
// callers' secrets (passphrases) are never empty, and an empty blob is more
// likely a bug than a message.
func WrapSecretAsymmetric(pub *PublicKey, secret []byte) ([]byte, error) {
	if pub == nil || pub.key == nil {
		return nil, fmt.Errorf("wrapping a secret needs a public key")
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("an empty secret is not a secret")
	}
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral key: %w", err)
	}
	shared, err := ephPriv.ECDH(pub.key)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	defer zeroBytes(shared)
	wrappingKey, err := deriveSecretWrappingKey(shared)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(wrappingKey)
	gcm, err := secretGCM(wrappingKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	ephPub := ephPriv.PublicKey().Bytes()
	out := make([]byte, 0, len(ephPub)+len(nonce)+len(secret)+TagSize)
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, secret, nil)
	return out, nil
}

// UnwrapSecretAsymmetric opens a blob sealed by WrapSecretAsymmetric (or a
// browser reproducing its layout) with the recipient's private key.
func UnwrapSecretAsymmetric(priv *PrivateKey, wrapped []byte) ([]byte, error) {
	if priv == nil || priv.key == nil {
		return nil, fmt.Errorf("unwrapping a secret needs a private key")
	}
	if len(wrapped) <= SecretWrapOverhead {
		return nil, fmt.Errorf("wrapped secret is %d bytes; it must be longer than the %d-byte overhead", len(wrapped), SecretWrapOverhead)
	}
	ephPub, err := ecdh.X25519().NewPublicKey(wrapped[:32])
	if err != nil {
		return nil, fmt.Errorf("parsing ephemeral public key: %w", err)
	}
	shared, err := priv.key.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	defer zeroBytes(shared)
	wrappingKey, err := deriveSecretWrappingKey(shared)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(wrappingKey)
	gcm, err := secretGCM(wrappingKey)
	if err != nil {
		return nil, err
	}
	nonce := wrapped[32 : 32+NonceSize]
	secret, err := gcm.Open(nil, nonce, wrapped[32+NonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("unwrapping secret (wrong key or tampered): %w", err)
	}
	return secret, nil
}

func deriveSecretWrappingKey(shared []byte) ([]byte, error) {
	r := hkdf.New(sha256.New, shared, nil, secretWrapInfo)
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, fmt.Errorf("HKDF derivation: %w", err)
	}
	return key, nil
}

func secretGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return gcm, nil
}
