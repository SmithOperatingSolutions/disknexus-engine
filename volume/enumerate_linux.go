//go:build !windows && !darwin

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux: the mount table names the mounted block devices; sysfs says which
// whole disk each belongs to, whether the kernel calls it removable or it
// hangs off USB, and the disk's serial. Disks are the sysfs block tree
// minus loop, ram and zram; the system disk is the one whose device tree
// carries the root mount's major:minor.

var mountedFilesystems = map[string]bool{
	"ntfs": true, "ntfs3": true, "fuseblk": true, "vfat": true, "exfat": true,
	"ext4": true, "ext3": true, "ext2": true, "hfsplus": true, "apfs": true, "btrfs": true, "xfs": true,
}

func (e Enumerator) mounts() string {
	if e.Mounts != "" {
		return e.Mounts
	}
	return "/proc/self/mounts"
}

func (e Enumerator) sysBlock() string {
	if e.SysBlock != "" {
		return e.SysBlock
	}
	return "/sys/block"
}

func (e Enumerator) volumes() ([]VolumeInfo, error) {
	f, err := os.Open(e.mounts())
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sysBlock := e.sysBlock()
	out := []VolumeInfo{}
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !strings.HasPrefix(fields[0], "/dev/") || !mountedFilesystems[fields[2]] {
			continue
		}
		dev := fields[0]
		if seen[dev] {
			continue // bind mounts list a device more than once
		}
		seen[dev] = true
		v := VolumeInfo{Device: dev, MountPoint: unescapeMount(fields[1]), Filesystem: fields[2]}
		if e.DevRoot != "" {
			v.Device = filepath.Join(e.DevRoot, strings.TrimPrefix(dev, "/dev/"))
		}
		node := strings.TrimPrefix(dev, "/dev/")
		disk := wholeDisk(node)
		v.Disk = "/dev/" + disk
		v.Removable = sysfsRemovable(sysBlock, disk)
		v.DiskSerial = sysfsString(filepath.Join(sysBlock, disk, "device", "serial"))
		if sectors, err := strconv.ParseInt(sysfsString(filepath.Join(sysBlock, disk, node, "size")), 10, 64); err == nil {
			v.Bytes = sectors * 512
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// sysfsRemovable: the kernel's removable flag, or a device path through a
// USB controller (external USB disks report removable=0).
func sysfsRemovable(sysBlock, disk string) bool {
	if sysfsString(filepath.Join(sysBlock, disk, "removable")) == "1" {
		return true
	}
	if real, err := filepath.EvalSymlinks(filepath.Join(sysBlock, disk)); err == nil && strings.Contains(real, "/usb") {
		return true
	}
	return false
}

func (e Enumerator) disks() ([]Disk, error) {
	sysBlock := e.sysBlock()
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return nil, err
	}
	out := []Disk{}
	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(sysBlock, name, "size"))
		if err != nil {
			continue
		}
		sectors, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		out = append(out, Disk{
			Path: "/dev/" + name, Bytes: sectors * 512,
			Serial:    sysfsString(filepath.Join(sysBlock, name, "device", "serial")),
			Removable: sysfsRemovable(sysBlock, name),
		})
	}
	return out, nil
}

// systemDisk resolves the root filesystem's major:minor from mountinfo,
// then finds the /sys/block disk whose device tree contains it.
func (e Enumerator) systemDisk() (string, error) {
	mountinfo := e.Mountinfo
	if mountinfo == "" {
		mountinfo = "/proc/self/mountinfo"
	}
	b, err := os.ReadFile(mountinfo)
	if err != nil {
		return "", err
	}
	var rootDev string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[4] == "/" {
			rootDev = f[2] // major:minor
			break
		}
	}
	if rootDev == "" {
		return "", fmt.Errorf("root mount not found in %s", mountinfo)
	}
	sysBlock := e.sysBlock()
	disks, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", err
	}
	for _, d := range disks {
		base := filepath.Join(sysBlock, d.Name())
		matches := func(devFile string) bool { return sysfsString(devFile) == rootDev }
		if matches(filepath.Join(base, "dev")) {
			return "/dev/" + d.Name(), nil
		}
		parts, _ := os.ReadDir(base)
		for _, p := range parts {
			if matches(filepath.Join(base, p.Name(), "dev")) {
				return "/dev/" + d.Name(), nil
			}
		}
	}
	return "", fmt.Errorf("no %s entry matches root device %s", sysBlock, rootDev)
}
