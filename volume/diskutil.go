// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os/exec"
	"strings"
)

// macOS composition over `diskutil -plist` output, kept build-tag free so
// the fixture tests run on every OS. `diskutil list` names every disk,
// partition and APFS volume; `diskutil info` says whether one is removable
// or external, its raw node, and the whole disk it sits on (whose UUID
// stands in for a drive serial, which diskutil does not expose).

func (e Enumerator) diskutil(args ...string) ([]byte, error) {
	if e.Diskutil != nil {
		return e.Diskutil(args...)
	}
	return exec.Command("diskutil", args...).Output()
}

func (e Enumerator) volumesFromDiskutil(listPlist []byte) ([]VolumeInfo, error) {
	top, err := parsePlist(listPlist)
	if err != nil {
		return nil, err
	}
	out := []VolumeInfo{}
	for _, disk := range top.dicts("AllDisksAndPartitions") {
		// Physical partitions, and the volumes of an APFS container (a
		// synthesized disk lists them under APFSVolumes — the system and
		// data volumes live there, never under Partitions).
		parts := append(disk.dicts("Partitions"), disk.dicts("APFSVolumes")...)
		if len(parts) == 0 && disk.str("MountPoint") != "" {
			parts = []plistDict{disk} // a whole-disk filesystem (no partition table)
		}
		for _, p := range parts {
			id := p.str("DeviceIdentifier")
			if id == "" {
				continue
			}
			v := VolumeInfo{
				Device:     "/dev/r" + id,
				MountPoint: p.str("MountPoint"),
				Label:      p.str("VolumeName"),
				Bytes:      p.integer("Size"),
			}
			if info, err := e.diskutil("info", "-plist", id); err == nil {
				if d, err := parsePlist(info); err == nil {
					v.Removable = d.boolean("RemovableMediaOrExternalDevice") || d.boolean("RemovableMedia") || d.boolean("Removable") || (d["Internal"] != nil && !d.boolean("Internal"))
					v.Filesystem = strings.ToLower(d.str("FilesystemType"))
					if node := d.str("DeviceNode"); node != "" {
						v.Device = strings.Replace(node, "/dev/disk", "/dev/rdisk", 1)
					}
					if parent := d.str("ParentWholeDisk"); parent != "" {
						v.Disk = "/dev/" + parent
						v.DiskSerial = e.wholeDiskUUID(parent)
					}
				}
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// disksFromDiskutil lists the whole disks: every top-level entry of
// `diskutil list`, sized from its Size key, marked removable and given a
// UUID from `diskutil info`.
func (e Enumerator) disksFromDiskutil(listPlist []byte) ([]Disk, error) {
	top, err := parsePlist(listPlist)
	if err != nil {
		return nil, err
	}
	out := []Disk{}
	for _, disk := range top.dicts("AllDisksAndPartitions") {
		id := disk.str("DeviceIdentifier")
		if id == "" || len(disk.dicts("APFSPhysicalStores")) > 0 {
			continue // a synthesized APFS container is not a disk
		}
		d := Disk{Path: "/dev/" + id, Bytes: disk.integer("Size")}
		if info, err := e.diskutil("info", "-plist", id); err == nil {
			if pd, err := parsePlist(info); err == nil {
				d.Removable = pd.boolean("RemovableMediaOrExternalDevice") || pd.boolean("RemovableMedia") || pd.boolean("Removable") || (pd["Internal"] != nil && !pd.boolean("Internal"))
				d.Serial = pd.str("DiskUUID")
				if d.Serial == "" {
					d.Serial = pd.str("MediaName")
				}
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// wholeDiskUUID: macOS exposes no drive serial through diskutil; the
// whole disk's UUID is the stable per-drive mark it does expose.
func (e Enumerator) wholeDiskUUID(disk string) string {
	info, err := e.diskutil("info", "-plist", disk)
	if err != nil {
		return ""
	}
	d, err := parsePlist(info)
	if err != nil {
		return ""
	}
	if u := d.str("DiskUUID"); u != "" {
		return u
	}
	return d.str("MediaName")
}

// systemDiskFromDiskutil: the whole disk under the root volume. An APFS
// root sits in a synthesized container whose physical store names the
// real disk (disk0s2 → disk0); a plain root names its ParentWholeDisk.
func (e Enumerator) systemDiskFromDiskutil(rootInfo []byte) (string, error) {
	d, err := parsePlist(rootInfo)
	if err != nil {
		return "", err
	}
	if stores := d.dicts("APFSPhysicalStores"); len(stores) > 0 {
		if id := macWholeDisk(stores[0].str("DeviceIdentifier")); id != "" {
			return "/dev/" + id, nil
		}
	}
	if parent := d.str("ParentWholeDisk"); parent != "" {
		// The container itself is synthesized: ask it for its store.
		if info, err := e.diskutil("info", "-plist", parent); err == nil {
			if pd, perr := parsePlist(info); perr == nil {
				if stores := pd.dicts("APFSPhysicalStores"); len(stores) > 0 {
					if id := macWholeDisk(stores[0].str("DeviceIdentifier")); id != "" {
						return "/dev/" + id, nil
					}
				}
			}
		}
		return "/dev/" + parent, nil
	}
	return "", errNoRootDisk
}

// macWholeDisk maps a macOS node to its whole disk: disk0s2 → disk0,
// /dev/disk3s1 → disk3, disk5 → disk5.
func macWholeDisk(id string) string {
	id = strings.TrimPrefix(strings.TrimPrefix(id, "/dev/"), "r")
	if !strings.HasPrefix(id, "disk") {
		return ""
	}
	rest := id[len("disk"):]
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return ""
	}
	return "disk" + rest[:n]
}
