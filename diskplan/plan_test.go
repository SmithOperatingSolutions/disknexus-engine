// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"bytes"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout/gpttest"
)

// TestBuildDiskMemberPlans_NonWindowsRaw: off Windows the shared builder
// (now also used by the service's disk-capture kind) returns one raw plan
// per disk with a working cleanup.
func TestBuildDiskMemberPlans_NonWindowsRaw(t *testing.T) {
	img := gpttest.BuildGPT(t, 512, 8192, gpttest.StdWindowsParts())
	l, err := disklayout.Parse(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	plans, cleanup, err := BuildDiskMemberPlans([]string{"a.img", "b.img"},
		[]*disklayout.DiskLayout{l, l}, config.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(plans) != 2 {
		t.Fatalf("plans = %d, want 2", len(plans))
	}
	for i, p := range plans {
		if len(p) != len(l.Partitions) {
			t.Fatalf("plan %d has %d members, want %d", i, len(p), len(l.Partitions))
		}
		for _, m := range p {
			if m.Kind != disklayout.MemberRaw {
				t.Fatalf("non-Windows member kind = %q", m.Kind)
			}
		}
	}
}

// TestDiskNumberOfParsesPhysicalDrivePaths (#311, closing the VSS routing gap
// #309 documented): panel-configured Windows disk sources arrive as
// \\.\PhysicalDriveN device paths, not bare numbers. DiskNumberOf is the gate
// that decides whether BuildDiskMemberPlans correlates volumes and takes the
// VSS path — while it only parsed bare numbers, every panel-configured
// capture fell through to a raw read of a live disk. The device-path
// spellings must parse to the same number the bare form does.
func TestDiskNumberOfParsesPhysicalDrivePaths(t *testing.T) {
	cases := []struct {
		arg  string
		want uint32
		ok   bool
	}{
		{"0", 0, true},
		{"17", 17, true},
		{`\\.\PhysicalDrive0`, 0, true},
		{`\\.\PhysicalDrive3`, 3, true},
		{`\\.\physicaldrive2`, 2, true}, // Windows device names are case-insensitive
		{`\\?\PhysicalDrive1`, 1, true}, // the \\?\ prefix opens the same device
		{`\\.\PhysicalDrive`, 0, false}, // no number
		{`\\.\C:`, 0, false},            // a volume, not a disk
		{"disk0.img", 0, false},
		{"/dev/sda", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := DiskNumberOf(c.arg)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("DiskNumberOf(%q) = (%d, %v), want (%d, %v)", c.arg, got, ok, c.want, c.ok)
		}
	}
}
