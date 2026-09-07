// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/sha256"
	"io"
	"strings"
	"testing"

	"golang.org/x/crypto/hkdf"
)

func TestSecretWrapRoundTripAnyLength(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Destroy()
	for _, n := range []int{1, 7, 32, 33, 200, 4096} {
		secret := bytes.Repeat([]byte{byte(n)}, n)
		wrapped, err := WrapSecretAsymmetric(pub, secret)
		if err != nil {
			t.Fatalf("wrap %d bytes: %v", n, err)
		}
		if len(wrapped) != n+SecretWrapOverhead {
			t.Fatalf("wrapped %d-byte secret is %d bytes, want %d", n, len(wrapped), n+SecretWrapOverhead)
		}
		if bytes.Contains(wrapped, secret) && n >= 7 {
			t.Fatalf("the wrapped blob carries the %d-byte secret in the clear", n)
		}
		got, err := UnwrapSecretAsymmetric(priv, wrapped)
		if err != nil {
			t.Fatalf("unwrap %d bytes: %v", n, err)
		}
		if !bytes.Equal(got, secret) {
			t.Fatalf("unwrapped %d bytes differ", n)
		}
	}
}

func TestSecretWrapRefusals(t *testing.T) {
	pub, priv, _ := GenerateKeyPair()
	defer priv.Destroy()
	_, priv2, _ := GenerateKeyPair()
	defer priv2.Destroy()
	secret := []byte("correct horse battery staple")
	wrapped, err := WrapSecretAsymmetric(pub, secret)
	if err != nil {
		t.Fatal(err)
	}
	// Positive control: the right key opens it (the refusals below are
	// about the key or the bytes, not a broken fixture).
	if got, err := UnwrapSecretAsymmetric(priv, wrapped); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("control: %v", err)
	}
	if _, err := UnwrapSecretAsymmetric(priv2, wrapped); err == nil {
		t.Fatal("another device's private key opened the secret")
	}
	for i, at := range []int{0, 31, 32, 43, 44, len(wrapped) - 1} {
		tampered := append([]byte(nil), wrapped...)
		tampered[at] ^= 0x01
		if _, err := UnwrapSecretAsymmetric(priv, tampered); err == nil {
			t.Fatalf("case %d: a bit flipped at %d still unwrapped", i, at)
		}
	}
	if _, err := UnwrapSecretAsymmetric(priv, wrapped[:SecretWrapOverhead]); err == nil || !strings.Contains(err.Error(), "overhead") {
		t.Fatalf("a blob with no ciphertext: %v", err)
	}
	if _, err := WrapSecretAsymmetric(pub, nil); err == nil {
		t.Fatal("an empty secret was wrapped")
	}
	// Domain separation: a master key wrapped for the DEK domain is not a
	// secret, and a secret is not a master key — the HKDF info differs.
	mk, _ := GenerateMasterKey()
	defer mk.Destroy()
	asDEK, err := WrapKeyAsymmetric(pub, mk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapSecretAsymmetric(priv, asDEK); err == nil {
		t.Fatal("a DEK-domain blob opened as a secret")
	}
	as32, _ := WrapSecretAsymmetric(pub, mk.key[:])
	if _, err := UnwrapKeyAsymmetric(priv, as32); err == nil {
		t.Fatal("a secret-domain blob opened as a master key")
	}
}

// TestSecretWrapLayoutIsWhatABrowserBuilds reproduces the wrap with the
// primitives WebCrypto exposes — X25519 shared secret, HKDF-SHA256 with no
// salt and the documented info, AES-256-GCM over the plain layout — and
// the engine must open it. This is the contract the panel's JavaScript is
// written against; change either side and this fails.
func TestSecretWrapLayoutIsWhatABrowserBuilds(t *testing.T) {
	pub, priv, _ := GenerateKeyPair()
	defer priv.Destroy()
	secret := []byte("from-the-browser")

	eph, err := ecdh.X25519().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{7}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	shared, err := eph.ECDH(pub.key)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, nil, []byte("disknexus-secret-wrap-v1")), key); err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{9}, 12)
	blob := append(append(append([]byte{}, eph.PublicKey().Bytes()...), nonce...), gcm.Seal(nil, nonce, secret, nil)...)

	got, err := UnwrapSecretAsymmetric(priv, blob)
	if err != nil {
		t.Fatalf("the engine does not open a browser-built blob: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatal("browser-built blob unwrapped to different bytes")
	}
	// And the info string is load-bearing: the DEK domain's info must not.
	key2 := make([]byte, 32)
	io.ReadFull(hkdf.New(sha256.New, shared, nil, []byte("disknexus-dek-wrap")), key2)
	block2, _ := aes.NewCipher(key2)
	gcm2, _ := cipher.NewGCM(block2)
	blob2 := append(append(append([]byte{}, eph.PublicKey().Bytes()...), nonce...), gcm2.Seal(nil, nonce, secret, nil)...)
	if _, err := UnwrapSecretAsymmetric(priv, blob2); err == nil {
		t.Fatal("a blob derived with the DEK info opened as a secret")
	}
}
