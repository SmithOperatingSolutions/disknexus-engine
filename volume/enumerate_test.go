// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestWholeDisk(t *testing.T) {
	for node, want := range map[string]string{
		"sdb1": "sdb", "sdb": "sdb", "sda12": "sda", "nvme0n1p2": "nvme0n1", "nvme0n1": "nvme0n1",
		"mmcblk0p1": "mmcblk0", "mmcblk0": "mmcblk0", "vda3": "vda",
	} {
		if got := wholeDisk(node); got != want {
			t.Errorf("wholeDisk(%q) = %q, want %q", node, got, want)
		}
	}
	for id, want := range map[string]string{"disk0s2": "disk0", "/dev/disk3s1": "disk3", "/dev/rdisk5": "disk5", "disk12": "disk12", "sda1": "", "disk": ""} {
		if got := macWholeDisk(id); got != want {
			t.Errorf("macWholeDisk(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/media/u/My\040Disk\011x`); got != "/media/u/My Disk\tx" {
		t.Fatalf("unescape = %q", got)
	}
	if got := unescapeMount("/plain"); got != "/plain" {
		t.Fatalf("plain = %q", got)
	}
}

func TestParseDeviceDescriptor(t *testing.T) {
	build := func(removable byte, bus uint32, serial string) []byte {
		b := make([]byte, 64)
		b[10] = removable
		binary.LittleEndian.PutUint32(b[28:32], bus)
		if serial != "" {
			binary.LittleEndian.PutUint32(b[24:28], 40)
			copy(b[40:], serial)
		}
		return b
	}
	if p := parseDeviceDescriptor(build(0, 11 /* SATA */, "  S3Z9NB0K123  ")); p.removable || p.serial != "S3Z9NB0K123" {
		t.Fatalf("internal SATA disk: %+v", p)
	}
	if p := parseDeviceDescriptor(build(1, 11, "")); !p.removable || p.serial != "" {
		t.Fatalf("removable media bit: %+v", p)
	}
	for _, bus := range []uint32{busTypeUSB, busTypeSD, busTypeMMC} {
		if p := parseDeviceDescriptor(build(0, bus, "X")); !p.removable {
			t.Fatalf("bus %d with media bit 0 was not removable", bus)
		}
	}
	if p := parseDeviceDescriptor(make([]byte, 16)); p.removable || p.serial != "" {
		t.Fatalf("a short descriptor produced facts: %+v", p)
	}
	bad := build(0, busTypeUSB, "")
	binary.LittleEndian.PutUint32(bad[24:28], 4000) // serial offset past the buffer
	if p := parseDeviceDescriptor(bad); p.serial != "" {
		t.Fatalf("an out-of-range serial offset was read: %+v", p)
	}
}

func TestParsePlistTypes(t *testing.T) {
	d, err := parsePlist([]byte(`<?xml version="1.0"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>s</key><string>x</string><key>i</key><integer>42</integer><key>r</key><real>1.5</real><key>t</key><true/><key>f</key><false/>
<key>a</key><array><dict><key>k</key><string>v</string></dict><integer>7</integer></array><key>d</key><date>2026-09-06T00:00:00Z</date></dict></plist>`))
	if err != nil {
		t.Fatal(err)
	}
	if d.str("s") != "x" || d.integer("i") != 42 || d.integer("r") != 1 || !d.boolean("t") || d.boolean("f") || d.str("d") == "" {
		t.Fatalf("parsed %+v", d)
	}
	if ds := d.dicts("a"); len(ds) != 1 || ds[0].str("k") != "v" {
		t.Fatalf("array of dicts: %+v", ds)
	}
	if _, err := parsePlist([]byte(`<plist><array/></plist>`)); err == nil {
		t.Fatal("a top-level array parsed as a dict")
	}
	if _, err := parsePlist([]byte(`<plist><dict><key>x</key><integer>abc</integer></dict></plist>`)); err == nil {
		t.Fatal("a non-numeric integer parsed")
	}
}

const diskutilList = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>AllDisksAndPartitions</key><array>
 <dict><key>DeviceIdentifier</key><string>disk0</string><key>Size</key><integer>500000000000</integer>
  <key>Partitions</key><array>
   <dict><key>DeviceIdentifier</key><string>disk0s1</string><key>Content</key><string>EFI</string><key>Size</key><integer>314572800</integer></dict>
   <dict><key>DeviceIdentifier</key><string>disk0s2</string><key>Content</key><string>Apple_APFS</string><key>Size</key><integer>499000000000</integer></dict>
  </array></dict>
 <dict><key>DeviceIdentifier</key><string>disk4</string><key>Size</key><integer>64000000000</integer>
  <key>Partitions</key><array>
   <dict><key>DeviceIdentifier</key><string>disk4s1</string><key>MountPoint</key><string>/Volumes/PHOTOS</string><key>VolumeName</key><string>PHOTOS</string><key>Size</key><integer>63900000000</integer></dict>
  </array></dict>
 <dict><key>DeviceIdentifier</key><string>disk5</string><key>MountPoint</key><string>/Volumes/RAW</string><key>VolumeName</key><string>RAW</string><key>Size</key><integer>16000000000</integer></dict>
 <dict><key>DeviceIdentifier</key><string>disk3</string><key>Size</key><integer>499000000000</integer>
  <key>APFSPhysicalStores</key><array><dict><key>DeviceIdentifier</key><string>disk0s2</string></dict></array>
  <key>APFSVolumes</key><array>
   <dict><key>DeviceIdentifier</key><string>disk3s1</string><key>MountPoint</key><string>/</string><key>VolumeName</key><string>Macintosh HD</string><key>Size</key><integer>499000000000</integer></dict>
   <dict><key>DeviceIdentifier</key><string>disk3s5</string><key>MountPoint</key><string>/System/Volumes/Data</string><key>VolumeName</key><string>Data</string><key>Size</key><integer>499000000000</integer></dict>
  </array></dict>
 <dict><key>DeviceIdentifier</key><string>disk6</string><key>Size</key><integer>2000000000000</integer>
  <key>Partitions</key><array>
   <dict><key>DeviceIdentifier</key><string>disk6s2</string><key>MountPoint</key><string>/Volumes/Archive</string><key>VolumeName</key><string>Archive</string><key>Size</key><integer>1999000000000</integer></dict>
  </array></dict>
</array></dict></plist>`

func diskutilInfo(id string) string {
	switch id {
	case "disk0s2":
		return `<plist version="1.0"><dict><key>DeviceNode</key><string>/dev/disk0s2</string><key>Internal</key><true/><key>RemovableMediaOrExternalDevice</key><false/><key>ParentWholeDisk</key><string>disk0</string><key>Content</key><string>Apple_APFS</string></dict></plist>`
	case "disk3s1", "disk3s5": // APFS volumes of the internal container
		return `<plist version="1.0"><dict><key>DeviceNode</key><string>/dev/` + id + `</string><key>Internal</key><true/><key>RemovableMediaOrExternalDevice</key><false/><key>ParentWholeDisk</key><string>disk3</string><key>FilesystemType</key><string>apfs</string></dict></plist>`
	case "disk3":
		return `<plist version="1.0"><dict><key>DiskUUID</key><string>3333-CONTAINER</string><key>APFSPhysicalStores</key><array><dict><key>DeviceIdentifier</key><string>disk0s2</string></dict></array></dict></plist>`
	case "disk4s1":
		return `<plist version="1.0"><dict><key>DeviceNode</key><string>/dev/disk4s1</string><key>Internal</key><false/><key>RemovableMediaOrExternalDevice</key><true/><key>ParentWholeDisk</key><string>disk4</string></dict></plist>`
	case "disk5": // a whole-disk filesystem: the volume and its parent are one node
		return `<plist version="1.0"><dict><key>DeviceNode</key><string>/dev/disk5</string><key>Internal</key><false/><key>RemovableMediaOrExternalDevice</key><false/><key>Removable</key><true/><key>ParentWholeDisk</key><string>disk5</string><key>MediaName</key><string>SanDisk Cruzer</string></dict></plist>`
	case "disk6s2": // an external SSD diskutil reports only as not internal
		return `<plist version="1.0"><dict><key>DeviceNode</key><string>/dev/disk6s2</string><key>Internal</key><false/><key>ParentWholeDisk</key><string>disk6</string></dict></plist>`
	case "disk6":
		return `<plist version="1.0"><dict><key>DiskUUID</key><string>6666-EXTERNAL-SSD</string><key>Internal</key><false/></dict></plist>`
	case "disk0":
		return `<plist version="1.0"><dict><key>DiskUUID</key><string>0000-INTERNAL</string><key>Internal</key><true/></dict></plist>`
	case "disk4":
		return `<plist version="1.0"><dict><key>DiskUUID</key><string>4444-EXTERNAL</string><key>RemovableMediaOrExternalDevice</key><true/></dict></plist>`
	}
	return `<plist version="1.0"><dict/></plist>`
}

func fixtureEnumerator(calls *[]string) Enumerator {
	return Enumerator{Diskutil: func(args ...string) ([]byte, error) {
		if calls != nil {
			*calls = append(*calls, strings.Join(args, " "))
		}
		if args[0] == "list" {
			return []byte(diskutilList), nil
		}
		return []byte(diskutilInfo(args[2])), nil
	}}
}

// macOS composition over captured-shape diskutil plists: partitions and
// whole-disk filesystems, raw nodes, removable from the external/removable
// keys, the whole disk's UUID as the drive mark, EFI (no mount point)
// listed, disks without the synthesized container, and the system disk
// resolved through the APFS physical store.
func TestDiskutilComposition(t *testing.T) {
	calls := []string{}
	e := fixtureEnumerator(&calls)
	vols, err := e.volumesFromDiskutil([]byte(diskutilList))
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]VolumeInfo{}
	for _, v := range vols {
		by[v.Device] = v
	}
	if len(vols) != 7 {
		t.Fatalf("listed %d volumes, want 7 (EFI, the APFS physical store, the system and Data volumes, PHOTOS, RAW, Archive): %+v", len(vols), vols)
	}
	if data := by["/dev/rdisk3s5"]; data.MountPoint != "/System/Volumes/Data" || data.DiskSerial != "3333-CONTAINER" || data.Disk != "/dev/disk3" || data.Removable || data.Filesystem != "apfs" {
		t.Fatalf("the APFS Data volume (under APFSVolumes, not Partitions): %+v", data)
	}
	if arch := by["/dev/rdisk6s2"]; !arch.Removable || arch.Disk != "/dev/disk6" {
		t.Fatalf("an external SSD reported only as Internal=false was not removable: %+v", arch)
	}
	if sys := by["/dev/rdisk3s1"]; sys.Removable || sys.MountPoint != "/" || sys.DiskSerial != "3333-CONTAINER" {
		t.Fatalf("system volume: %+v", sys)
	}
	if photos := by["/dev/rdisk4s1"]; !photos.Removable || photos.DiskSerial != "4444-EXTERNAL" || photos.Bytes != 63900000000 || photos.Label != "PHOTOS" {
		t.Fatalf("external PHOTOS: %+v", photos)
	}
	if raw := by["/dev/rdisk5"]; !raw.Removable || raw.MountPoint != "/Volumes/RAW" || raw.DiskSerial != "SanDisk Cruzer" || raw.Disk != "/dev/disk5" {
		t.Fatalf("whole-disk FAT32 stick (Removable key, no DiskUUID): %+v", raw)
	}
	if efi, ok := by["/dev/rdisk0s1"]; !ok || efi.MountPoint != "" {
		t.Fatalf("EFI partition (unmounted) must be listed: %+v", efi)
	}
	if len(calls) < 6 || calls[0] != "info -plist disk0s1" {
		t.Fatalf("diskutil info was not asked per partition: %v", calls)
	}

	disks, err := e.disksFromDiskutil([]byte(diskutilList))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, d := range disks {
		paths = append(paths, d.Path)
	}
	if strings.Join(paths, ",") != "/dev/disk0,/dev/disk4,/dev/disk5,/dev/disk6" {
		t.Fatalf("disks %v: the synthesized APFS container is not a disk", paths)
	}
	if disks[0].Bytes != 500000000000 || disks[0].Removable || disks[0].Serial != "0000-INTERNAL" {
		t.Fatalf("disk0: %+v", disks[0])
	}
	if !disks[1].Removable || disks[1].Serial != "4444-EXTERNAL" || !disks[2].Removable || disks[2].Serial != "SanDisk Cruzer" || !disks[3].Removable {
		t.Fatalf("external disks: %+v", disks[1:])
	}

	// The root volume is an APFS volume of a synthesized container: the
	// system disk is the container's physical store's disk.
	sys, err := e.systemDiskFromDiskutil([]byte(diskutilInfo("disk3s1")))
	if err != nil || sys != "/dev/disk0" {
		t.Fatalf("system disk = %q, %v (want /dev/disk0 through disk3's physical store disk0s2)", sys, err)
	}
	// The root's own info names the physical store (what diskutil info /
	// reports for an APFS volume): resolved there, never through the
	// container it names as its parent.
	rootInfo := `<plist version="1.0"><dict><key>ParentWholeDisk</key><string>disk9</string><key>APFSPhysicalStores</key><array><dict><key>DeviceIdentifier</key><string>disk0s2</string></dict></array></dict></plist>`
	if sys, err := e.systemDiskFromDiskutil([]byte(rootInfo)); err != nil || sys != "/dev/disk0" {
		t.Fatalf("root with its own physical store = %q, %v (want /dev/disk0, not the container disk9)", sys, err)
	}
	// diskutil info spells the store as APFSPhysicalStore (list says
	// DeviceIdentifier): the shape a real `diskutil info -plist /` has.
	infoShaped := `<plist version="1.0"><dict><key>ParentWholeDisk</key><string>disk1</string><key>APFSPhysicalStores</key><array><dict><key>APFSPhysicalStore</key><string>disk0s2</string></dict></array></dict></plist>`
	if sys, err := e.systemDiskFromDiskutil([]byte(infoShaped)); err != nil || sys != "/dev/disk0" {
		t.Fatalf("info-shaped physical store = %q, %v (want /dev/disk0, not the container disk1)", sys, err)
	}
	if sys, err := e.systemDiskFromDiskutil([]byte(diskutilInfo("disk4s1"))); err != nil || sys != "/dev/disk4" {
		t.Fatalf("plain root: %q %v", sys, err)
	}
	if _, err := e.systemDiskFromDiskutil([]byte(`<plist version="1.0"><dict/></plist>`)); err == nil {
		t.Fatal("no parent disk resolved to something")
	}
}
