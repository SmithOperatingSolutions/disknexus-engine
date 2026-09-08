// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"strings"
	"testing"
)

// The runner's own Mac: diskutil lists at least one sized /dev/diskN, the
// root volume resolves to one of them, and no synthesized APFS container
// is counted as a disk. Fixtures cannot prove the key names a real
// diskutil emits; this does.
func TestDarwinEnumerationFacts(t *testing.T) {
	e := Enumerator{}
	disks, err := e.Disks()
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) == 0 {
		t.Fatal("diskutil listed no disks")
	}
	paths := map[string]bool{}
	for _, d := range disks {
		if !strings.HasPrefix(d.Path, "/dev/disk") || d.Bytes <= 0 {
			t.Fatalf("disk %+v: want a sized /dev/diskN", d)
		}
		paths[d.Path] = true
	}
	sys, err := e.SystemDisk()
	if err != nil {
		t.Fatalf("system disk: %v", err)
	}
	if !paths[sys] {
		t.Fatalf("system disk %s is not among the disks %v", sys, disks)
	}
	vols, err := e.Volumes()
	if err != nil {
		t.Fatal(err)
	}
	var root *VolumeInfo
	for i := range vols {
		if vols[i].MountPoint == "/" {
			root = &vols[i]
		}
	}
	if root == nil {
		t.Fatalf("the root volume is not among %+v", vols)
	}
	if !strings.HasPrefix(root.Device, "/dev/rdisk") || root.Removable {
		t.Fatalf("root volume: %+v", root)
	}
}
