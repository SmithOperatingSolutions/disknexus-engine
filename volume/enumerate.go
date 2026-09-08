// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"strconv"
	"strings"
)

// Device enumeration: what disks and filesystems this machine has, as the
// OS reports them, in the path forms the rest of this package opens. This
// is the fact-gathering half; what a product makes of a volume (whether it
// is a backup target, carries recovery media, holds a repository) is the
// caller's.

// Disk is a physical disk in capture form: \\.\PhysicalDriveN on Windows,
// /dev/sdX or /dev/nvme0n1 on Linux, /dev/diskN on macOS.
type Disk struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	// Serial and Removable as the OS reports them: the storage descriptor
	// on Windows (a USB, SD or MMC bus counts as removable whatever the
	// media bit says), sysfs on Linux (the removable flag, or a device
	// path through a USB controller), diskutil on macOS.
	Serial    string `json:"serial,omitempty"`
	Removable bool   `json:"removable"`
}

// VolumeInfo is a filesystem the OS knows about, with the disk it sits on.
// Device is the raw node a capture opens; Filesystem and Label are what
// the OS reported (empty when it reported nothing; the boot sector is the
// authority and the caller reads it through volumefs.IdentityAuto).
type VolumeInfo struct {
	Device     string `json:"device"`
	MountPoint string `json:"mount_point,omitempty"` // "" when unmounted
	Filesystem string `json:"filesystem,omitempty"`
	Label      string `json:"label,omitempty"`
	Bytes      int64  `json:"bytes"`
	Disk       string `json:"disk,omitempty"` // the whole disk, in Disk.Path form
	DiskSerial string `json:"disk_serial,omitempty"`
	Removable  bool   `json:"removable"`
}

// Enumerator reads the machine. The zero value reads the real OS; the
// fields are the seams tests feed fixtures through.
type Enumerator struct {
	// Linux: the mount table (/proc/self/mounts), the mountinfo table
	// (/proc/self/mountinfo), the sysfs block tree (/sys/block) and the
	// directory device nodes live in (/dev). Empty = the real ones.
	Mounts, Mountinfo, SysBlock, DevRoot string
	// Diskutil runs macOS's diskutil with args; nil = exec it.
	Diskutil func(args ...string) ([]byte, error)
}

// DefaultEnumerator honors the fixture environment the product's tests
// use: DISKNEXUS_SYS_BLOCK, DISKNEXUS_PROC_MOUNTS, DISKNEXUS_DEV_ROOT.
func DefaultEnumerator() Enumerator {
	return Enumerator{
		Mounts:   os.Getenv("DISKNEXUS_PROC_MOUNTS"),
		SysBlock: os.Getenv("DISKNEXUS_SYS_BLOCK"),
		DevRoot:  os.Getenv("DISKNEXUS_DEV_ROOT"),
	}
}

// Volumes lists the filesystems the OS knows, mounted or not where the OS
// can say. A volume the OS lists but cannot describe is still returned;
// the caller decides what an unreadable one means.
func (e Enumerator) Volumes() ([]VolumeInfo, error) { return e.volumes() }

// Disks lists the physical disks a capture can name. Loop, RAM and zram
// devices are not disks.
func (e Enumerator) Disks() ([]Disk, error) { return e.disks() }

// SystemDisk is the disk the running OS boots from, in Disk.Path form;
// an error when it cannot be resolved.
func (e Enumerator) SystemDisk() (string, error) { return e.systemDisk() }

// wholeDisk maps a partition node name to its disk: sdb1 → sdb,
// nvme0n1p2 → nvme0n1, mmcblk0p1 → mmcblk0; a whole-disk node maps to
// itself (nvme0n1 → nvme0n1, sdb → sdb).
func wholeDisk(node string) string {
	if strings.HasPrefix(node, "nvme") || strings.HasPrefix(node, "mmcblk") {
		if i := strings.LastIndex(node, "p"); i > 0 {
			if _, err := strconv.Atoi(node[i+1:]); err == nil {
				return node[:i]
			}
		}
		return node
	}
	return strings.TrimRight(node, "0123456789")
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func sysfsString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
