// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import (
	"context"
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
)

// Verify holds a clean backup and condemns a silently corrupted one. The
// corruption is the test's own: bytes flipped in the middle of a pack the
// backup references, the disk-rot case no checksum on the wire catches.
// Positive control first (§4): the same verify passes before the flip.
func TestVerifyCatchesASilentlyCorruptedPack(t *testing.T) {
	w := newWorld(t)
	res := w.backupBytes(noise(41, 1024<<10), "disk0")
	w.requirePacks(4)
	b := w.load(res.BackupID)

	idx, cs := w.open()
	clean, err := restore.Verify(context.Background(), b, idx, cs)
	if err != nil {
		t.Fatalf("Verify (clean): %v", err)
	}
	if len(clean.Errors) != 0 || clean.VerifiedChunks != clean.TotalChunks {
		t.Fatalf("positive control failed: clean backup verified %d/%d with %d errors", clean.VerifiedChunks, clean.TotalChunks, len(clean.Errors))
	}

	// Rot: flip 64 bytes in the middle of the second pack.
	packs := w.packFiles()
	f, err := os.OpenFile(packs[1], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	buf := make([]byte, 64)
	if _, err := f.ReadAt(buf, st.Size()/2); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] ^= 0xFF
	}
	if _, err := f.WriteAt(buf, st.Size()/2); err != nil {
		t.Fatal(err)
	}
	f.Close()

	idx2, cs2 := w.open()
	rot, err := restore.Verify(context.Background(), b, idx2, cs2)
	if err == nil && len(rot.Errors) == 0 {
		t.Fatalf("verify passed a backup whose pack %s was corrupted in place — silent disk rot would be reported as a good backup", packs[1])
	}
	if rot != nil && rot.VerifiedChunks == rot.TotalChunks && len(rot.Errors) == 0 {
		t.Fatalf("verify counted every chunk verified on a corrupted pack")
	}
	// And a restore of the corrupted backup must not hand back the source.
	if got, rerr := w.tryRestore(res.BackupID, nil); rerr == nil && sum(got) == sum(noise(41, 1024<<10)) {
		t.Fatal("restore produced the original bytes from a corrupted pack — the corruption did not land where the backup reads (§2)")
	}
}
