//go:build !windows && !darwin

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Linux facts from fixtures: a mount table with a bind mount, an escaped
// mount point, a non-filesystem loop mount and a device with no node; a
// sysfs tree with the kernel's removable flag, a USB-attached disk that
// reports removable=0, serials and partition sizes; a mountinfo naming
// the root device.
func TestLinuxEnumerationFacts(t *testing.T) {
	root := t.TempDir()
	dev := filepath.Join(root, "dev")
	sys := filepath.Join(root, "sys", "block")
	os.MkdirAll(dev, 0o755)
	for _, n := range []string{"sdb1", "sdc1", "nvme0n1p2"} {
		writeFixture(t, filepath.Join(dev, n), "")
	}
	writeFixture(t, filepath.Join(sys, "sdb", "removable"), "1\n")
	writeFixture(t, filepath.Join(sys, "sdb", "device", "serial"), "WD-WX11A8765432\n")
	writeFixture(t, filepath.Join(sys, "sdb", "size"), "8192\n")
	writeFixture(t, filepath.Join(sys, "sdb", "sdb1", "size"), "4096\n")
	writeFixture(t, filepath.Join(sys, "sdb", "dev"), "8:16\n")
	writeFixture(t, filepath.Join(sys, "sdb", "sdb1", "dev"), "8:17\n")
	usbDisk := filepath.Join(root, "sys", "devices", "pci0000:00", "0000:00:14.0", "usb3", "3-1", "3-1:1.0", "host6", "target6:0:0", "6:0:0:0", "block", "sdc")
	writeFixture(t, filepath.Join(usbDisk, "removable"), "0\n")
	writeFixture(t, filepath.Join(usbDisk, "device", "serial"), "2HC015KJ\n")
	writeFixture(t, filepath.Join(usbDisk, "size"), "16384\n")
	if err := os.Symlink(usbDisk, filepath.Join(sys, "sdc")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(sys, "nvme0n1", "removable"), "0\n")
	writeFixture(t, filepath.Join(sys, "nvme0n1", "size"), "2000000\n")
	writeFixture(t, filepath.Join(sys, "nvme0n1", "nvme0n1p2", "size"), "1000000\n")
	writeFixture(t, filepath.Join(sys, "nvme0n1", "dev"), "259:0\n")
	writeFixture(t, filepath.Join(sys, "nvme0n1", "nvme0n1p2", "dev"), "259:2\n")
	for _, n := range []string{"loop0", "ram0", "zram0"} {
		writeFixture(t, filepath.Join(sys, n, "size"), "8192\n")
	}
	mydisk := filepath.Join(root, "mnt", "My Disk")
	escaped := strings.ReplaceAll(mydisk, " ", `\040`)
	mounts := filepath.Join(root, "mounts")
	writeFixture(t, mounts, strings.Join([]string{
		"sysfs /sys sysfs rw 0 0",
		"/dev/nvme0n1p2 /data ext4 rw 0 0",
		"/dev/sdb1 /photos vfat rw 0 0",
		"/dev/sdb1 /bind vfat rw 0 0", // a bind mount of the same device
		"/dev/sdc1 " + escaped + " fuseblk rw 0 0",
		"/dev/sde1 /mnt/gone ext4 rw 0 0", // no such device node
		"tmpfs /run tmpfs rw 0 0",
		"/dev/loop3 /snap/core/1 squashfs ro 0 0",
	}, "\n")+"\n")
	mountinfo := filepath.Join(root, "mountinfo")
	writeFixture(t, mountinfo, "22 1 259:2 / / rw - ext4 /dev/nvme0n1p2 rw\n23 22 0:5 / /proc rw - proc proc rw\n")

	e := Enumerator{Mounts: mounts, Mountinfo: mountinfo, SysBlock: sys, DevRoot: dev}
	vols, err := e.Volumes()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]VolumeInfo{}
	for _, v := range vols {
		by[filepath.Base(v.Device)] = v
	}
	if len(vols) != 4 {
		t.Fatalf("listed %d volumes, want 4 (sdb1 once, sdc1, nvme0n1p2, sde1; no squashfs loop): %+v", len(vols), vols)
	}
	photos := by["sdb1"]
	if photos.Device != filepath.Join(dev, "sdb1") || photos.MountPoint != "/photos" || photos.Filesystem != "vfat" || !photos.Removable ||
		photos.Disk != "/dev/sdb" || photos.DiskSerial != "WD-WX11A8765432" || photos.Bytes != 4096*512 {
		t.Fatalf("sdb1: %+v", photos)
	}
	usb := by["sdc1"]
	if !usb.Removable || usb.DiskSerial != "2HC015KJ" || usb.MountPoint != mydisk || usb.Bytes != 0 {
		t.Fatalf("sdc1 (NTFS over USB with removable=0, escaped mount point, no partition size in sysfs): %+v", usb)
	}
	if data := by["nvme0n1p2"]; data.Removable || data.Disk != "/dev/nvme0n1" || data.Bytes != 1000000*512 {
		t.Fatalf("nvme0n1p2: %+v", data)
	}
	if gone, ok := by["sde1"]; !ok || gone.Disk != "/dev/sde" {
		t.Fatalf("sde1 (no device node) must still be listed: %+v", gone)
	}
	if _, err := (Enumerator{Mounts: filepath.Join(root, "absent")}).Volumes(); err == nil {
		t.Fatal("an unreadable mount table enumerated something")
	}

	disks, err := e.Disks()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, d := range disks {
		paths = append(paths, d.Path)
	}
	if strings.Join(paths, ",") != "/dev/nvme0n1,/dev/sdb,/dev/sdc" {
		t.Fatalf("disks %v: loop, ram and zram are not disks", paths)
	}
	if disks[1].Bytes != 8192*512 || !disks[1].Removable || disks[1].Serial != "WD-WX11A8765432" {
		t.Fatalf("sdb: %+v", disks[1])
	}
	if !disks[2].Removable || disks[2].Serial != "2HC015KJ" {
		t.Fatalf("sdc (USB, removable=0): %+v", disks[2])
	}
	if disks[0].Removable || disks[0].Bytes != 2000000*512 {
		t.Fatalf("nvme0n1: %+v", disks[0])
	}
	if _, err := (Enumerator{SysBlock: filepath.Join(root, "absent")}).Disks(); err == nil {
		t.Fatal("an unreadable sysfs enumerated disks")
	}

	// The root's major:minor is a partition of nvme0n1.
	sysDisk, err := e.SystemDisk()
	if err != nil || sysDisk != "/dev/nvme0n1" {
		t.Fatalf("system disk = %q, %v", sysDisk, err)
	}
	if _, err := (Enumerator{Mountinfo: filepath.Join(root, "absent"), SysBlock: sys}).SystemDisk(); err == nil {
		t.Fatal("no mountinfo resolved a system disk")
	}
	// A root directly on a whole disk (a VM image with no partition table).
	writeFixture(t, filepath.Join(root, "mi-whole"), "22 1 8:16 / / rw - ext4 /dev/sdb rw\n")
	if sd, err := (Enumerator{Mountinfo: filepath.Join(root, "mi-whole"), SysBlock: sys}).SystemDisk(); err != nil || sd != "/dev/sdb" {
		t.Fatalf("root on a whole disk: %q %v", sd, err)
	}
	writeFixture(t, filepath.Join(root, "mi2"), "22 1 8:99 / / rw - ext4 /dev/sdz rw\n")
	if _, err := (Enumerator{Mountinfo: filepath.Join(root, "mi2"), SysBlock: sys}).SystemDisk(); err == nil || !strings.Contains(err.Error(), "8:99") {
		t.Fatalf("an unknown root device resolved: %v", err)
	}
}
