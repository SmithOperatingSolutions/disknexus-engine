//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package vss

import (
	"os"
	"strings"
	"testing"
)

// TestCreateSnapshotSet2Volumes exercises an atomic 2-volume VSS snapshot set
// (#68) plus disk/volume enumeration. Gated on DISKNEXUS_VSS_SET_VOLUMES
// (e.g. "T:,U:") — set by the CI vss-windows job, which creates two VHDs.
// Requires elevation.
func TestCreateSnapshotSet2Volumes(t *testing.T) {
	env := os.Getenv("DISKNEXUS_VSS_SET_VOLUMES")
	if env == "" {
		t.Skip("DISKNEXUS_VSS_SET_VOLUMES not set")
	}
	vols := strings.Split(env, ",")
	if len(vols) < 2 {
		t.Fatalf("need >=2 volumes, got %q", env)
	}

	set, err := CreateSnapshotSet(vols)
	if err != nil {
		t.Fatalf("CreateSnapshotSet(%v): %v", vols, err)
	}
	defer set.Release()

	if len(set.Members) != len(vols) {
		t.Fatalf("got %d members, want %d", len(set.Members), len(vols))
	}
	seenDev := map[string]bool{}
	for i, m := range set.Members {
		if m.Volume != vols[i] {
			t.Fatalf("member %d volume = %s, want %s (order must match request)", i, m.Volume, vols[i])
		}
		if m.ID == "" || m.DevicePath == "" {
			t.Fatalf("member %d missing ID/device: %+v", i, m)
		}
		if seenDev[m.DevicePath] {
			t.Fatalf("duplicate shadow device %s", m.DevicePath)
		}
		seenDev[m.DevicePath] = true
		// The shadow device must be readable (open + read a sector).
		f, err := os.Open(m.DevicePath)
		if err != nil {
			t.Fatalf("open shadow device %s: %v", m.DevicePath, err)
		}
		buf := make([]byte, 512)
		if _, err := f.Read(buf); err != nil {
			f.Close()
			t.Fatalf("read shadow device %s: %v", m.DevicePath, err)
		}
		f.Close()
	}

	// Enumeration: every requested volume must be locatable on SOME disk with
	// extents (the VHDs appear as physical disks).
	found := 0
	for disk := uint32(0); disk < 16; disk++ {
		list, err := VolumesOnDisk(disk)
		if err != nil {
			continue
		}
		for _, v := range list {
			for _, want := range vols {
				if strings.EqualFold(strings.TrimSuffix(v.MountPoint, `\`), want) {
					if len(v.Extents) == 0 {
						t.Fatalf("volume %s found with no extents", want)
					}
					found++
				}
			}
		}
	}
	if found < len(vols) {
		t.Fatalf("enumeration located %d of %d test volumes across disks", found, len(vols))
	}
	t.Logf("2-volume set OK: %d members, enumeration found all volumes", len(set.Members))
}
