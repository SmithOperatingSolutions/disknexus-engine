// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"testing"
)

// #149 slice 2: machine manifests carry MBR layouts — boot track in
// PrimaryGPT (verbatim), NO backup structures (MBR has none), and
// relocation to a larger disk is the identity (nothing lives at disk end).
func TestMachineManifestValidatesMBRDisk(t *testing.T) {
	bootTrack := make([]byte, 2048*512)
	l := DiskLayout{
		Scheme: "mbr", SectorSize: 512, DiskSize: 1 << 30,
		Partitions:    []Partition{{Index: 0, MBRType: 0x07, FirstLBA: 2048, LastLBA: 4095}},
		PrimaryRegion: Range{Offset: 0, Length: int64(len(bootTrack))},
	}
	m := MachineManifest{
		Version: machineManifestVersion, MachineID: "m1", Hostname: "h", OS: "windows",
		Disks: []DiskCapture{{
			Source: `\\.\PhysicalDrive0`, Layout: l, PrimaryGPT: bootTrack, BackupGPT: nil,
			Members: []PartitionMember{{Index: 0, Kind: MemberRaw, BackupID: "b1"}},
		}},
	}
	if err := m.validate(); err != nil {
		t.Fatalf("MBR machine manifest must validate: %v", err)
	}

	// GPT disks still demand their backup structures (regression guard).
	m.Disks[0].Layout.Scheme = "gpt"
	m.Disks[0].Layout.BackupRegion = Range{Offset: 1<<30 - 512*33, Length: 512 * 33}
	if err := m.validate(); err == nil {
		t.Fatal("GPT manifest with empty BackupGPT must fail validation")
	}
}

func TestRelocateMBRIsIdentity(t *testing.T) {
	boot := make([]byte, 2048*512)
	boot[0] = 0xFA
	l := &DiskLayout{
		Scheme: "mbr", SectorSize: 512, DiskSize: 1 << 30,
		PrimaryRegion: Range{Offset: 0, Length: int64(len(boot))},
	}
	primary, backupOff, backup, err := RelocateGPT(l, boot, nil, 2<<30)
	if err != nil {
		t.Fatalf("MBR relocation must be identity: %v", err)
	}
	if &primary[0] != &boot[0] || backup != nil || backupOff != 0 {
		t.Fatalf("MBR relocation changed data: off=%d backupLen=%d", backupOff, len(backup))
	}
}
