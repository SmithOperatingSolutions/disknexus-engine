// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

// #370: an index opened WITHOUT the repo key must refuse an ENCRYPTED index,
// not read it as an EMPTY one.
//
// The user-visible failure this pins: hash-index.db.enc / bloom.bin.enc are
// only looked at when a key is supplied, so a keyless open ignores them, finds
// no plaintext beside them, and opens an index that has forgotten every chunk
// the repository holds. Nothing catches it — BloomSuspect is
// "bloom missing AND Count() > 0", and the count is zero for the same reason.
// The next backup then publishes that amnesiac index over the real one and the
// EARLIER backup stops being restorable ("chunk N not found in index").
//
// The delta path already refuses exactly this (deltaapply.go, "index delta %s
// is encrypted and this index was opened without a key"). The authoritative
// index open is the one place that does not.

// encryptedIndexFixture builds a real encrypted index at rest: entries written
// through the ordinary API with a key, closed, leaving .enc objects and no
// plaintext. Returns the directory, its key, and the chunks it holds.
func encryptedIndexFixture(t *testing.T) (string, *crypto.MasterKey, []hasher.ChunkID) {
	t.Helper()
	dir := t.TempDir()
	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	t.Cleanup(mk.Destroy)

	idx, err := index.NewDedupIndex(dir, 1000, 0.01, 1, mk)
	if err != nil {
		t.Fatalf("building the encrypted index: %v", err)
	}
	ids := make([]hasher.ChunkID, 12)
	for i := range ids {
		ids[i] = hasher.Sum([]byte{byte(i), byte(i + 7), 0xA5})
		idx.Insert(ids[i], uint32(i%3), uint64(8+i*100), 90)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("closing the encrypted index: %v", err)
	}

	// Interrogate the fixture. If a plaintext hash-index.db or bloom.bin were
	// left beside the .enc objects, a keyless open would load THAT and this
	// test would say nothing at all about #370.
	for _, name := range []string{"hash-index.db.enc", "bloom.bin.enc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("fixture is not an encrypted index: %s missing (%v)", name, err)
		}
	}
	for _, name := range []string{"hash-index.db", "bloom.bin"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("fixture left plaintext %s beside the .enc objects — a keyless open would read it "+
				"and this test would prove nothing", name)
		}
	}
	return dir, mk, ids
}

// freshCopy hands each open its own copy of the fixture, so one open's side
// effects (a keyless open creates an empty plaintext hash-index.db) cannot
// change what the next one sees.
func freshCopy(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestEncryptedIndexOpenedWithoutTheRepoKeyRefusesInsteadOfReadingEmpty(t *testing.T) {
	fixture, mk, ids := encryptedIndexFixture(t)

	opens := []struct {
		name string
		open func(dir string, key *crypto.MasterKey) (*index.DedupIndex, error)
	}{
		{"NewDedupIndex", func(dir string, key *crypto.MasterKey) (*index.DedupIndex, error) {
			return index.NewDedupIndex(dir, index.ReadOpenExpectedChunks, 0.01, 1, key)
		}},
		{"NewDedupIndexReadOnly", func(dir string, key *crypto.MasterKey) (*index.DedupIndex, error) {
			return index.NewDedupIndexReadOnly(dir, index.ReadOpenExpectedChunks, 0.01, 1, key)
		}},
	}

	for _, o := range opens {
		t.Run(o.name, func(t *testing.T) {
			// POSITIVE CONTROL, first: the very same directory, opened WITH the
			// repo key, must hand back every chunk. Without this the refusal
			// below would pass on an unreadable, empty or corrupt fixture.
			withKey, err := o.open(freshCopy(t, fixture), mk)
			if err != nil {
				t.Fatalf("control: %s with the repo key failed: %v", o.name, err)
			}
			for _, id := range ids {
				if _, found, err := withKey.LookupDirect(id.StrongHash); err != nil || !found {
					withKey.CloseDiscard()
					t.Fatalf("control: %s with the repo key cannot resolve chunk %x (found=%v err=%v)",
						o.name, id.StrongHash[:8], found, err)
				}
			}
			// ...and it is an index, not a yes-machine: a hash the fixture never
			// stored must still miss.
			if _, found, _ := withKey.LookupDirect(hasher.Sum([]byte("never stored")).StrongHash); found {
				withKey.CloseDiscard()
				t.Fatalf("control: %s resolves a chunk that was never inserted", o.name)
			}
			withKey.CloseDiscard()

			// The red: the same directory, no key.
			keyless, err := o.open(freshCopy(t, fixture), nil)
			if err == nil {
				entries := keyless.Stats().IndexEntries
				_, found, _ := keyless.LookupDirect(ids[0].StrongHash)
				suspect := keyless.BloomSuspect()
				keyless.CloseDiscard()
				t.Fatalf("%s opened an ENCRYPTED index with no key and reported success: %d entries, "+
					"chunk %x found=%v, BloomSuspect=%v. The repository holds %d chunks. A backup written "+
					"against this index publishes one that has forgotten every prior chunk, and the earlier "+
					"backup can no longer be restored (\"chunk N not found in index\"). It must refuse and "+
					"name the repo key, the way deltaapply.go does for an .enc delta.",
					o.name, entries, ids[0].StrongHash[:8], found, suspect, len(ids))
			}
			// Not any error: the one that tells the operator what is missing.
			if !strings.Contains(err.Error(), "store.IndexKeyFor") {
				t.Fatalf("%s refused, but the message does not name the missing input (store.IndexKeyFor): %v",
					o.name, err)
			}
		})
	}
}
