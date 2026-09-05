// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
)

// An encrypted repository restores byte-identical with its key and refuses
// with another — the only two outcomes a key can have.
func TestEncryptedRepoRestoresWithTheKeyAndRefusesWithout(t *testing.T) {
	key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, key)
	src := noise(51, 1024<<10)
	res := w.backupBytes(src, "disk0")
	w.requirePacks(4)

	if got := sum(w.restoreBytes(res.BackupID)); got != sum(src) {
		t.Fatalf("encrypted backup does not restore to its source with the right key")
	}

	wrong, _ := crypto.GenerateMasterKey()
	got, rerr := w.tryRestore(res.BackupID, wrong)
	if rerr == nil {
		if sum(got) == sum(src) {
			t.Fatal("a DIFFERENT key restored the plaintext — the repository is not actually encrypted")
		}
		t.Fatal("restore with the wrong key returned no error — the caller would write garbage to a disk believing it succeeded")
	}
}
