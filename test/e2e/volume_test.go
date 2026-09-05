// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

import "testing"

// A volume backup restores byte-identical, full and incremental alike, and
// the incremental actually deduplicated against its parent. The authority
// is the SHA-256 of the source the test generated — the engine never sees
// it.
func TestVolumeBackupRestoresByteIdentical(t *testing.T) {
	w := newWorld(t)
	v1 := noise(1, 1536<<10) // 1.5 MB, motif-repeated
	full := w.backupBytes(v1, "disk0")
	w.requirePacks(4)
	if full.DedupChunks == 0 {
		t.Fatal("fixture defect: the source produced no dedup hits, so the index path is untested (§2)")
	}
	if got := sum(w.restoreBytes(full.BackupID)); got != sum(v1) {
		t.Fatalf("full restore differs from source: got %s want %s", got[:12], sum(v1)[:12])
	}

	// Change one region in the middle; everything else must dedup against
	// the parent and BOTH generations must still restore exactly.
	v2 := append([]byte(nil), v1...)
	copy(v2[700<<10:], noise(2, 96<<10))
	inc := w.backupIncremental(v2, "disk0", full.BackupID)
	if inc.ParentBackupID != full.BackupID {
		t.Fatalf("incremental parent = %q, want %q", inc.ParentBackupID, full.BackupID)
	}
	if inc.UnchangedChunks == 0 || inc.UnchangedChunks >= inc.TotalChunks {
		t.Fatalf("incremental unchanged=%d of %d — a 96 KB edit in 1.5 MB must leave most chunks unchanged and some changed", inc.UnchangedChunks, inc.TotalChunks)
	}
	if got := sum(w.restoreBytes(inc.BackupID)); got != sum(v2) {
		t.Fatalf("incremental restore differs from its source")
	}
	if got := sum(w.restoreBytes(full.BackupID)); got != sum(v1) {
		t.Fatalf("the parent no longer restores to ITS bytes after a child was written — the child corrupted shared chunks")
	}
}
