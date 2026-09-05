// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// GUID is a 16-byte GPT GUID in on-disk (mixed-endian) order: the first three
// fields are little-endian, the last two big-endian — so the canonical string
// form requires the byte swaps below (UEFI spec appendix A / RFC 4122).
type GUID [16]byte

// String renders the canonical uppercase form, e.g.
// "C12A7328-F81F-11D2-BA4B-00A0C93EC93B".
func (g GUID) String() string {
	return fmt.Sprintf("%08X-%04X-%04X-%04X-%012X",
		binary.LittleEndian.Uint32(g[0:4]),
		binary.LittleEndian.Uint16(g[4:6]),
		binary.LittleEndian.Uint16(g[6:8]),
		binary.BigEndian.Uint16(g[8:10]),
		g[10:16])
}

// IsZero reports the all-zero GUID (an unused GPT partition entry).
func (g GUID) IsZero() bool {
	return g == GUID{}
}

// ParseGUID parses the canonical string form back into on-disk order.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	clean := strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(s)), "-", "")
	if len(clean) != 32 {
		return g, fmt.Errorf("guid %q: want 32 hex digits", s)
	}
	var raw [16]byte
	for i := 0; i < 16; i++ {
		var b byte
		if _, err := fmt.Sscanf(clean[i*2:i*2+2], "%02X", &b); err != nil {
			return g, fmt.Errorf("guid %q: %w", s, err)
		}
		raw[i] = b
	}
	// raw is display order; convert to on-disk mixed-endian.
	g[0], g[1], g[2], g[3] = raw[3], raw[2], raw[1], raw[0]
	g[4], g[5] = raw[5], raw[4]
	g[6], g[7] = raw[7], raw[6]
	copy(g[8:], raw[8:])
	return g, nil
}

// Well-known partition type GUIDs (canonical string form).
const (
	TypeESP           = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B" // EFI System Partition
	TypeMSR           = "E3C9E316-0B5C-4DB8-817D-F92DF00215AE" // Microsoft Reserved
	TypeMSBasicData   = "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7" // Windows basic data (NTFS/exFAT)
	TypeWinRE         = "DE94BBA4-06D1-4D40-A16A-BFD50179D6AC" // Windows Recovery Environment
	TypeLinuxFS       = "0FC63DAF-8483-4772-8E79-3D69D8477DE4" // Linux filesystem
	TypeLinuxSwap     = "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F" // Linux swap
	TypeBIOSBoot      = "21686148-6449-6E6F-744E-656564454649" // GRUB BIOS boot
	TypeLinuxLVM      = "E6D6D379-F507-44C2-A23C-238F2A3DF928" // Linux LVM PV
	TypeLinuxHome     = "933AC7E1-2EB4-4F13-B844-0E14E2AEF915" // Linux /home
	TypeMSLDMMetadata = "5808C8AA-7E8F-42E0-85D2-E1E90434CFB3" // Windows LDM metadata
)

// typeNames maps well-known type GUIDs to short human-readable names.
var typeNames = map[string]string{
	TypeESP:           "EFI System",
	TypeMSR:           "Microsoft Reserved",
	TypeMSBasicData:   "Basic Data",
	TypeWinRE:         "Windows RE",
	TypeLinuxFS:       "Linux Filesystem",
	TypeLinuxSwap:     "Linux Swap",
	TypeBIOSBoot:      "BIOS Boot",
	TypeLinuxLVM:      "Linux LVM",
	TypeLinuxHome:     "Linux Home",
	TypeMSLDMMetadata: "Windows LDM",
}

// TypeName returns a short human-readable name for a partition type GUID, or
// the GUID string itself when unknown.
func TypeName(g GUID) string {
	s := g.String()
	if n, ok := typeNames[s]; ok {
		return n
	}
	return s
}
