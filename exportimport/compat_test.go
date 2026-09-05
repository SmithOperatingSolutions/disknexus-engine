// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/exportimport"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// Import used to write the archive's frames into the destination repo
// verbatim, with no check that source and destination agree about how a chunk
// is encrypted or how its identity is computed. A plaintext chunk therefore
// landed in a managed-encryption repo and Import returned success; the
// manifests were installed, so the panel listed a backup whose bytes sat
// readable in an allegedly-encrypted repo.
//
// These tests assert on the BYTES in the destination, not only on an error:
// the second half of the same defect was that the one refusal that did exist
// (the index rebuild's AEAD failure on a passphrase repo) fired only AFTER
// every frame had already been appended to a pack file, with no rollback.

// initRepo creates a repo whose stored config is rc (geometry filled in from
// the defaults so the chunker behaves like a real repo).
func initRepo(t *testing.T, rc store.RepoConfig) string {
	t.Helper()
	base := config.Default()
	if rc.ChunkMinSize == 0 {
		rc.ChunkMinSize = base.ChunkMinSize
		rc.ChunkAvgSize = base.ChunkAvgSize
		rc.ChunkMaxSize = base.ChunkMaxSize
		rc.BuzhashMask = base.BuzhashMask
	}
	if rc.PackFileMaxSize == 0 {
		rc.PackFileMaxSize = base.PackFileMaxSize
	}
	if rc.CompressionLevel == 0 {
		rc.CompressionLevel = base.CompressionLevel
	}
	repoPath := filepath.Join(t.TempDir(), "repo")
	if err := store.InitRepo(repoPath, rc); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	return repoPath
}

// backupInto runs a real backup of data into repoPath, bound to that repo's
// config and key exactly as the CLI binds it.
func backupInto(t *testing.T, repoPath string, rc store.RepoConfig, key *crypto.MasterKey, data []byte) string {
	t.Helper()
	cfg := config.Default()
	rc.ApplyTo(&cfg)
	sourcePath := filepath.Join(t.TempDir(), "source.img")
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatal(err)
	}
	p := pipeline.New(cfg, newLogger(), pipeline.MustBind(rc, key))
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	res, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return res.BackupID
}

// exportOf backs up data into a fresh source repo of shape rc and returns the
// path of a zip holding that backup.
func exportOf(t *testing.T, rc store.RepoConfig, key *crypto.MasterKey, data []byte) string {
	t.Helper()
	srcRepo := initRepo(t, rc)
	id := backupInto(t, srcRepo, rc, key, data)
	zipPath := filepath.Join(t.TempDir(), "archive.zip")
	if err := exportimport.Export(srcRepo, []string{id}, zipPath, key); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return zipPath
}

func mustKey(t *testing.T) *crypto.MasterKey {
	t.Helper()
	k, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// repoPackBytes is the total size of the destination's pack files. A refusal
// that fires after the frames were appended still leaves them on disk
// forever, so "no bytes landed" is a separate assertion from "Import failed".
func repoPackBytes(t *testing.T, repoPath string) int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "chunks"))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func manifestCount(t *testing.T, repoPath string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "manifests"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".dnm") {
			n++
		}
	}
	return n
}

// packsContain reports whether any pack file contains needle verbatim.
func packsContain(t *testing.T, repoPath string, needle []byte) bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoPath, "chunks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(repoPath, "chunks", e.Name()))
		if err != nil {
			continue
		}
		if bytes.Contains(b, needle) {
			return true
		}
	}
	return false
}

// assertNothingLanded is the whole point of refusing BEFORE the chunk store is
// opened: a rejected archive must leave the destination byte-identical.
func assertNothingLanded(t *testing.T, repoPath string) {
	t.Helper()
	if n := repoPackBytes(t, repoPath); n != 0 {
		t.Errorf("refused import still left %d pack bytes in the destination — the refusal must precede the write, "+
			"there is no rollback for frames already appended to a pack", n)
	}
	if n := manifestCount(t, repoPath); n != 0 {
		t.Errorf("refused import installed %d manifest(s) — a listable backup whose chunks are not there", n)
	}
}

var managedRC = store.RepoConfig{Encrypted: true, EncryptionMode: store.EncryptManaged}
var passphraseRC = store.RepoConfig{Encrypted: true, EncryptionMode: store.EncryptPassphrase}

// TestImportRefusesAPlaintextArchiveIntoAnEncryptedRepo is the #265 invariant
// ("a backup that cannot encrypt must FAIL LOUDLY") in the path the original
// fix did not touch. Import never encrypts — StoreRaw copies the frame
// byte-for-byte — so an archive from an unencrypted repo is plaintext data
// being written into an encrypted repo.
func TestImportRefusesAPlaintextArchiveIntoAnEncryptedRepo(t *testing.T) {
	needle := []byte("IMPORT-PLAINTEXT-NEEDLE-XYZZY")
	data := make([]byte, 256*1024)
	rand.Read(data)
	copy(data[1024:], needle)

	zipPath := exportOf(t, store.RepoConfig{}, nil, data)

	for _, tc := range []struct {
		name string
		rc   store.RepoConfig
	}{
		{"managed", managedRC},
		{"passphrase", passphraseRC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := initRepo(t, tc.rc)
			err := exportimport.Import(context.Background(), dest, zipPath, mustKey(t))
			if err == nil {
				t.Errorf("Import of a PLAINTEXT archive into a %s-encryption repo returned success", tc.name)
			}
			if packsContain(t, dest, needle) {
				t.Errorf("the %s repo's packs contain the plaintext needle %q verbatim", tc.name, needle)
			}
			assertNothingLanded(t, dest)
		})
	}
}

// TestImportRefusesAnEncryptedArchiveIntoAnUnencryptedRepo is the mirror
// image: ciphertext frames written into a repo every reader opens with a nil
// key, where they decompress to nothing.
func TestImportRefusesAnEncryptedArchiveIntoAnUnencryptedRepo(t *testing.T) {
	data := make([]byte, 256*1024)
	rand.Read(data)
	zipPath := exportOf(t, managedRC, mustKey(t), data)

	dest := initRepo(t, store.RepoConfig{})
	if err := exportimport.Import(context.Background(), dest, zipPath, nil); err == nil {
		t.Error("Import of an ENCRYPTED archive into an unencrypted repo returned success")
	}
	assertNothingLanded(t, dest)
}

// TestImportRefusesAnArchiveEncryptedUnderADifferentKey: agreeing on the MODE
// is not enough. Two managed repos have two different DEKs, so frames from one
// authenticate under neither the other's key nor a nil key.
func TestImportRefusesAnArchiveEncryptedUnderADifferentKey(t *testing.T) {
	data := make([]byte, 256*1024)
	rand.Read(data)
	zipPath := exportOf(t, managedRC, mustKey(t), data)

	dest := initRepo(t, managedRC)
	if err := exportimport.Import(context.Background(), dest, zipPath, mustKey(t)); err == nil {
		t.Error("Import accepted frames encrypted under a DIFFERENT key")
	}
	assertNothingLanded(t, dest)
}

// peImage is a minimal but PLAUSIBLE PE image: the PE normalizer validates the
// COFF header before touching anything, so only a well-formed one actually
// changes the hash input.
func peImage(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + 13)
	}
	const peOff = 0x80
	data[0], data[1] = 'M', 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:], peOff)
	copy(data[peOff:], []byte{'P', 'E', 0, 0})
	coff := peOff + 4
	binary.LittleEndian.PutUint16(data[coff:], 0x8664)       // Machine: amd64
	binary.LittleEndian.PutUint16(data[coff+2:], 3)          // NumberOfSections
	binary.LittleEndian.PutUint32(data[coff+4:], 0xDEADBEEF) // TimeDateStamp (zeroed by the normalizer)
	binary.LittleEndian.PutUint16(data[coff+16:], 0)         // SizeOfOptionalHeader
	return data
}

// TestImportRefusesWhenChunkIdentityDisagrees: chunk identity is the hash of
// NORMALIZED bytes while ORIGINAL bytes are stored, so importing chunks whose
// identity was computed without a normalizer into a repo that declares one
// files them under hashes this repo's read path can never reproduce — every
// restore fails the integrity check, and the dedup index is poisoned.
func TestImportRefusesWhenChunkIdentityDisagrees(t *testing.T) {
	data := peImage(256 * 1024)
	zipPath := exportOf(t, store.RepoConfig{}, nil, data)

	dest := initRepo(t, store.RepoConfig{Normalizers: []string{"pe"}})
	err := exportimport.Import(context.Background(), dest, zipPath, nil)
	if err == nil {
		t.Error("Import accepted chunks whose identity was computed under a different normalizer")
	}
	assertNothingLanded(t, dest)
}

// TestImportAcceptsAMatchingArchive is the regression guard on the refusal:
// the check must not cost the ordinary import its success. Same mode, same
// key, same normalizers — the archive is exactly what the destination can
// read, and it goes in.
func TestImportAcceptsAMatchingArchive(t *testing.T) {
	data := make([]byte, 256*1024)
	rand.Read(data)

	t.Run("unencrypted", func(t *testing.T) {
		zipPath := exportOf(t, store.RepoConfig{}, nil, data)
		dest := initRepo(t, store.RepoConfig{})
		if err := exportimport.Import(context.Background(), dest, zipPath, nil); err != nil {
			t.Fatalf("Import of a matching archive failed: %v", err)
		}
		if manifestCount(t, dest) != 1 {
			t.Error("matching import installed no manifest")
		}
	})

	t.Run("same-key-passphrase", func(t *testing.T) {
		key := mustKey(t)
		zipPath := exportOf(t, passphraseRC, key, data)
		dest := initRepo(t, passphraseRC)
		if err := exportimport.Import(context.Background(), dest, zipPath, key); err != nil {
			t.Fatalf("Import of an archive encrypted under this repo's own key failed: %v", err)
		}
		if manifestCount(t, dest) != 1 {
			t.Error("matching import installed no manifest")
		}
	})

	t.Run("same-key-managed", func(t *testing.T) {
		key := mustKey(t)
		zipPath := exportOf(t, managedRC, key, data)
		dest := initRepo(t, managedRC)
		if err := exportimport.Import(context.Background(), dest, zipPath, key); err != nil {
			t.Fatalf("Import into a managed repo holding the same DEK failed: %v", err)
		}
		if repoPackBytes(t, dest) == 0 {
			t.Error("matching managed import stored no chunks")
		}
	})

	t.Run("same-normalizer", func(t *testing.T) {
		pe := store.RepoConfig{Normalizers: []string{"pe"}}
		zipPath := exportOf(t, pe, nil, peImage(256*1024))
		dest := initRepo(t, pe)
		if err := exportimport.Import(context.Background(), dest, zipPath, nil); err != nil {
			t.Fatalf("Import of an archive from a repo with the SAME normalizer failed: %v", err)
		}
	})
}

// TestImportRefusesAnEncryptedRepoWithNoKey closes the one way the frame check
// can be talked out of doing its job: verification uses the key the CALLER
// supplied, so a caller that hands Import a nil key for an encrypted repo
// makes plaintext frames verify successfully and land. The repo config, not
// the caller, decides whether a key was required — the same rule pipeline.Bind
// enforces on every other write path (#265).
func TestImportRefusesAnEncryptedRepoWithNoKey(t *testing.T) {
	data := make([]byte, 256*1024)
	rand.Read(data)
	zipPath := exportOf(t, store.RepoConfig{}, nil, data)

	for _, tc := range []struct {
		name string
		rc   store.RepoConfig
	}{
		{"managed", managedRC},
		{"passphrase", passphraseRC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := initRepo(t, tc.rc)
			if err := exportimport.Import(context.Background(), dest, zipPath, nil); err == nil {
				t.Errorf("Import into a %s repo with NO key returned success", tc.name)
			}
			assertNothingLanded(t, dest)
		})
	}
}
