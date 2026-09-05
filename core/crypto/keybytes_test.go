// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"bytes"
	"testing"
)

// Keys travel as raw bytes — the keyring, the controller's key custody,
// the recovery key entry — and come back through these constructors. A
// key rebuilt from its bytes must be THE key: it decrypts what the
// original encrypted, and a private key rebuilt from its bytes still pairs
// with the same public key. Wrong-length material is refused, never
// truncated or padded into a key that silently differs.
func TestKeysRebuiltFromBytesAreTheSameKeys(t *testing.T) {
	raw := bytes.Repeat([]byte{0x5a}, KeySize)
	raw[3], raw[17] = 0x01, 0xfe
	k1, err := MasterKeyFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer k1.Destroy()
	ct, err := k1.Encrypt([]byte("the payload a re-derived key must open"))
	if err != nil {
		t.Fatal(err)
	}
	k2, err := MasterKeyFromBytes(append([]byte(nil), raw...))
	if err != nil {
		t.Fatal(err)
	}
	defer k2.Destroy()
	pt, err := k2.Decrypt(ct)
	if err != nil || string(pt) != "the payload a re-derived key must open" {
		t.Fatalf("a master key rebuilt from the same bytes did not decrypt: %v", err)
	}
	other := append([]byte(nil), raw...)
	other[0] ^= 0x80
	k3, _ := MasterKeyFromBytes(other)
	defer k3.Destroy()
	if _, err := k3.Decrypt(ct); err == nil {
		t.Fatal("a key from different bytes decrypted the ciphertext")
	}
	for _, n := range []int{0, KeySize - 1, KeySize + 1, 64} {
		if _, err := MasterKeyFromBytes(make([]byte, n)); err == nil {
			t.Fatalf("MasterKeyFromBytes accepted %d bytes", n)
		}
	}

	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Destroy()
	privBytes := priv.Bytes()
	if len(privBytes) != 32 || len(pub.Bytes()) != 32 {
		t.Fatalf("X25519 key bytes: private %d public %d, want 32/32", len(privBytes), len(pub.Bytes()))
	}
	again, err := PrivateKeyFromBytes(privBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Destroy()
	if !bytes.Equal(again.Bytes(), privBytes) {
		t.Fatal("a private key rebuilt from its bytes serializes differently")
	}
	pubAgain, err := PublicKeyFromBytes(pub.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pubAgain.Bytes(), pub.Bytes()) {
		t.Fatal("a public key rebuilt from its bytes serializes differently")
	}
	if _, err := PrivateKeyFromBytes(privBytes[:31]); err == nil {
		t.Fatal("PrivateKeyFromBytes accepted 31 bytes")
	}
	if _, err := PublicKeyFromBytes([]byte("short")); err == nil {
		t.Fatal("PublicKeyFromBytes accepted 5 bytes")
	}
}
