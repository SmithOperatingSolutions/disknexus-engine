// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package disklayout parses and models GPT disk layouts for bare-metal
// backup/recovery (issue #67, docs/BARE_METAL_RECOVERY.md). It is pure
// parsing over an io.ReaderAt — device, image file, or byte slice — with no
// platform dependencies, so the whole package is testable on any OS against
// synthetic images.
package disklayout

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"unicode/utf16"
)

const (
	gptSignature   = "EFI PART"
	gptHeaderLBA   = 1
	mbrSigOffset   = 510
	minSectorSize  = 512
	gptHeaderSize  = 92      // bytes actually defined by rev 1.0; HeaderSize field may claim more
	maxEntryBytes  = 4 << 20 // sanity cap on the partition entry array (spec minimum is 16 KiB)
	maxSectorProbe = 4096
)

// Partition is one in-use GPT partition entry.
type Partition struct {
	Index      int    `json:"index"` // 0-based slot in the entry array
	TypeGUID   string `json:"type_guid,omitempty"`
	TypeName   string `json:"type_name"`
	PartGUID   string `json:"part_guid,omitempty"`
	Name       string `json:"name,omitempty"` // UTF-16LE name, decoded (GPT)
	FirstLBA   uint64 `json:"first_lba"`
	LastLBA    uint64 `json:"last_lba"` // inclusive, per spec
	Attributes uint64 `json:"attributes,omitempty"`

	// MBR fields (#149): set when the disk's Scheme is "mbr".
	MBRType  byte `json:"mbr_type,omitempty"`
	Bootable bool `json:"bootable,omitempty"`
}

// Offset and Length return the partition's byte range on disk.
func (p Partition) Offset(sectorSize int) int64 { return int64(p.FirstLBA) * int64(sectorSize) }
func (p Partition) Length(sectorSize int) int64 {
	return int64(p.LastLBA-p.FirstLBA+1) * int64(sectorSize)
}

// Range is a byte range on the disk.
type Range struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// DiskLayout is a parsed GPT disk.
type DiskLayout struct {
	// Scheme is "gpt" or "mbr" (#149); "" in manifests written before MBR
	// support means gpt.
	Scheme         string      `json:"scheme,omitempty"`
	SectorSize     int         `json:"sector_size"`
	DiskSize       int64       `json:"disk_size"`
	DiskGUID       string      `json:"disk_guid"`
	FirstUsableLBA uint64      `json:"first_usable_lba"`
	LastUsableLBA  uint64      `json:"last_usable_lba"`
	AlternateLBA   uint64      `json:"alternate_lba"` // backup header location
	Partitions     []Partition `json:"partitions"`

	// AuxRegions (#149, MBR only): structural sectors OUTSIDE PrimaryRegion
	// and all partitions — the EBR chain lives in inter-partition gaps and
	// must restore verbatim or logical partitions vanish.
	AuxRegions []Range `json:"aux_regions,omitempty"`

	// PrimaryRegion covers LBA0 through the end of the primary partition entry
	// array (protective MBR + primary header + entries): captured verbatim so a
	// restore reproduces boot-relevant metadata byte-exactly.
	PrimaryRegion Range `json:"primary_region"`
	// BackupRegion covers the backup entry array + backup header at the end of
	// the disk.
	BackupRegion Range `json:"backup_region"`
}

// Parse reads and validates a GPT layout from r. diskSize is the device/image
// size in bytes. The sector size is auto-detected (512 and 4096 probed) by
// locating a valid primary header; pass an explicit size via ParseWithSector
// when known.
func Parse(r io.ReaderAt, diskSize int64) (*DiskLayout, error) {
	var firstErr error
	for _, ss := range []int{512, 4096} {
		l, err := ParseWithSector(r, diskSize, ss)
		if err == nil {
			return l, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	// GPT probes failed at both sector sizes. If LBA0 carries a REAL (non-
	// protective) MBR partition table, parse it natively (#149); if it
	// carries the protective 0xEE entry, the GPT is corrupt — say that
	// instead of "no GPT signature".
	sector := make([]byte, 512)
	if _, err := r.ReadAt(sector, 0); err == nil &&
		sector[mbrSigOffset] == 0x55 && sector[mbrSigOffset+1] == 0xAA {
		entries := readMBREntries(sector)
		protective, real := false, false
		for _, e := range entries {
			switch {
			case e.typ == mbrTypeGPTProt:
				protective = true
			case e.typ != mbrTypeEmpty && e.sectors != 0:
				real = true
			}
		}
		if protective && !real {
			return nil, fmt.Errorf("corrupt GPT: protective MBR present but no valid GPT header at either sector size (%w)", firstErr)
		}
		if real {
			return parseMBR(r, diskSize, 512, sector)
		}
	}
	return nil, firstErr
}

// ParseWithSector parses a GPT with a known sector size.
func ParseWithSector(r io.ReaderAt, diskSize int64, sectorSize int) (*DiskLayout, error) {
	if sectorSize < minSectorSize || sectorSize > maxSectorProbe || sectorSize%512 != 0 {
		return nil, fmt.Errorf("unsupported sector size %d", sectorSize)
	}
	if diskSize < int64(sectorSize)*3 {
		return nil, fmt.Errorf("disk too small (%d bytes) for GPT at sector size %d", diskSize, sectorSize)
	}

	// Protective MBR must carry the boot signature (a disk with no MBR at all
	// is not a valid GPT disk per spec — and catching it here beats a confusing
	// header error).
	mbr := make([]byte, sectorSize)
	if _, err := r.ReadAt(mbr, 0); err != nil {
		return nil, fmt.Errorf("reading LBA0: %w", err)
	}
	if mbr[mbrSigOffset] != 0x55 || mbr[mbrSigOffset+1] != 0xAA {
		return nil, fmt.Errorf("no MBR boot signature at LBA0 (not a partitioned disk)")
	}

	hdr := make([]byte, sectorSize)
	if _, err := r.ReadAt(hdr, int64(gptHeaderLBA)*int64(sectorSize)); err != nil {
		return nil, fmt.Errorf("reading GPT header: %w", err)
	}
	if string(hdr[0:8]) != gptSignature {
		return nil, fmt.Errorf("no GPT signature at LBA1 (sector size %d)", sectorSize)
	}
	headerSize := binary.LittleEndian.Uint32(hdr[12:16])
	if headerSize < gptHeaderSize || int(headerSize) > sectorSize {
		return nil, fmt.Errorf("implausible GPT header size %d", headerSize)
	}

	// Header CRC: computed over HeaderSize bytes with the CRC field zeroed.
	declaredCRC := binary.LittleEndian.Uint32(hdr[16:20])
	crcInput := make([]byte, headerSize)
	copy(crcInput, hdr[:headerSize])
	crcInput[16], crcInput[17], crcInput[18], crcInput[19] = 0, 0, 0, 0
	if got := crc32.ChecksumIEEE(crcInput); got != declaredCRC {
		return nil, fmt.Errorf("GPT header CRC mismatch (got %08x, header says %08x)", got, declaredCRC)
	}

	var l DiskLayout
	l.Scheme = "gpt"
	l.SectorSize = sectorSize
	l.DiskSize = diskSize
	var diskGUID GUID
	copy(diskGUID[:], hdr[56:72])
	l.DiskGUID = diskGUID.String()
	myLBA := binary.LittleEndian.Uint64(hdr[24:32])
	l.AlternateLBA = binary.LittleEndian.Uint64(hdr[32:40])
	l.FirstUsableLBA = binary.LittleEndian.Uint64(hdr[40:48])
	l.LastUsableLBA = binary.LittleEndian.Uint64(hdr[48:56])
	entryLBA := binary.LittleEndian.Uint64(hdr[72:80])
	entryCount := binary.LittleEndian.Uint32(hdr[80:84])
	entrySize := binary.LittleEndian.Uint32(hdr[84:88])
	entriesCRC := binary.LittleEndian.Uint32(hdr[88:92])

	if myLBA != gptHeaderLBA {
		return nil, fmt.Errorf("primary GPT header claims MyLBA=%d, want 1", myLBA)
	}
	if entrySize < 128 || entrySize%8 != 0 {
		return nil, fmt.Errorf("implausible partition entry size %d", entrySize)
	}
	entryBytes := int64(entryCount) * int64(entrySize)
	if entryBytes <= 0 || entryBytes > maxEntryBytes {
		return nil, fmt.Errorf("implausible partition entry array size %d", entryBytes)
	}

	entries := make([]byte, entryBytes)
	if _, err := r.ReadAt(entries, int64(entryLBA)*int64(sectorSize)); err != nil {
		return nil, fmt.Errorf("reading partition entries: %w", err)
	}
	if got := crc32.ChecksumIEEE(entries); got != entriesCRC {
		return nil, fmt.Errorf("GPT partition entry array CRC mismatch (got %08x, header says %08x)", got, entriesCRC)
	}

	for i := 0; i < int(entryCount); i++ {
		e := entries[i*int(entrySize) : (i+1)*int(entrySize)]
		var typeGUID, partGUID GUID
		copy(typeGUID[:], e[0:16])
		if typeGUID.IsZero() {
			continue
		}
		copy(partGUID[:], e[16:32])
		p := Partition{
			Index:      i,
			TypeGUID:   typeGUID.String(),
			TypeName:   TypeName(typeGUID),
			PartGUID:   partGUID.String(),
			FirstLBA:   binary.LittleEndian.Uint64(e[32:40]),
			LastLBA:    binary.LittleEndian.Uint64(e[40:48]),
			Attributes: binary.LittleEndian.Uint64(e[48:56]),
			Name:       decodeUTF16Name(e[56:128]),
		}
		if p.LastLBA < p.FirstLBA {
			return nil, fmt.Errorf("partition %d: LastLBA %d < FirstLBA %d", i, p.LastLBA, p.FirstLBA)
		}
		if end := p.Offset(sectorSize) + p.Length(sectorSize); end > diskSize {
			return nil, fmt.Errorf("partition %d extends past end of disk (%d > %d)", i, end, diskSize)
		}
		l.Partitions = append(l.Partitions, p)
	}

	// Verbatim capture regions for byte-exact restore.
	entriesEnd := int64(entryLBA)*int64(sectorSize) + entryBytes
	l.PrimaryRegion = Range{Offset: 0, Length: entriesEnd}
	// Backup: entry array copy sits before the backup header (at AlternateLBA).
	backupStart := int64(l.AlternateLBA)*int64(sectorSize) - entryBytes
	l.BackupRegion = Range{
		Offset: backupStart,
		Length: int64(l.AlternateLBA)*int64(sectorSize) + int64(sectorSize) - backupStart,
	}
	if l.BackupRegion.Offset < 0 || l.BackupRegion.Offset+l.BackupRegion.Length > diskSize {
		return nil, fmt.Errorf("backup GPT region [%d,+%d) out of disk bounds %d", l.BackupRegion.Offset, l.BackupRegion.Length, diskSize)
	}

	return &l, nil
}

// decodeUTF16Name decodes a NUL-terminated UTF-16LE partition name.
func decodeUTF16Name(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		c := binary.LittleEndian.Uint16(b[i : i+2])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

// VerifyBackupHeader cross-checks that the backup GPT header at AlternateLBA is
// present, valid, and agrees with the primary (same disk GUID). Damaged backup
// headers are common on cloned disks; callers decide whether to warn or fail.
func (l *DiskLayout) VerifyBackupHeader(r io.ReaderAt) error {
	hdr := make([]byte, l.SectorSize)
	if _, err := r.ReadAt(hdr, int64(l.AlternateLBA)*int64(l.SectorSize)); err != nil {
		return fmt.Errorf("reading backup GPT header: %w", err)
	}
	if string(hdr[0:8]) != gptSignature {
		return fmt.Errorf("backup GPT header missing at LBA %d", l.AlternateLBA)
	}
	headerSize := binary.LittleEndian.Uint32(hdr[12:16])
	if headerSize < gptHeaderSize || int(headerSize) > l.SectorSize {
		return fmt.Errorf("backup GPT header implausible size %d", headerSize)
	}
	declared := binary.LittleEndian.Uint32(hdr[16:20])
	in := make([]byte, headerSize)
	copy(in, hdr[:headerSize])
	in[16], in[17], in[18], in[19] = 0, 0, 0, 0
	if got := crc32.ChecksumIEEE(in); got != declared {
		return fmt.Errorf("backup GPT header CRC mismatch")
	}
	var g GUID
	copy(g[:], hdr[56:72])
	if g.String() != l.DiskGUID {
		return fmt.Errorf("backup GPT header disk GUID %s != primary %s", g, l.DiskGUID)
	}
	return nil
}

// FindPartitionAt returns the partition covering the given byte offset, if any.
func (l *DiskLayout) FindPartitionAt(off int64) (Partition, bool) {
	for _, p := range l.Partitions {
		if off >= p.Offset(l.SectorSize) && off < p.Offset(l.SectorSize)+p.Length(l.SectorSize) {
			return p, true
		}
	}
	return Partition{}, false
}
