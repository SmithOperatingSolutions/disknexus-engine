// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"bytes"
	"testing"
)

// TestAADDomainSeparation verifies that AES-GCM domain tags actually bind a
// ciphertext to its context: a value sealed for one purpose fails
// authentication if opened with a different tag (or none). This is what stops a
// ciphertext being transplanted between chunk / index / SQLite / key-wrap
// contexts under the shared master key.
func TestAADDomainSeparation(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	pt := []byte("top secret payload")
	ct, err := mk.EncryptWithAAD(pt, AADChunk)
	if err != nil {
		t.Fatalf("EncryptWithAAD: %v", err)
	}

	// Same tag round-trips.
	got, err := mk.DecryptWithAAD(ct, AADChunk)
	if err != nil {
		t.Fatalf("DecryptWithAAD same tag: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("round trip mismatch: got %q want %q", got, pt)
	}

	// A different domain tag must fail authentication.
	if _, err := mk.DecryptWithAAD(ct, AADIndex); err == nil {
		t.Fatal("chunk-context ciphertext decrypted under the index tag — no domain separation")
	}

	// No tag (plain Decrypt) must also fail.
	if _, err := mk.Decrypt(ct); err == nil {
		t.Fatal("chunk-context ciphertext decrypted with no tag — no domain separation")
	}

	// Sanity: each distinct tag is actually distinct in effect.
	for _, tag := range [][]byte{AADIndex, AADSQLiteField, AADPrivateKey, AADDEK} {
		c, err := mk.EncryptWithAAD(pt, tag)
		if err != nil {
			t.Fatalf("EncryptWithAAD(%s): %v", tag, err)
		}
		if _, err := mk.DecryptWithAAD(c, AADChunk); err == nil {
			t.Fatalf("ciphertext for tag %q opened under the chunk tag", tag)
		}
	}
}
