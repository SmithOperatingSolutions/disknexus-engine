// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout_test

import (
	"bytes"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
)

// Bare-metal recovery onto a bigger disk rewrites the GPT for the new
// geometry (#69): the backup header moves to the new last sector, the
// alternate-LBA pointers and the CRCs are recomputed. The proof is that
// the relocated structures PARSE as a valid GPT of the new size with the
// same partitions — Parse validates every CRC, so a stale one fails here,
// not in a firmware that refuses to boot the restored machine.
func TestRelocateGPTProducesAValidTableOnTheLargerDisk(t *testing.T) {
	const ss = 512
	img := gpttest.BuildGPT(t, ss, 8192, gpttest.StdWindowsParts())
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	primary := img[l.PrimaryRegion.Offset : l.PrimaryRegion.Offset+l.PrimaryRegion.Length]
	backup := img[l.BackupRegion.Offset : l.BackupRegion.Offset+l.BackupRegion.Length]

	newSize := int64(16384 * ss)
	newPrimary, backupOff, newBackup, err := disklayout.RelocateGPT(l, primary, backup, newSize)
	if err != nil {
		t.Fatal(err)
	}
	if backupOff+int64(len(newBackup)) != newSize {
		t.Fatalf("backup region ends at %d, want the new disk end %d", backupOff+int64(len(newBackup)), newSize)
	}
	disk := make([]byte, newSize)
	copy(disk, newPrimary)
	copy(disk[backupOff:], newBackup)
	got, err := disklayout.Parse(bytes.NewReader(disk), newSize)
	if err != nil {
		t.Fatalf("the relocated GPT does not parse on the new disk: %v", err)
	}
	if len(got.Partitions) != len(l.Partitions) {
		t.Fatalf("relocated table has %d partitions, want %d", len(got.Partitions), len(l.Partitions))
	}
	for i, p := range l.Partitions {
		q := got.Partitions[i]
		if q.Index != p.Index || q.Offset(ss) != p.Offset(ss) || q.Length(ss) != p.Length(ss) || q.TypeName != p.TypeName {
			t.Fatalf("partition %d moved: %+v vs %+v", i, q, p)
		}
	}
	if got.BackupRegion.Offset <= l.BackupRegion.Offset {
		t.Fatalf("backup region did not move to the end of the larger disk: %d vs %d", got.BackupRegion.Offset, l.BackupRegion.Offset)
	}
	// Byte-identical primary bytes would mean the pointers were not
	// rewritten (the backup-LBA field must differ).
	if bytes.Equal(newPrimary, primary) {
		t.Fatal("the primary header was not rewritten for the new geometry")
	}

	// A disk too small for the last partition is refused, never truncated.
	if _, _, _, err := disklayout.RelocateGPT(l, primary, backup, int64(4096*ss)); err == nil {
		t.Fatal("relocating onto a disk smaller than the partitions returned no error")
	}
}
