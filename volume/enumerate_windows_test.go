// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"os"
	"strings"
	"testing"
)

// The runner's own machine: the system drive is listed as a GUID volume on
// a PhysicalDrive with a size, not removable; that drive is among the
// disks and is the system disk.
func TestWindowsEnumerationFacts(t *testing.T) {
	sysRoot := os.Getenv("SystemDrive") + `\`
	vols, err := Enumerator{}.Volumes()
	if err != nil {
		t.Fatal(err)
	}
	var sys *VolumeInfo
	for i := range vols {
		if strings.EqualFold(vols[i].MountPoint, sysRoot) {
			sys = &vols[i]
		}
	}
	if sys == nil {
		t.Fatalf("the system drive %s is not among %+v", sysRoot, vols)
	}
	if sys.Filesystem != "ntfs" || sys.Bytes <= 0 || !strings.HasPrefix(sys.Device, `\\?\Volume{`) || !strings.HasPrefix(sys.Disk, `\\.\PhysicalDrive`) || sys.Removable {
		t.Fatalf("system drive: %+v", sys)
	}
	disks, err := Enumerator{}.Disks()
	if err != nil {
		t.Fatal(err)
	}
	var found *Disk
	for i := range disks {
		if disks[i].Path == sys.Disk {
			found = &disks[i]
		}
	}
	if found == nil || found.Bytes < sys.Bytes || found.Removable {
		t.Fatalf("the system drive's disk %s is not among the disks: %+v", sys.Disk, disks)
	}
	sd, err := Enumerator{}.SystemDisk()
	if err != nil || sd != sys.Disk {
		t.Fatalf("system disk = %q, %v; the system volume sits on %s", sd, err, sys.Disk)
	}
}
