// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMasterKeyRoundTrip(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	plaintext := []byte("hello world, this is a secret message for testing encryption")

	ciphertext, err := mk.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Ciphertext should be nonce + plaintext + tag
	expectedLen := NonceSize + len(plaintext) + TagSize
	if len(ciphertext) != expectedLen {
		t.Fatalf("ciphertext length = %d, want %d", len(ciphertext), expectedLen)
	}

	decrypted, err := mk.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("decrypted data does not match original")
	}
}

func TestMasterKeyEmptyPlaintext(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	ciphertext, err := mk.Encrypt([]byte{})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	decrypted, err := mk.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}

	if len(decrypted) != 0 {
		t.Errorf("expected empty, got %d bytes", len(decrypted))
	}
}

func TestNonceUniqueness(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	plaintext := []byte("same data")
	nonces := make(map[string]bool)

	for i := 0; i < 100; i++ {
		ct, err := mk.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt %d: %v", i, err)
		}
		nonce := string(ct[:NonceSize])
		if nonces[nonce] {
			t.Fatalf("duplicate nonce at iteration %d", i)
		}
		nonces[nonce] = true
	}
}

func TestTamperedCiphertext(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	ciphertext, err := mk.Encrypt([]byte("secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Flip a bit in the ciphertext (after nonce)
	ciphertext[NonceSize+2] ^= 0xFF

	_, err = mk.Decrypt(ciphertext)
	if err == nil {
		t.Error("expected error on tampered ciphertext, got nil")
	}
}

func TestTruncatedCiphertext(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	_, err = mk.Decrypt([]byte("short"))
	if err == nil {
		t.Error("expected error on truncated ciphertext, got nil")
	}
}

func TestKeyWrapUnwrap(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	kek := DeriveKEK("test-passphrase", make([]byte, SaltSize), Argon2Params{
		Time: 1, Memory: 64 * 1024, Threads: 1,
	})

	nonce, wrapped, err := WrapKey(kek, mk)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}

	unwrapped, err := UnwrapKey(kek, nonce, wrapped)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	defer unwrapped.Destroy()

	if mk.key != unwrapped.key {
		t.Error("unwrapped key does not match original")
	}
}

func TestWrongPassphrase(t *testing.T) {
	kf, mk, err := GenerateKeyFile("correct-passphrase")
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	mk.Destroy()

	_, err = OpenMasterKey("wrong-passphrase", kf)
	if err == nil {
		t.Error("expected error with wrong passphrase, got nil")
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.key")

	kf, mk, err := GenerateKeyFile("my-secret")
	if err != nil {
		t.Fatalf("GenerateKeyFile: %v", err)
	}
	defer mk.Destroy()

	// Encrypt something to verify the key works
	plaintext := []byte("test data")
	ct, err := mk.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Save and reload key file
	if err := SaveKeyFile(path, kf); err != nil {
		t.Fatalf("SaveKeyFile: %v", err)
	}

	// Verify file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0600 {
			t.Errorf("key file permissions = %o, want 0600", info.Mode().Perm())
		}
	}

	loaded, err := LoadKeyFile(path)
	if err != nil {
		t.Fatalf("LoadKeyFile: %v", err)
	}

	// Open master key from loaded file
	mk2, err := OpenMasterKey("my-secret", loaded)
	if err != nil {
		t.Fatalf("OpenMasterKey: %v", err)
	}
	defer mk2.Destroy()

	// Decrypt with the reloaded key
	decrypted, err := mk2.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt with reloaded key: %v", err)
	}

	if !bytes.Equal(plaintext, decrypted) {
		t.Error("decrypted data does not match original")
	}
}

func TestEncryptDecryptFile(t *testing.T) {
	dir := t.TempDir()
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	srcPath := filepath.Join(dir, "plain.bin")
	encPath := filepath.Join(dir, "encrypted.bin")
	decPath := filepath.Join(dir, "decrypted.bin")

	original := []byte("file encryption test content with some data")
	if err := os.WriteFile(srcPath, original, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := EncryptFile(mk, srcPath, encPath); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	// Encrypted file should differ from original
	encData, _ := os.ReadFile(encPath)
	if bytes.Equal(encData, original) {
		t.Error("encrypted file is identical to original")
	}

	if err := DecryptFile(mk, encPath, decPath); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}

	decData, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if !bytes.Equal(original, decData) {
		t.Error("decrypted file does not match original")
	}
}

func TestAsymmetricWrapUnwrap(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	defer priv.Destroy()

	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	wrapped, err := WrapKeyAsymmetric(pub, mk)
	if err != nil {
		t.Fatalf("WrapKeyAsymmetric: %v", err)
	}

	if len(wrapped) != 92 {
		t.Fatalf("wrapped length = %d, want 92", len(wrapped))
	}

	unwrapped, err := UnwrapKeyAsymmetric(priv, wrapped)
	if err != nil {
		t.Fatalf("UnwrapKeyAsymmetric: %v", err)
	}
	defer unwrapped.Destroy()

	if mk.key != unwrapped.key {
		t.Error("unwrapped key does not match original")
	}
}

func TestAsymmetricWrongKey(t *testing.T) {
	pub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	_, wrongPriv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	defer wrongPriv.Destroy()

	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	wrapped, err := WrapKeyAsymmetric(pub, mk)
	if err != nil {
		t.Fatalf("WrapKeyAsymmetric: %v", err)
	}

	_, err = UnwrapKeyAsymmetric(wrongPriv, wrapped)
	if err == nil {
		t.Error("expected error unwrapping with wrong key, got nil")
	}
}

func TestAsymmetricTampered(t *testing.T) {
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	defer priv.Destroy()

	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	wrapped, err := WrapKeyAsymmetric(pub, mk)
	if err != nil {
		t.Fatalf("WrapKeyAsymmetric: %v", err)
	}

	// Flip a bit in the ciphertext portion
	wrapped[60] ^= 0xFF

	_, err = UnwrapKeyAsymmetric(priv, wrapped)
	if err == nil {
		t.Error("expected error on tampered wrapped data, got nil")
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption.pub")

	pub, _, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}

	if err := SavePublicKey(path, pub); err != nil {
		t.Fatalf("SavePublicKey: %v", err)
	}

	loaded, err := LoadPublicKey(path)
	if err != nil {
		t.Fatalf("LoadPublicKey: %v", err)
	}

	if !bytes.Equal(pub.Bytes(), loaded.Bytes()) {
		t.Error("loaded public key does not match original")
	}
}

func TestDestroy(t *testing.T) {
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}

	// Key should not be all zeros
	var zeroKey [KeySize]byte
	if mk.key == zeroKey {
		t.Fatal("key should not be zero after generation")
	}

	mk.Destroy()

	if mk.key != zeroKey {
		t.Error("key should be zeroed after Destroy")
	}
}
