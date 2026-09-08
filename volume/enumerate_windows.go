// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	govss "github.com/SmithOperatingSolutions/go-vss"
	"golang.org/x/sys/windows"
)

// Windows: every \\?\Volume{GUID}\ the volume manager knows, its mount
// path, label and filesystem from GetVolumeInformation, the disk it sits
// on from its extents, and that disk's removable bit, bus type and serial
// from IOCTL_STORAGE_QUERY_PROPERTY. Disks are \\.\PhysicalDrive0..63,
// sized through the same IOCTL a capture uses.

const (
	ioctlStorageQueryProperty = 0x002D1400
	driveRemovable            = 2 // GetDriveType: DRIVE_REMOVABLE
	maxPhysicalDrives         = 64
)

func (e Enumerator) volumes() ([]VolumeInfo, error) {
	vols, err := govss.EnumerateVolumes()
	if err != nil {
		return nil, fmt.Errorf("enumerating volumes: %w", err)
	}
	out := []VolumeInfo{}
	for _, gv := range vols {
		v := VolumeInfo{
			Device:     strings.TrimSuffix(gv.VolumeName, `\`), // raw volume open form
			MountPoint: gv.MountPoint,
		}
		if v.MountPoint == gv.VolumeName {
			v.MountPoint = "" // resolved by GUID only: nowhere in the namespace
		}
		label, fsName, ok := volumeInformation(gv.VolumeName)
		if !ok {
			continue // CD-ROM with no media, an unformatted volume: not a candidate
		}
		v.Label = label
		v.Filesystem = strings.ToLower(fsName)
		if exts, err := gv.DiskExtents(); err == nil && len(exts) > 0 {
			for _, x := range exts {
				v.Bytes += x.Length
			}
			d := diskProperties(exts[0].DiskNumber)
			v.DiskSerial = d.serial
			v.Removable = d.removable
			v.Disk = physicalDrive(exts[0].DiskNumber)
		}
		if v.MountPoint != "" && driveType(v.MountPoint) == driveRemovable {
			v.Removable = true
		}
		out = append(out, v)
	}
	return out, nil
}

func physicalDrive(n uint32) string { return fmt.Sprintf(`\\.\PhysicalDrive%d`, n) }

func volumeInformation(root string) (label, fsName string, ok bool) {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", "", false
	}
	var labelBuf, fsBuf [261]uint16
	var serial, maxComp, flags uint32
	if err := windows.GetVolumeInformation(p, &labelBuf[0], uint32(len(labelBuf)), &serial, &maxComp, &flags, &fsBuf[0], uint32(len(fsBuf))); err != nil {
		return "", "", false
	}
	return windows.UTF16ToString(labelBuf[:]), windows.UTF16ToString(fsBuf[:]), true
}

func driveType(root string) uint32 {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0
	}
	return windows.GetDriveType(p)
}

// diskProperties asks the physical drive for STORAGE_DEVICE_DESCRIPTOR.
func diskProperties(diskNumber uint32) diskProps {
	path, err := windows.UTF16PtrFromString(physicalDrive(diskNumber))
	if err != nil {
		return diskProps{}
	}
	h, err := windows.CreateFile(path, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return diskProps{}
	}
	defer windows.CloseHandle(h)
	// STORAGE_PROPERTY_QUERY{PropertyId: StorageDeviceProperty(0), QueryType: PropertyStandardQuery(0)}
	query := make([]byte, 12)
	buf := make([]byte, 4096)
	var ret uint32
	if err := windows.DeviceIoControl(h, ioctlStorageQueryProperty, &query[0], uint32(len(query)), &buf[0], uint32(len(buf)), &ret, nil); err != nil {
		return diskProps{}
	}
	return parseDeviceDescriptor(buf[:ret])
}

// disks probes PhysicalDrive0..63 (gaps are normal) and sizes each through
// IOCTL_DISK_GET_LENGTH_INFO, the derivation a capture uses.
func (e Enumerator) disks() ([]Disk, error) {
	out := []Disk{}
	for n := uint32(0); n < maxPhysicalDrives; n++ {
		p, err := windows.UTF16PtrFromString(physicalDrive(n))
		if err != nil {
			continue
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			continue // no such drive
		}
		size, err := deviceSizeHandle(syscall.Handle(h))
		windows.CloseHandle(h)
		if err != nil {
			continue
		}
		props := diskProperties(n)
		out = append(out, Disk{Path: physicalDrive(n), Bytes: size, Serial: props.serial, Removable: props.removable})
	}
	return out, nil
}

// systemDisk is the disk holding the system drive (C: unless SystemDrive
// says otherwise).
func (e Enumerator) systemDisk() (string, error) {
	sys := os.Getenv("SystemDrive")
	if sys == "" {
		sys = "C:"
	}
	vols, err := govss.EnumerateVolumes()
	if err != nil {
		return "", err
	}
	for _, gv := range vols {
		if !strings.EqualFold(strings.TrimSuffix(gv.MountPoint, `\`), sys) {
			continue
		}
		if exts, err := gv.DiskExtents(); err == nil && len(exts) > 0 {
			return physicalDrive(exts[0].DiskNumber), nil
		}
	}
	return "", fmt.Errorf("no volume mounted at %s", sys)
}
