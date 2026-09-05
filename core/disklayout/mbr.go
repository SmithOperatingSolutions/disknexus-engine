// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Native MBR support (#149, reopens #88): Win10-era BIOS fleets are a real
// population — the #83 validation machine itself is MBR. Parsing recognizes
// a genuine MBR disk natively; a protective MBR (0xEE) whose GPT header is
// missing is reported as CORRUPT GPT, not as an MBR disk.
//
// Inherent MBR bounds are kept, not worked around: uint32 LBAs cap
// addressable space at 2 TiB with 512-byte sectors; logical partitions ride
// the EBR chain inside one extended partition.

const (
	mbrEntryBase   = 0x1BE
	mbrTypeEmpty   = 0x00
	mbrTypeExtCHS  = 0x05
	mbrTypeExtLBA  = 0x0F
	mbrTypeGPTProt = 0xEE
)

// mbrTypeNames covers the types worth naming in listings.
var mbrTypeNames = map[byte]string{
	0x01: "FAT12", 0x04: "FAT16", 0x06: "FAT16B", 0x07: "NTFS/exFAT",
	0x0B: "FAT32", 0x0C: "FAT32", 0x0E: "FAT16 LBA",
	0x05: "Extended", 0x0F: "Extended LBA",
	0x82: "Linux swap", 0x83: "Linux", 0x8E: "Linux LVM",
	0x27: "Windows RE", 0xEE: "GPT protective",
}

type mbrEntry struct {
	bootable bool
	typ      byte
	startLBA uint32
	sectors  uint32
}

func readMBREntries(sector []byte) [4]mbrEntry {
	var out [4]mbrEntry
	for i := 0; i < 4; i++ {
		e := sector[mbrEntryBase+i*16 : mbrEntryBase+i*16+16]
		out[i] = mbrEntry{
			bootable: e[0] == 0x80,
			typ:      e[4],
			startLBA: binary.LittleEndian.Uint32(e[8:12]),
			sectors:  binary.LittleEndian.Uint32(e[12:16]),
		}
	}
	return out
}

func isExtended(t byte) bool { return t == mbrTypeExtCHS || t == mbrTypeExtLBA }

// parseMBR parses a non-protective MBR disk: primaries from the boot
// sector, logicals by walking the EBR chain. sector is LBA0, already
// validated to carry the 0x55AA signature.
func parseMBR(r io.ReaderAt, diskSize int64, sectorSize int, sector []byte) (*DiskLayout, error) {
	entries := readMBREntries(sector)

	l := &DiskLayout{
		Scheme:     "mbr",
		SectorSize: sectorSize,
		DiskSize:   diskSize,
	}

	firstPartLBA := uint64(0)
	slot := 0
	var extended *mbrEntry
	for i := range entries {
		e := entries[i]
		if e.typ == mbrTypeEmpty || e.sectors == 0 {
			continue
		}
		if firstPartLBA == 0 || uint64(e.startLBA) < firstPartLBA {
			firstPartLBA = uint64(e.startLBA)
		}
		if isExtended(e.typ) {
			if extended != nil {
				return nil, fmt.Errorf("MBR has multiple extended partitions")
			}
			ext := e
			extended = &ext
			continue
		}
		l.Partitions = append(l.Partitions, Partition{
			Index:    slot,
			MBRType:  e.typ,
			TypeName: mbrTypeNames[e.typ],
			Bootable: e.bootable,
			FirstLBA: uint64(e.startLBA),
			LastLBA:  uint64(e.startLBA) + uint64(e.sectors) - 1,
		})
		slot++
	}
	if len(l.Partitions) == 0 && extended == nil {
		return nil, fmt.Errorf("MBR carries no partitions")
	}

	if extended != nil {
		logicals, ebrs, err := walkEBRChain(r, sectorSize, uint64(extended.startLBA), diskSize, slot)
		if err != nil {
			return nil, err
		}
		l.Partitions = append(l.Partitions, logicals...)
		l.AuxRegions = ebrs
	}

	// Boot track: LBA0 through the first partition — BIOS boot code and the
	// MBR gap restore byte-exactly, which is what makes restored disks boot
	// without bootloader surgery.
	l.PrimaryRegion = Range{Offset: 0, Length: int64(firstPartLBA) * int64(sectorSize)}
	return l, nil
}

// walkEBRChain enumerates logical partitions: each EBR's entry 0 is the
// logical (relative to that EBR), entry 1 links the next EBR (relative to
// the EXTENDED partition start). Chain length is bounded to reject loops.
func walkEBRChain(r io.ReaderAt, sectorSize int, extStart uint64, diskSize int64, slot int) ([]Partition, []Range, error) {
	const maxLogicals = 128
	var out []Partition
	var ebrs []Range
	ebrLBA := extStart
	for i := 0; i < maxLogicals; i++ {
		buf := make([]byte, sectorSize)
		off := int64(ebrLBA) * int64(sectorSize)
		if off >= diskSize {
			return nil, nil, fmt.Errorf("EBR at LBA %d beyond disk end", ebrLBA)
		}
		if _, err := r.ReadAt(buf, off); err != nil {
			return nil, nil, fmt.Errorf("reading EBR at LBA %d: %w", ebrLBA, err)
		}
		if buf[mbrSigOffset] != 0x55 || buf[mbrSigOffset+1] != 0xAA {
			return nil, nil, fmt.Errorf("EBR at LBA %d lacks boot signature", ebrLBA)
		}
		ebrs = append(ebrs, Range{Offset: off, Length: int64(sectorSize)})
		entries := readMBREntries(buf)
		if entries[0].typ != mbrTypeEmpty && entries[0].sectors != 0 {
			first := ebrLBA + uint64(entries[0].startLBA)
			out = append(out, Partition{
				Index:    slot,
				MBRType:  entries[0].typ,
				TypeName: mbrTypeNames[entries[0].typ],
				Bootable: entries[0].bootable,
				FirstLBA: first,
				LastLBA:  first + uint64(entries[0].sectors) - 1,
			})
			slot++
		}
		if entries[1].typ == mbrTypeEmpty || entries[1].sectors == 0 {
			return out, ebrs, nil
		}
		ebrLBA = extStart + uint64(entries[1].startLBA)
	}
	return nil, nil, fmt.Errorf("EBR chain exceeds %d logicals (loop?)", maxLogicals)
}
