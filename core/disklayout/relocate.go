// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// RelocateGPT adapts a captured disk's verbatim GPT regions to a LARGER target
// disk (#76): real drive replacements are rarely the same size, and a backup
// GPT header that is not at the last LBA is flagged as corruption by firmware
// and partitioning tools.
//
// It returns:
//   - primary: the captured primary region with ONLY the header's AlternateLBA
//     patched to the new last LBA (and the header CRC recomputed) — everything
//     else, including the protective MBR, disk GUID, partition entries and
//     their GUIDs, stays byte-identical, which is what keeps BCD/fstab
//     references valid.
//   - backupOffset/backup: the rebuilt backup structures (entry-array copy +
//     backup header) positioned at the true end of the new disk.
//
// The extra space beyond the captured LastUsableLBA is left unallocated
// (partition growth is a deliberate non-goal here; growpart/ntfsresize on the
// recovery key cover it). Same-size targets pass through unchanged (verbatim
// backup at its captured offset). Smaller targets are refused.
func RelocateGPT(l *DiskLayout, capturedPrimary, capturedBackup []byte, newDiskSize int64) (primary []byte, backupOffset int64, backup []byte, err error) {
	// MBR (#149): nothing lives at the end of the disk — the boot track
	// restores verbatim wherever the disk size lands.
	if l.Scheme == "mbr" {
		return capturedPrimary, 0, nil, nil
	}
	ss := int64(l.SectorSize)
	if newDiskSize%ss != 0 {
		return nil, 0, nil, fmt.Errorf("target size %d is not a multiple of the captured sector size %d", newDiskSize, l.SectorSize)
	}
	if newDiskSize < l.DiskSize {
		return nil, 0, nil, fmt.Errorf("target (%d bytes) is smaller than the captured disk (%d bytes); shrinking restores are not supported", newDiskSize, l.DiskSize)
	}
	if int64(len(capturedPrimary)) != l.PrimaryRegion.Length || int64(len(capturedBackup)) != l.BackupRegion.Length {
		return nil, 0, nil, fmt.Errorf("captured region sizes do not match the layout")
	}
	if newDiskSize == l.DiskSize {
		return capturedPrimary, l.BackupRegion.Offset, capturedBackup, nil
	}

	// Pull geometry facts from the captured primary header.
	hdrOff := ss // LBA1
	if int64(len(capturedPrimary)) < hdrOff+int64(gptHeaderSize) {
		return nil, 0, nil, fmt.Errorf("primary region too short for a GPT header")
	}
	hdr := capturedPrimary[hdrOff:]
	headerSize := binary.LittleEndian.Uint32(hdr[12:16])
	entryCount := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	entryBytes := int64(entryCount) * int64(entrySize)
	entrySectors := (entryBytes + ss - 1) / ss

	newLastLBA := newDiskSize/ss - 1
	newBackupEntryLBA := newLastLBA - entrySectors

	// 1) Primary region: verbatim copy, then patch AlternateLBA + re-CRC.
	primary = append([]byte(nil), capturedPrimary...)
	pHdr := primary[hdrOff : hdrOff+int64(headerSize)]
	binary.LittleEndian.PutUint64(pHdr[32:40], uint64(newLastLBA))
	recomputeHeaderCRC(pHdr)

	// 2) Backup structures at the new end: the entry-array copy followed by the
	// backup header (mirroring the captured backup region's shape, relocated).
	// The captured backup region is [entries][header]; reuse its entry bytes so
	// the array stays byte-identical, and rebuild the header from the (patched)
	// primary with the roles swapped.
	entries := capturedBackup[:entryBytes]
	backup = make([]byte, entryBytes+ss)
	copy(backup, entries)
	bHdr := backup[entryBytes : entryBytes+int64(headerSize)]
	copy(bHdr, pHdr)
	binary.LittleEndian.PutUint64(bHdr[24:32], uint64(newLastLBA))        // MyLBA
	binary.LittleEndian.PutUint64(bHdr[32:40], 1)                         // AlternateLBA -> primary
	binary.LittleEndian.PutUint64(bHdr[72:80], uint64(newBackupEntryLBA)) // entry array LBA
	recomputeHeaderCRC(bHdr)

	return primary, newBackupEntryLBA * ss, backup, nil
}

// recomputeHeaderCRC zeroes and refills the header CRC field over HeaderSize.
func recomputeHeaderCRC(hdr []byte) {
	hdr[16], hdr[17], hdr[18], hdr[19] = 0, 0, 0, 0
	crc := crc32.ChecksumIEEE(hdr)
	binary.LittleEndian.PutUint32(hdr[16:20], crc)
}
