// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func sampleManifest(t *testing.T) *MachineManifest {
	t.Helper()
	img := buildGPT(t, 512, 8192, stdParts())
	l, err := ParseWithSector(bytes.NewReader(img), int64(len(img)), 512)
	if err != nil {
		t.Fatal(err)
	}
	prim := img[l.PrimaryRegion.Offset : l.PrimaryRegion.Offset+l.PrimaryRegion.Length]
	back := img[l.BackupRegion.Offset : l.BackupRegion.Offset+l.BackupRegion.Length]
	return &MachineManifest{
		MachineID: "machine-1",
		Hostname:  "test-host",
		OS:        "windows",
		CreatedAt: time.Now().UTC(),
		Disks: []DiskCapture{{
			Source:     `\\.\PhysicalDrive0`,
			Layout:     *l,
			PrimaryGPT: append([]byte(nil), prim...),
			BackupGPT:  append([]byte(nil), back...),
			Members: []PartitionMember{
				{Index: 0, Kind: MemberRaw, BackupID: "b-esp"},
				{Index: 1, Kind: MemberRaw, BackupID: "b-msr"},
				{Index: 2, Kind: MemberVolume, BackupID: "b-c"},
				{Index: 3, Kind: MemberVolume, BackupID: "b-re"},
			},
		}},
	}
}

func TestMachineManifestRoundTrip(t *testing.T) {
	repo := t.TempDir()
	m := sampleManifest(t)
	if err := SaveMachineManifest(repo, "snap1", m); err != nil {
		t.Fatal(err)
	}
	got, err := LoadMachineManifest(repo, "snap1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "test-host" || len(got.Disks) != 1 {
		t.Fatalf("round-trip basics: %+v", got)
	}
	d := got.Disks[0]
	if len(d.Layout.Partitions) != 4 || len(d.Members) != 4 {
		t.Fatalf("disk round-trip: %d parts, %d members", len(d.Layout.Partitions), len(d.Members))
	}
	if !bytes.Equal(d.PrimaryGPT, m.Disks[0].PrimaryGPT) || !bytes.Equal(d.BackupGPT, m.Disks[0].BackupGPT) {
		t.Fatal("verbatim GPT regions did not round-trip")
	}
	ids, err := ListMachineManifests(repo)
	if err != nil || len(ids) != 1 || ids[0] != "snap1" {
		t.Fatalf("List = %v (%v)", ids, err)
	}
}

// TestMachineManifestValidation: every internal inconsistency a restore
// depends on must be refused at save AND load time.
func TestMachineManifestValidation(t *testing.T) {
	repo := t.TempDir()

	cases := []struct {
		name string
		mut  func(m *MachineManifest)
	}{
		{"no disks", func(m *MachineManifest) { m.Disks = nil }},
		{"member for unknown partition", func(m *MachineManifest) { m.Disks[0].Members[0].Index = 99 }},
		{"double-covered partition", func(m *MachineManifest) { m.Disks[0].Members[1].Index = 0 }},
		{"volume member missing backup ID", func(m *MachineManifest) { m.Disks[0].Members[2].BackupID = "" }},
		{"uncovered partition", func(m *MachineManifest) { m.Disks[0].Members = m.Disks[0].Members[:3] }},
		{"unknown member kind", func(m *MachineManifest) { m.Disks[0].Members[0].Kind = "banana" }},
		{"primary GPT length mismatch", func(m *MachineManifest) { m.Disks[0].PrimaryGPT = m.Disks[0].PrimaryGPT[:10] }},
		{"backup GPT length mismatch", func(m *MachineManifest) { m.Disks[0].BackupGPT = append(m.Disks[0].BackupGPT, 0) }},
	}
	for _, tc := range cases {
		m := sampleManifest(t)
		tc.mut(m)
		if err := SaveMachineManifest(repo, "bad", m); err == nil {
			t.Fatalf("%s: save accepted invalid manifest", tc.name)
		}
	}

	// Skipped members are legal without a backup ID.
	m := sampleManifest(t)
	m.Disks[0].Members[1] = PartitionMember{Index: 1, Kind: MemberSkipped, Reason: "MSR has no data"}
	if err := SaveMachineManifest(repo, "ok", m); err != nil {
		t.Fatalf("skipped member should be legal: %v", err)
	}
}

// TestMachineManifestTornWrite: a truncated/corrupted file is rejected by CRC,
// never half-parsed.
func TestMachineManifestTornWrite(t *testing.T) {
	repo := t.TempDir()
	if err := SaveMachineManifest(repo, "snap", sampleManifest(t)); err != nil {
		t.Fatal(err)
	}
	path := MachineManifestPath(repo, "snap")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMachineManifest(repo, "snap"); err == nil {
		t.Fatal("corrupted machine manifest loaded successfully")
	}
}
