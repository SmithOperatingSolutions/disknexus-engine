// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenMasterKeyRejectsInvalidParams proves that a key file with
// unusable Argon2 parameters (e.g. missing "argon2_params" in the JSON, so
// Time/Threads are zero) returns an error instead of panicking. argon2.IDKey
// panics when time < 1 or threads < 1, so a truncated or hand-edited key
// file would otherwise crash the whole process.
func TestOpenMasterKeyRejectsInvalidParams(t *testing.T) {
	kf := &KeyFile{
		Params:     Argon2Params{}, // all zero — unusable
		Salt:       make([]byte, SaltSize),
		Nonce:      make([]byte, NonceSize),
		WrappedKey: make([]byte, KeySize+TagSize),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("OpenMasterKey panicked on invalid params instead of returning an error: %v", r)
		}
	}()

	if _, err := OpenMasterKey("passphrase", kf); err == nil {
		t.Fatal("OpenMasterKey accepted a key file with zero Argon2 params; want an error")
	}
}

// TestEncryptFileIsAtomicOverSymlink proves EncryptFile writes its output
// atomically (temp file + rename) rather than truncating the destination in
// place. The encrypted dedup index is re-encrypted over its only copy, so an
// in-place write that follows a symlink or fails mid-write can destroy the
// prior good data. Here dst is a symlink to a sentinel file: an in-place
// os.WriteFile follows the link and clobbers the sentinel, whereas an atomic
// rename replaces the link and leaves the sentinel intact.
func TestEncryptFileIsAtomicOverSymlink(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.bin")
	if err := os.WriteFile(src, []byte("secret payload"), 0600); err != nil {
		t.Fatal(err)
	}

	sentinel := filepath.Join(dir, "unrelated.bin")
	if err := os.WriteFile(sentinel, []byte("IMPORTANT DATA"), 0600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out.enc")
	if err := os.Symlink(sentinel, dst); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := EncryptFile(mk, src, dst); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "IMPORTANT DATA" {
		t.Fatalf("EncryptFile followed a symlink and clobbered an unrelated file: sentinel now %q", got)
	}

	// And the destination itself must round-trip.
	dec := filepath.Join(dir, "roundtrip.bin")
	if err := DecryptFile(mk, dst, dec); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	back, err := os.ReadFile(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "secret payload" {
		t.Fatalf("round-trip mismatch: got %q", back)
	}
}
