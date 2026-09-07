// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestKeyIDNamesTheKeyAndSurvivesRewrap(t *testing.T) {
	kf, mk, err := GenerateKeyFile("first")
	if err != nil {
		t.Fatal(err)
	}
	defer mk.Destroy()
	if len(kf.KeyID) != KeyIDSize || !bytes.Equal(kf.KeyID, mk.ID()) {
		t.Fatalf("a generated key file's KeyID is %x, want the master key's ID %x", kf.KeyID, mk.ID())
	}
	if bytes.Contains(kf.KeyID, mk.key[:8]) {
		t.Fatal("the key ID carries key bytes")
	}
	rot, err := RewrapKeyFile(mk, "second")
	if err != nil {
		t.Fatal(err)
	}
	// Authority: the new passphrase opens the rewrapped file to the SAME key;
	// the old one does not.
	got, err := OpenMasterKey("second", rot)
	if err != nil {
		t.Fatalf("the rewrapped key file does not open with the new passphrase: %v", err)
	}
	defer got.Destroy()
	if got.key != mk.key {
		t.Fatal("rewrap changed the master key — data encrypted before the rotation is stranded")
	}
	if _, err := OpenMasterKey("first", rot); err == nil {
		t.Fatal("the old passphrase still opens the rewrapped key file")
	}
	if bytes.Equal(rot.Salt, kf.Salt) || bytes.Equal(rot.Nonce, kf.Nonce) || bytes.Equal(rot.WrappedKey, kf.WrappedKey) {
		t.Fatal("rewrap reused salt, nonce, or ciphertext")
	}
	if !kf.SameKey(rot) || !rot.SameKey(kf) {
		t.Fatal("two wraps of one master key are not recognized as the same key")
	}
	// A different repository's key file is a different key.
	other, mk2, _ := GenerateKeyFile("first")
	defer mk2.Destroy()
	if kf.SameKey(other) {
		t.Fatal("two different master keys share a key ID")
	}
	if _, err := RewrapKeyFile(mk, ""); err == nil {
		t.Fatal("rewrap accepted an empty passphrase")
	}
	// The ID covers the whole key: two keys that agree on every byte but
	// the last must have different IDs (random keys would differ at byte 0
	// and prove nothing about the rest).
	a := bytes.Repeat([]byte{0x42}, KeySize)
	b := append(bytes.Repeat([]byte{0x42}, KeySize-1), 0x43)
	ka, _ := MasterKeyFromBytes(a)
	kb, _ := MasterKeyFromBytes(b)
	defer ka.Destroy()
	defer kb.Destroy()
	if bytes.Equal(ka.ID(), kb.ID()) {
		t.Fatal("keys differing only in their last byte share an ID — the ID does not cover the whole key")
	}
}

// A key file written before key IDs (v0.2.5 and earlier) loads, opens, and
// is never "the same key" as anything — absence is unknown.
func TestKeyFileWithoutKeyIDIsUnknownNotSame(t *testing.T) {
	kf, mk, _ := GenerateKeyFile("pw")
	defer mk.Destroy()
	legacy := *kf
	legacy.KeyID = nil
	raw, _ := json.Marshal(legacy)
	if bytes.Contains(raw, []byte("key_id")) {
		t.Fatal("a legacy key file serializes a key_id field")
	}
	var back KeyFile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	got, err := OpenMasterKey("pw", &back)
	if err != nil {
		t.Fatalf("a key file without a key ID does not open: %v", err)
	}
	got.Destroy()
	if back.SameKey(kf) || kf.SameKey(&back) || back.SameKey(&back) {
		t.Fatal("a key file without a key ID was called the same key as something")
	}
	// And a modern file round-trips its id through JSON.
	raw2, _ := json.Marshal(kf)
	var back2 KeyFile
	json.Unmarshal(raw2, &back2)
	if !back2.SameKey(kf) {
		t.Fatal("the key ID does not survive JSON")
	}
}
