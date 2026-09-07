// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const (
	KeySize   = 32 // AES-256
	NonceSize = 12 // GCM nonce
	TagSize   = 16 // GCM tag
	Overhead  = NonceSize + TagSize
	SaltSize  = 16
)

// Argon2id default parameters.
const (
	DefaultArgonTime    = 3
	DefaultArgonMemory  = 64 * 1024 // 64 MB
	DefaultArgonThreads = 4
)

// Argon2Params holds Argon2id KDF parameters.
type Argon2Params struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory"`
	Threads uint8  `json:"threads"`
}

// MasterKey holds the 256-bit master encryption key.
type MasterKey struct {
	key [KeySize]byte
}

// Domain-separation AAD tags. AES-GCM authenticates the additional data, so
// binding each ciphertext to a context tag means a value encrypted for one
// purpose (e.g. a SQLite field) fails authentication if it is transplanted into
// another (e.g. a chunk payload or a wrapped key). Encrypt and decrypt of the
// same value must always pass the same tag.
var (
	AADChunk       = []byte("disknexus/chunk/v1")
	AADIndex       = []byte("disknexus/index/v1")
	AADSQLiteField = []byte("disknexus/sqlite-field/v1")
	AADPrivateKey  = []byte("disknexus/private-key/v1")
	AADDEK         = []byte("disknexus/dek/v1")
)

// Encrypt encrypts plaintext using AES-256-GCM with no domain separation.
// Prefer EncryptWithAAD with a context tag; this remains for callers that have
// no meaningful context. Returns [12-byte nonce][ciphertext + 16-byte tag].
func (mk *MasterKey) Encrypt(plaintext []byte) ([]byte, error) {
	return mk.EncryptWithAAD(plaintext, nil)
}

// Decrypt decrypts data produced by Encrypt (nil AAD).
func (mk *MasterKey) Decrypt(data []byte) ([]byte, error) {
	return mk.DecryptWithAAD(data, nil)
}

// EncryptWithAAD encrypts plaintext with AES-256-GCM, binding the ciphertext to
// aad (a domain tag). aad is authenticated but not stored in the output.
// Returns [12-byte nonce][ciphertext + 16-byte tag].
func (mk *MasterKey) EncryptWithAAD(plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(mk.key[:])
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	// nonce + Seal(ciphertext + tag)
	out := make([]byte, NonceSize, NonceSize+len(plaintext)+TagSize)
	copy(out, nonce)
	out = gcm.Seal(out, nonce, plaintext, aad)
	return out, nil
}

// DecryptWithAAD decrypts data produced by EncryptWithAAD with the same aad. A
// tag mismatch (ciphertext from another context) fails authentication.
func (mk *MasterKey) DecryptWithAAD(data, aad []byte) ([]byte, error) {
	if len(data) < Overhead {
		return nil, fmt.Errorf("ciphertext too short (%d bytes)", len(data))
	}

	block, err := aes.NewCipher(mk.key[:])
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := data[:NonceSize]
	ciphertext := data[NonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return plaintext, nil
}

// Destroy zeros the key material.
func (mk *MasterKey) Destroy() {
	for i := range mk.key {
		mk.key[i] = 0
	}
}

// zeroBytes overwrites a byte slice with zeros to prevent key material
// from lingering in heap memory.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// GenerateMasterKey creates a new random 256-bit master key.
func GenerateMasterKey() (*MasterKey, error) {
	mk := &MasterKey{}
	if _, err := io.ReadFull(rand.Reader, mk.key[:]); err != nil {
		return nil, fmt.Errorf("generating master key: %w", err)
	}
	return mk, nil
}

// Validate reports whether the Argon2 parameters are usable. argon2.IDKey
// panics on time < 1 or threads < 1 and requires memory >= 8*threads, so
// zero/garbage params (a truncated or hand-edited key file, or JSON missing
// "argon2_params") must be rejected before deriving a key.
func (p Argon2Params) Validate() error {
	if p.Time < 1 {
		return fmt.Errorf("argon2 time must be >= 1, got %d", p.Time)
	}
	if p.Threads < 1 {
		return fmt.Errorf("argon2 threads must be >= 1, got %d", p.Threads)
	}
	if p.Memory < 8*uint32(p.Threads) {
		return fmt.Errorf("argon2 memory must be >= %d, got %d", 8*uint32(p.Threads), p.Memory)
	}
	return nil
}

// DeriveKEK derives a key-encryption key from a passphrase using Argon2id.
// The caller must have validated params (see Argon2Params.Validate); passing
// unusable params panics, matching golang.org/x/crypto/argon2.
func DeriveKEK(passphrase string, salt []byte, params Argon2Params) []byte {
	return argon2.IDKey([]byte(passphrase), salt, params.Time, params.Memory, params.Threads, KeySize)
}

// WrapKey encrypts the master key with the KEK using AES-GCM.
// Returns (nonce, wrappedKey).
func WrapKey(kek []byte, masterKey *MasterKey) (nonce []byte, wrapped []byte, err error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce = make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("generating nonce: %w", err)
	}

	wrapped = gcm.Seal(nil, nonce, masterKey.key[:], nil)
	return nonce, wrapped, nil
}

// UnwrapKey decrypts the master key with the KEK.
func UnwrapKey(kek, nonce, wrapped []byte) (*MasterKey, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	keyBytes, err := gcm.Open(nil, nonce, wrapped, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrapping key: %w", err)
	}
	defer zeroBytes(keyBytes)

	mk := &MasterKey{}
	copy(mk.key[:], keyBytes)
	return mk, nil
}

// KeyFile is the JSON-serializable encryption key file.
type KeyFile struct {
	Params     Argon2Params `json:"argon2_params"`
	Salt       []byte       `json:"salt"`
	Nonce      []byte       `json:"nonce"`
	WrappedKey []byte       `json:"wrapped_key"`
	// KeyID names the master key without carrying it (see keyid.go). Absent
	// in key files written before v0.2.6; readers treat absence as unknown.
	KeyID []byte `json:"key_id,omitempty"`
}

// GenerateKeyFile creates a new key file from a passphrase.
// Returns the key file (for saving) and the master key (for immediate use).
func GenerateKeyFile(passphrase string) (*KeyFile, *MasterKey, error) {
	mk, err := GenerateMasterKey()
	if err != nil {
		return nil, nil, err
	}

	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, nil, fmt.Errorf("generating salt: %w", err)
	}

	params := Argon2Params{
		Time:    DefaultArgonTime,
		Memory:  DefaultArgonMemory,
		Threads: DefaultArgonThreads,
	}

	kek := DeriveKEK(passphrase, salt, params)
	defer zeroBytes(kek)
	nonce, wrapped, err := WrapKey(kek, mk)
	if err != nil {
		mk.Destroy()
		return nil, nil, err
	}

	kf := &KeyFile{
		Params:     params,
		Salt:       salt,
		Nonce:      nonce,
		WrappedKey: wrapped,
		KeyID:      mk.ID(),
	}
	return kf, mk, nil
}

// SaveKeyFile writes a KeyFile to disk as JSON.
func SaveKeyFile(path string, kf *KeyFile) error {
	data, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling key file: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing key file: %w", err)
	}
	return nil
}

// LoadKeyFile reads a KeyFile from disk.
func LoadKeyFile(path string) (*KeyFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}
	var kf KeyFile
	if err := json.Unmarshal(data, &kf); err != nil {
		return nil, fmt.Errorf("parsing key file: %w", err)
	}
	return &kf, nil
}

// OpenMasterKey derives the KEK from a passphrase and unwraps the master key.
func OpenMasterKey(passphrase string, kf *KeyFile) (*MasterKey, error) {
	if err := kf.Params.Validate(); err != nil {
		return nil, fmt.Errorf("invalid key file: %w", err)
	}
	kek := DeriveKEK(passphrase, kf.Salt, kf.Params)
	defer zeroBytes(kek)
	mk, err := UnwrapKey(kek, kf.Nonce, kf.WrappedKey)
	if err != nil {
		return nil, fmt.Errorf("unlocking master key (wrong passphrase?): %w", err)
	}
	return mk, nil
}

// writeFileAtomic writes data to a temp file in dst's directory, fsyncs it,
// and renames it over dst. A crash or write error leaves the previous dst
// intact rather than a truncated file, and the rename replaces dst itself
// (not a symlink target). Callers rely on this for the encrypted dedup index,
// which is re-encrypted over its only copy.
func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("renaming into place %s: %w", dst, err)
	}
	return nil
}

// EncryptFile encrypts src file to dst file using the master key. Used only for
// index files, so it binds the AADIndex domain tag.
func EncryptFile(mk *MasterKey, src, dst string) error {
	plaintext, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	ciphertext, err := mk.EncryptWithAAD(plaintext, AADIndex)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, ciphertext, 0644)
}

// DecryptFile decrypts src file to dst file using the master key. Pairs with
// EncryptFile (AADIndex domain tag).
func DecryptFile(mk *MasterKey, src, dst string) error {
	ciphertext, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	plaintext, err := mk.DecryptWithAAD(ciphertext, AADIndex)
	if err != nil {
		return err
	}
	return writeFileAtomic(dst, plaintext, 0644)
}

// --- Asymmetric (X25519 ECIES) key wrapping ---

// hkdfInfo is the info string used for HKDF key derivation in ECIES wrapping.
var hkdfInfo = []byte("disknexus-dek-wrap")

// PublicKey wraps an X25519 public key for managed encryption.
type PublicKey struct {
	key *ecdh.PublicKey
}

// PrivateKey wraps an X25519 private key for managed encryption.
type PrivateKey struct {
	key *ecdh.PrivateKey
}

// GenerateKeyPair creates a new X25519 keypair.
func GenerateKeyPair() (*PublicKey, *PrivateKey, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating X25519 keypair: %w", err)
	}
	return &PublicKey{key: priv.PublicKey()}, &PrivateKey{key: priv}, nil
}

// MasterKeyFromBytes constructs a MasterKey from a raw 32-byte slice.
func MasterKeyFromBytes(b []byte) (*MasterKey, error) {
	if len(b) != KeySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", KeySize, len(b))
	}
	mk := &MasterKey{}
	copy(mk.key[:], b)
	return mk, nil
}

// PublicKeyFromBytes parses an X25519 public key from raw bytes.
func PublicKeyFromBytes(b []byte) (*PublicKey, error) {
	key, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		return nil, fmt.Errorf("parsing X25519 public key: %w", err)
	}
	return &PublicKey{key: key}, nil
}

// PrivateKeyFromBytes parses an X25519 private key from raw bytes.
func PrivateKeyFromBytes(b []byte) (*PrivateKey, error) {
	key, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parsing X25519 private key: %w", err)
	}
	return &PrivateKey{key: key}, nil
}

// Bytes returns the raw public key bytes (32 bytes).
func (pk *PublicKey) Bytes() []byte {
	return pk.key.Bytes()
}

// Bytes returns the raw private key bytes (32 bytes).
func (pk *PrivateKey) Bytes() []byte {
	return pk.key.Bytes()
}

// Destroy zeros the private key material.
func (pk *PrivateKey) Destroy() {
	// ecdh.PrivateKey doesn't expose mutable backing memory,
	// so we nil the reference to allow GC.
	pk.key = nil
}

// WrapKeyAsymmetric encrypts a MasterKey using ECIES with X25519 + HKDF + AES-256-GCM.
// Output: [32-byte ephemeral pubkey][12-byte nonce][32+16 byte wrapped key] = 92 bytes.
func WrapKeyAsymmetric(pub *PublicKey, mk *MasterKey) ([]byte, error) {
	// 1. Generate ephemeral X25519 keypair
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating ephemeral key: %w", err)
	}

	// 2. ECDH(ephemeral_priv, recipient_pub) → shared secret
	shared, err := ephPriv.ECDH(pub.key)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	defer zeroBytes(shared)

	// 3. HKDF-SHA256(shared_secret, info) → 32-byte wrapping key
	wrappingKey, err := deriveWrappingKey(shared)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(wrappingKey)

	// 4. AES-256-GCM(wrapping_key, master_key_bytes) → ciphertext
	block, err := aes.NewCipher(wrappingKey)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}

	wrapped := gcm.Seal(nil, nonce, mk.key[:], nil)

	// 5. Assemble: ephemeral_pub || nonce || wrapped
	ephPub := ephPriv.PublicKey().Bytes()
	out := make([]byte, 0, len(ephPub)+len(nonce)+len(wrapped))
	out = append(out, ephPub...)
	out = append(out, nonce...)
	out = append(out, wrapped...)
	return out, nil
}

// UnwrapKeyAsymmetric decrypts a MasterKey from ECIES-wrapped data using the recipient's private key.
func UnwrapKeyAsymmetric(priv *PrivateKey, wrapped []byte) (*MasterKey, error) {
	// Expected: 32 (ephemeral pub) + 12 (nonce) + 32 (key) + 16 (tag) = 92
	const expectedLen = 32 + NonceSize + KeySize + TagSize
	if len(wrapped) != expectedLen {
		return nil, fmt.Errorf("wrapped key must be %d bytes, got %d", expectedLen, len(wrapped))
	}

	// 1. Parse ephemeral public key
	ephPub, err := ecdh.X25519().NewPublicKey(wrapped[:32])
	if err != nil {
		return nil, fmt.Errorf("parsing ephemeral public key: %w", err)
	}

	// 2. ECDH(recipient_priv, ephemeral_pub) → shared secret
	shared, err := priv.key.ECDH(ephPub)
	if err != nil {
		return nil, fmt.Errorf("ECDH: %w", err)
	}
	defer zeroBytes(shared)

	// 3. HKDF-SHA256(shared_secret, info) → wrapping key
	wrappingKey, err := deriveWrappingKey(shared)
	if err != nil {
		return nil, err
	}
	defer zeroBytes(wrappingKey)

	// 4. AES-256-GCM decrypt
	block, err := aes.NewCipher(wrappingKey)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	nonce := wrapped[32 : 32+NonceSize]
	ciphertext := wrapped[32+NonceSize:]
	keyBytes, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("unwrapping key: %w", err)
	}
	// Zero the plaintext key material once it's copied into the MasterKey, same
	// as the symmetric UnwrapKey twin — otherwise it lingers on the heap.
	defer zeroBytes(keyBytes)

	mk := &MasterKey{}
	copy(mk.key[:], keyBytes)
	return mk, nil
}

// deriveWrappingKey uses HKDF-SHA256 to derive a 32-byte wrapping key from a shared secret.
func deriveWrappingKey(shared []byte) ([]byte, error) {
	hkdfReader := hkdf.New(sha256.New, shared, nil, hkdfInfo)
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(hkdfReader, key); err != nil {
		return nil, fmt.Errorf("HKDF derivation: %w", err)
	}
	return key, nil
}

// publicKeyFile is the JSON representation of a saved public key.
type publicKeyFile struct {
	PublicKey []byte `json:"public_key"`
}

// SavePublicKey writes a public key to disk as JSON (0644 permissions).
func SavePublicKey(path string, pk *PublicKey) error {
	data, err := json.MarshalIndent(publicKeyFile{PublicKey: pk.Bytes()}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling public key: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing public key: %w", err)
	}
	return nil
}

// LoadPublicKey reads a public key from a JSON file.
func LoadPublicKey(path string) (*PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading public key: %w", err)
	}
	var pkf publicKeyFile
	if err := json.Unmarshal(data, &pkf); err != nil {
		return nil, fmt.Errorf("parsing public key file: %w", err)
	}
	return PublicKeyFromBytes(pkf.PublicKey)
}
