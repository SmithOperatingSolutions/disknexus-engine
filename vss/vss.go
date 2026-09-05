// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package vss creates and releases Windows Volume Shadow Copy snapshots for
// consistent volume backups. It is a thin adapter over github.com/SmithOperatingSolutions/go-vss
// (a pure-Go COM requester); the surrounding disknexus code depends only on
// the small Snapshot shape defined here.
package vss

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	govss "github.com/SmithOperatingSolutions/go-vss"
)

// Snapshot represents an active VSS shadow copy.
type Snapshot struct {
	ID         string // VSS snapshot ID (GUID, "{...}" form)
	DevicePath string // Shadow device path, e.g. \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy1
	Volume     string // Source volume as requested (e.g. "C:")

	// set holds the underlying shadow copy set. It must stay open for the
	// life of the backup: closing it notifies writers and deletes the
	// (transient) snapshot. nil on non-Windows / not-created snapshots.
	set *govss.SnapshotSet
	// unregister removes this snapshot from the active-device registry
	// (#322) when it is released.
	unregister func()
}

// CreateSnapshot creates a VSS snapshot of the given volume (e.g. "C:").
// On non-Windows platforms it returns an error directing the user to
// file-based input instead.
func CreateSnapshot(volume string) (*Snapshot, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("VSS snapshots require Windows (current OS: %s); use --input flag for file-based analysis", runtime.GOOS)
	}

	// go-vss expects a volume path with a trailing separator ("C:" -> "C:\").
	volPath := volume
	if !strings.HasSuffix(volPath, `\`) {
		volPath += `\`
	}

	set, err := govss.Create(context.Background(), volPath)
	if err != nil {
		return nil, err
	}
	snaps := set.Snapshots()
	if len(snaps) == 0 {
		set.Close()
		return nil, fmt.Errorf("VSS returned no snapshot for %s", volume)
	}
	s := snaps[0]
	return &Snapshot{
		ID:         s.ID,
		DevicePath: s.DeviceObject,
		Volume:     volume,
		set:        set,
		unregister: registerActiveDevices([]string{s.DeviceObject}),
	}, nil
}

// Release tears down the VSS snapshot, notifying writers and deleting the
// shadow copy. Safe to call on a nil or already-released snapshot.
func (s *Snapshot) Release() error {
	if s == nil || s.set == nil {
		return nil
	}
	if s.unregister != nil {
		s.unregister()
	}
	set := s.set
	s.set = nil
	return set.Close()
}

// SetMember is one volume's snapshot within an atomic multi-volume set.
type SetMember struct {
	Volume     string // as requested (e.g. "C:", or a \\?\Volume{GUID}\ name)
	ID         string // snapshot GUID
	DevicePath string // shadow device to read
}

// Set is an atomic multi-volume shadow-copy set (#68): every member snapshot
// is from the same instant, the way cross-volume applications expect. Release
// tears the whole set down.
type Set struct {
	Members []SetMember
	set     *govss.SnapshotSet
	// unregister removes the set's devices from the active registry (#322).
	unregister func()
}

// CreateSnapshotSet snapshots all given volumes in ONE VSS snapshot set
// (file-consistent, writer-participating; VSS caps a set at 64 volumes).
// Members are returned in the order requested.
func CreateSnapshotSet(volumes []string) (*Set, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("VSS snapshots require Windows (current OS: %s)", runtime.GOOS)
	}
	if len(volumes) == 0 {
		return nil, fmt.Errorf("no volumes given")
	}
	paths := make([]string, len(volumes))
	for i, v := range volumes {
		p := v
		if !strings.HasSuffix(p, `\`) {
			p += `\`
		}
		paths[i] = p
	}
	set, err := govss.CreateSet(context.Background(), paths)
	if err != nil {
		return nil, err
	}
	s := &Set{set: set}
	for i, p := range paths {
		snap := set.ForVolume(p)
		if snap == nil {
			set.Close()
			return nil, fmt.Errorf("VSS set missing snapshot for %s", volumes[i])
		}
		s.Members = append(s.Members, SetMember{Volume: volumes[i], ID: snap.ID, DevicePath: snap.DeviceObject})
	}
	devs := make([]string, len(s.Members))
	for i, m := range s.Members {
		devs[i] = m.DevicePath
	}
	s.unregister = registerActiveDevices(devs)
	return s, nil
}

// Release tears down the snapshot set (notifies writers, deletes the
// transient copies).
func (s *Set) Release() error {
	if s == nil || s.set == nil {
		return nil
	}
	if s.unregister != nil {
		s.unregister()
	}
	err := s.set.Close()
	s.set = nil
	return err
}

// DiskExtent locates a slice of a volume on a physical disk.
type DiskExtent struct {
	DiskNumber     uint32
	StartingOffset int64
	Length         int64
}

// VolumeOnDisk describes a volume found during disk enumeration (#68): its
// canonical GUID name, mount point when mounted (empty for e.g. the ESP), and
// where it lives on physical disks.
type VolumeOnDisk struct {
	VolumeName string // \\?\Volume{GUID}\ form — usable with CreateSnapshotSet
	MountPoint string // "" when unmounted
	Extents    []DiskExtent
}

// VolumesOnDisk enumerates all volumes that have at least one extent on the
// given physical disk, for correlating GPT partitions with snapshot-able
// volumes (BMR disk capture).
func VolumesOnDisk(diskNumber uint32) ([]VolumeOnDisk, error) {
	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("volume enumeration requires Windows (current OS: %s)", runtime.GOOS)
	}
	vols, err := govss.EnumerateVolumes()
	if err != nil {
		return nil, err
	}
	var out []VolumeOnDisk
	for _, v := range vols {
		exts, err := v.DiskExtents()
		if err != nil {
			continue // e.g. CD-ROM or offline volume; not a candidate
		}
		var mine []DiskExtent
		for _, e := range exts {
			if e.DiskNumber == diskNumber {
				mine = append(mine, DiskExtent{DiskNumber: e.DiskNumber, StartingOffset: e.StartingOffset, Length: e.Length})
			}
		}
		if len(mine) > 0 {
			out = append(out, VolumeOnDisk{VolumeName: v.VolumeName, MountPoint: v.MountPoint, Extents: mine})
		}
	}
	return out, nil
}
