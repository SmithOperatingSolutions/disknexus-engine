// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package gpttest builds synthetic, spec-valid GPT disk images for tests
// (correct header/entry CRCs, protective MBR, backup structures). It is
// deliberately self-contained (no disklayout import) so both disklayout's own
// tests and higher-level consumers (bmr) can use it without import cycles.
package gpttest

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
	"unicode/utf16"
)

// Well-known type GUIDs (duplicated string constants; the parser package pins
// the same values against real sgdisk output).
const (
	TypeESP         = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	TypeMSR         = "E3C9E316-0B5C-4DB8-817D-F92DF00215AE"
	TypeMSBasicData = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7"
	TypeWinRE       = "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC"
	TypeLinuxFS     = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
)

// SynthPart describes one partition for BuildGPT; partitions are placed
// sequentially from FirstUsableLBA.
type SynthPart struct {
	TypeGUID string
	Name     string
	Sectors  uint64
}

// guidBytes converts canonical GUID text to on-disk mixed-endian bytes.
func guidBytes(t *testing.T, s string) [16]byte {
	t.Helper()
	clean := strings.ReplaceAll(strings.ToUpper(s), "-", "")
	if len(clean) != 32 {
		t.Fatalf("bad guid %q", s)
	}
	var raw [16]byte
	for i := 0; i < 16; i++ {
		var b byte
		if _, err := fmt.Sscanf(clean[i*2:i*2+2], "%02X", &b); err != nil {
			t.Fatalf("bad guid %q: %v", s, err)
		}
		raw[i] = b
	}
	var g [16]byte
	g[0], g[1], g[2], g[3] = raw[3], raw[2], raw[1], raw[0]
	g[4], g[5] = raw[5], raw[4]
	g[6], g[7] = raw[7], raw[6]
	copy(g[8:], raw[8:])
	return g
}

// BuildGPT constructs a valid synthetic GPT disk image in memory: protective
// MBR, primary header+entries, partition data area, backup entries+header.
func BuildGPT(t *testing.T, sectorSize int, totalSectors uint64, parts []SynthPart) []byte {
	t.Helper()
	ss := uint64(sectorSize)
	img := make([]byte, totalSectors*ss)

	// Protective MBR.
	img[510] = 0x55
	img[511] = 0xAA
	img[450] = 0xEE

	const entryCount = 128
	const entrySize = 128
	entryBytes := uint64(entryCount * entrySize)
	entrySectors := (entryBytes + ss - 1) / ss

	entryLBA := uint64(2)
	firstUsable := entryLBA + entrySectors
	alternateLBA := totalSectors - 1
	backupEntryLBA := alternateLBA - entrySectors
	lastUsable := backupEntryLBA - 1

	entries := make([]byte, entryBytes)
	next := firstUsable
	for i, sp := range parts {
		e := entries[i*entrySize : (i+1)*entrySize]
		tg := guidBytes(t, sp.TypeGUID)
		copy(e[0:16], tg[:])
		var ug [16]byte
		ug[0] = byte(i + 1)
		ug[8] = 0x80
		copy(e[16:32], ug[:])
		binary.LittleEndian.PutUint64(e[32:40], next)
		binary.LittleEndian.PutUint64(e[40:48], next+sp.Sectors-1)
		u := utf16.Encode([]rune(sp.Name))
		for j, c := range u {
			if 56+j*2+1 >= entrySize {
				break
			}
			binary.LittleEndian.PutUint16(e[56+j*2:56+j*2+2], c)
		}
		next += sp.Sectors
	}
	if next-1 > lastUsable {
		t.Fatalf("test partitions exceed usable space")
	}
	copy(img[entryLBA*ss:], entries)
	copy(img[backupEntryLBA*ss:], entries)
	entriesCRC := crc32.ChecksumIEEE(entries)

	mkHeader := func(myLBA, altLBA, partLBA uint64) []byte {
		h := make([]byte, 92)
		copy(h[0:8], "EFI PART")
		binary.LittleEndian.PutUint32(h[8:12], 0x00010000)
		binary.LittleEndian.PutUint32(h[12:16], 92)
		binary.LittleEndian.PutUint64(h[24:32], myLBA)
		binary.LittleEndian.PutUint64(h[32:40], altLBA)
		binary.LittleEndian.PutUint64(h[40:48], firstUsable)
		binary.LittleEndian.PutUint64(h[48:56], lastUsable)
		dg := guidBytes(t, "11111111-2222-3333-4455-66778899AABB")
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

// StdWindowsParts is the standard Windows boot-disk shape: ESP, MSR, basic
// data, WinRE.
func StdWindowsParts() []SynthPart {
	return []SynthPart{
		{TypeESP, "EFI system partition", 200},
		{TypeMSR, "Microsoft reserved partition", 32},
		{TypeMSBasicData, "Basic data partition", 2000},
		{TypeWinRE, "Recovery", 300},
	}
}
