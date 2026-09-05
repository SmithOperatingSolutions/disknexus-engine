// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package gpttest

import (
	"encoding/binary"
	"testing"
)

// SynthMBRPart describes one synthetic MBR partition for BuildMBR.
type SynthMBRPart struct {
	Type     byte // partition type byte (0x07 NTFS, 0x0C FAT32, ...)
	Sectors  uint64
	Bootable bool
	Logical  bool // carved inside the extended partition (EBR chain)
}

// BuildMBR builds a synthetic MBR disk image: primaries at 0x1BE, an
// extended partition (type 0x0F) containing an EBR chain when any part is
// Logical. Layout: 2048-sector alignment, boot code region left zeroed
// except a recognizable marker so boot-track capture tests can assert
// byte-exact restore.
func BuildMBR(t *testing.T, sectorSize int, totalSectors uint64, parts []SynthMBRPart) []byte {
	t.Helper()
	img := make([]byte, int(totalSectors)*sectorSize)
	// Marker "fake boot code" so tests can verify the boot track round-trips.
	copy(img[0:], []byte{0xFA, 0x33, 0xC0, 0x8E, 0xD0, 0xBC, 0x00, 0x7C})
	img[510], img[511] = 0x55, 0xAA

	writeEntry := func(base []byte, slot int, typ byte, bootable bool, startLBA, sectors uint32) {
		e := base[0x1BE+slot*16 : 0x1BE+slot*16+16]
		if bootable {
			e[0] = 0x80
		}
		e[4] = typ
		binary.LittleEndian.PutUint32(e[8:12], startLBA)
		binary.LittleEndian.PutUint32(e[12:16], sectors)
	}

	var primaries, logicals []SynthMBRPart
	for _, p := range parts {
		if p.Logical {
			logicals = append(logicals, p)
		} else {
			primaries = append(primaries, p)
		}
	}
	if len(primaries) > 3 && len(logicals) > 0 {
		t.Fatal("BuildMBR: max 3 primaries when an extended partition is needed")
	}

	cursor := uint64(2048)
	slot := 0
	for _, p := range primaries {
		writeEntry(img, slot, p.Type, p.Bootable, uint32(cursor), uint32(p.Sectors))
		cursor += p.Sectors
		if r := cursor % 2048; r != 0 {
			cursor += 2048 - r
		}
		slot++
	}

	if len(logicals) > 0 {
		extStart := cursor
		// Each logical: one EBR sector block (2048 aligned) then the data.
		type placed struct{ ebr, data, sectors uint64 }
		var plan []placed
		for _, l := range logicals {
			ebr := cursor
			data := ebr + 2048
			cursor = data + l.Sectors
			if r := cursor % 2048; r != 0 {
				cursor += 2048 - r
			}
			plan = append(plan, placed{ebr, data, l.Sectors})
		}
		extEnd := cursor
		writeEntry(img, slot, 0x0F, false, uint32(extStart), uint32(extEnd-extStart))

		for i, pl := range plan {
			ebrOff := int(pl.ebr) * sectorSize
			ebr := img[ebrOff : ebrOff+sectorSize]
			ebr[510], ebr[511] = 0x55, 0xAA
			// Entry 0: the logical, relative to THIS EBR.
			writeEntry(ebr, 0, logicals[i].Type, logicals[i].Bootable,
				uint32(pl.data-pl.ebr), uint32(pl.sectors))
			// Entry 1: link to next EBR, relative to the EXTENDED start.
			if i+1 < len(plan) {
				writeEntry(ebr, 1, 0x05, false,
					uint32(plan[i+1].ebr-extStart), uint32(plan[i+1].sectors+2048))
			}
		}
	}
	return img
}
