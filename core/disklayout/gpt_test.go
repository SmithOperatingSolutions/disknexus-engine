// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"unicode/utf16"
)

// buildGPT constructs a valid synthetic GPT disk image in memory: protective
// MBR, primary header+entries, partition data area, backup entries+header.
// Returns the image. Partitions are placed sequentially from FirstUsableLBA.
type synthPart struct {
	typeGUID string
	name     string
	sectors  uint64
}

func buildGPT(t *testing.T, sectorSize int, totalSectors uint64, parts []synthPart) []byte {
	t.Helper()
	ss := uint64(sectorSize)
	img := make([]byte, totalSectors*ss)

	// Protective MBR.
	img[mbrSigOffset] = 0x55
	img[mbrSigOffset+1] = 0xAA
	img[450] = 0xEE // partition type: protective GPT

	const entryCount = 128
	const entrySize = 128
	entryBytes := uint64(entryCount * entrySize)
	entrySectors := (entryBytes + ss - 1) / ss

	entryLBA := uint64(2)
	firstUsable := entryLBA + entrySectors
	alternateLBA := totalSectors - 1
	backupEntryLBA := alternateLBA - entrySectors
	lastUsable := backupEntryLBA - 1

	// Partition entries.
	entries := make([]byte, entryBytes)
	next := firstUsable
	for i, sp := range parts {
		e := entries[i*entrySize : (i+1)*entrySize]
		tg, err := ParseGUID(sp.typeGUID)
		if err != nil {
			t.Fatal(err)
		}
		copy(e[0:16], tg[:])
		// Unique GUID: deterministic per index.
		var ug GUID
		ug[0] = byte(i + 1)
		ug[8] = 0x80
		copy(e[16:32], ug[:])
		binary.LittleEndian.PutUint64(e[32:40], next)
		binary.LittleEndian.PutUint64(e[40:48], next+sp.sectors-1)
		u := utf16.Encode([]rune(sp.name))
		for j, c := range u {
			if 56+j*2+1 >= entrySize {
				break
			}
			binary.LittleEndian.PutUint16(e[56+j*2:56+j*2+2], c)
		}
		next += sp.sectors
	}
	if next-1 > lastUsable {
		t.Fatalf("test partitions exceed usable space")
	}
	copy(img[entryLBA*ss:], entries)
	copy(img[backupEntryLBA*ss:], entries)
	entriesCRC := crc32.ChecksumIEEE(entries)

	mkHeader := func(myLBA, altLBA, partLBA uint64) []byte {
		h := make([]byte, gptHeaderSize)
		copy(h[0:8], gptSignature)
		binary.LittleEndian.PutUint32(h[8:12], 0x00010000) // rev 1.0
		binary.LittleEndian.PutUint32(h[12:16], gptHeaderSize)
		binary.LittleEndian.PutUint64(h[24:32], myLBA)
		binary.LittleEndian.PutUint64(h[32:40], altLBA)
		binary.LittleEndian.PutUint64(h[40:48], firstUsable)
		binary.LittleEndian.PutUint64(h[48:56], lastUsable)
		dg, _ := ParseGUID("11111111-2222-3333-4455-66778899AABB")
		copy(h[56:72], dg[:])
		binary.LittleEndian.PutUint64(h[72:80], partLBA)
		binary.LittleEndian.PutUint32(h[80:84], entryCount)
		binary.LittleEndian.PutUint32(h[84:88], entrySize)
		binary.LittleEndian.PutUint32(h[88:92], entriesCRC)
		crc := crc32.ChecksumIEEE(h)
		binary.LittleEndian.PutUint32(h[16:20], crc)
		return h
	}
	copy(img[1*ss:], mkHeader(1, alternateLBA, entryLBA))
	copy(img[alternateLBA*ss:], mkHeader(alternateLBA, 1, backupEntryLBA))
	return img
}

func stdParts() []synthPart {
	return []synthPart{
		{TypeESP, "EFI system partition", 200},
		{TypeMSR, "Microsoft reserved partition", 32},
		{TypeMSBasicData, "Basic data partition", 2000},
		{TypeWinRE, "Recovery", 300},
	}
}

func TestParseGPT(t *testing.T) {
	img := buildGPT(t, 512, 8192, stdParts())
	l, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if l.SectorSize != 512 || l.DiskGUID != "11111111-2222-3333-4455-66778899AABB" {
		t.Fatalf("layout basics wrong: %+v", l)
	}
	if len(l.Partitions) != 4 {
		t.Fatalf("got %d partitions, want 4", len(l.Partitions))
	}
	p0 := l.Partitions[0]
	if p0.TypeGUID != TypeESP || p0.TypeName != "EFI System" || p0.Name != "EFI system partition" {
		t.Fatalf("ESP entry wrong: %+v", p0)
	}
	// Sequential placement, inclusive LastLBA.
	if p0.Length(l.SectorSize) != 200*512 {
		t.Fatalf("ESP length = %d", p0.Length(l.SectorSize))
	}
	if l.Partitions[1].FirstLBA != p0.LastLBA+1 {
		t.Fatal("partitions not sequential")
	}
	// Regions: primary covers MBR+header+entries; backup ends at disk end.
	if l.PrimaryRegion.Offset != 0 || l.PrimaryRegion.Length != int64(2*512+128*128) {
		t.Fatalf("primary region %+v", l.PrimaryRegion)
	}
	if l.BackupRegion.Offset+l.BackupRegion.Length != int64(len(img)) {
		t.Fatalf("backup region %+v does not end at disk end", l.BackupRegion)
	}
	if err := l.VerifyBackupHeader(bytes.NewReader(img)); err != nil {
		t.Fatalf("backup header should verify: %v", err)
	}
	// FindPartitionAt: middle of basic data.
	bd := l.Partitions[2]
	if got, ok := l.FindPartitionAt(bd.Offset(512) + 100); !ok || got.Index != bd.Index {
		t.Fatal("FindPartitionAt missed")
	}
}

func TestParseGPT4KSector(t *testing.T) {
	img := buildGPT(t, 4096, 2048, []synthPart{{TypeLinuxFS, "root", 500}})
	l, err := Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if l.SectorSize != 4096 || len(l.Partitions) != 1 || l.Partitions[0].TypeName != "Linux Filesystem" {
		t.Fatalf("4Kn parse wrong: ss=%d parts=%+v", l.SectorSize, l.Partitions)
	}
}

// TestParseGPTRejectsCorruption: every tamper must be caught, never silently
// parsed — a wrong layout partitions a restore target incorrectly (data loss).
func TestParseGPTRejectsCorruption(t *testing.T) {
	base := buildGPT(t, 512, 8192, stdParts())

	cases := []struct {
		name string
		mut  func(img []byte)
	}{
		{"no MBR signature", func(img []byte) { img[mbrSigOffset] = 0 }},
		{"no GPT signature", func(img []byte) { copy(img[512:], "NOTAGPT!") }},
		{"header CRC tamper", func(img []byte) { img[512+40]++ }},         // FirstUsableLBA changed without re-CRC
		{"entries tamper", func(img []byte) { img[2*512+32]++ }},          // partition FirstLBA changed without re-CRC
		{"declared CRC tamper", func(img []byte) { img[512+16] ^= 0xFF }}, // CRC field itself
	}
	for _, tc := range cases {
		img := append([]byte(nil), base...)
		tc.mut(img)
		if _, err := ParseWithSector(bytes.NewReader(img), int64(len(img)), 512); err == nil {
			t.Fatalf("%s: parse accepted corrupt GPT", tc.name)
		}
	}

	// Sanity: untampered parses.
	if _, err := ParseWithSector(bytes.NewReader(base), int64(len(base)), 512); err != nil {
		t.Fatalf("untampered image failed: %v", err)
	}
}

// TestVerifyBackupHeaderTamper: damaged backup header is detected.
func TestVerifyBackupHeaderTamper(t *testing.T) {
	img := buildGPT(t, 512, 8192, stdParts())
	l, err := ParseWithSector(bytes.NewReader(img), int64(len(img)), 512)
	if err != nil {
		t.Fatal(err)
	}
	img[int(l.AlternateLBA)*512+30]++ // tamper inside backup header
	if err := l.VerifyBackupHeader(bytes.NewReader(img)); err == nil {
		t.Fatal("tampered backup header verified")
	}
}

// TestGUIDRoundTrip pins the mixed-endian encoding with a known vector: the
// ESP type GUID's on-disk bytes per the UEFI spec.
func TestGUIDRoundTrip(t *testing.T) {
	g, err := ParseGUID(TypeESP)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := GUID{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	if g != wantBytes {
		t.Fatalf("ESP on-disk bytes = %x, want %x", g, wantBytes)
	}
	if g.String() != TypeESP {
		t.Fatalf("round-trip = %s", g.String())
	}
	if !((GUID{}).IsZero()) || g.IsZero() {
		t.Fatal("IsZero wrong")
	}
	if TypeName(g) != "EFI System" {
		t.Fatalf("TypeName = %s", TypeName(g))
	}
}
