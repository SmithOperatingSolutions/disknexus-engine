// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

func TestParseExclusion_Vocabulary(t *testing.T) {
	ok := []struct{ in, drive, rel string }{
		{`C:\Users\x\VMs`, "C:", "Users/x/VMs"},
		{`c:/users/x/vms/`, "C:", "users/x/vms"},
		{`D:\Scratch\*`, "D:", "Scratch"}, // trailing \* = the subtree, which a path already means
		{`  E:\cache  `, "E:", "cache"},
		{`C:\pagefile.sys`, "C:", "pagefile.sys"}, // a single file is fine
	}
	for _, c := range ok {
		e, err := ParseExclusion(c.in)
		if err != nil {
			t.Errorf("%q: unexpected refusal: %v", c.in, err)
			continue
		}
		if e.Drive != c.drive || e.Rel != c.rel || e.Raw != c.in {
			t.Errorf("%q → drive %q rel %q raw %q; want %q %q %q", c.in, e.Drive, e.Rel, e.Raw, c.drive, c.rel, c.in)
		}
	}
	if s := (Exclusion{Drive: "C:", Rel: "Users/x/VMs"}).String(); s != `C:\Users\x\VMs` {
		t.Errorf("String() = %q", s)
	}

	// Refusals (§4: the accepted cases above are the positive control).
	bad := []struct{ in, want string }{
		{"", "empty"},
		{`\Users\x`, "drive letter"},
		{`Users\x`, "drive letter"},
		{`C:`, "whole volume"},
		{`C:\`, "whole volume"},
		{`C:\*`, "whole volume"},
		{`C:\Users\*\Temp`, "wildcards"},
		{`C:\Users\x\?.tmp`, "wildcards"},
		{`C:\Users\..\Windows`, "relative"},
		{`C:\Users\\x`, "empty or relative"},
	}
	for _, c := range bad {
		if _, err := ParseExclusion(c.in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want a refusal mentioning %q", c.in, err, c.want)
		}
	}
}

func ext4Fixture(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "volumefs", "testdata", "ext4.img")
}

// ApplyExclusions on the real NTFS fixture: the operator's path resolves
// to exactly the file's extents (authority: the scanner's catalog), and
// every way it can fail to apply is a distinct, named outcome — because an
// exclusion that silently did not apply is data in a backup the operator
// believes is not there.
func TestApplyExclusions_OutcomesAgainstTheFixture(t *testing.T) {
	img := ntfsFixture(t)
	excls := []Exclusion{
		mustParse(t, `X:\dir1`),      // exists on the fixture
		mustParse(t, `X:\nope\here`), // right volume, no such path
		mustParse(t, `Y:\dir1`),      // another drive
	}
	m := volume.NewExclusionMap()
	out := ApplyExclusions(img, "X:", m, excls)
	if len(out) != 3 {
		t.Fatalf("%d outcomes for 3 exclusions", len(out))
	}
	if out[0].Status != ExclusionApplied || out[0].Bytes <= 0 {
		t.Fatalf("X:\\dir1 on the volume that holds it: %+v, want applied with bytes > 0", out[0])
	}
	if out[1].Status != ExclusionNotFound {
		t.Fatalf("X:\\nope\\here: %+v, want not-found — 'I could not find it' must not read as 'excluded'", out[1])
	}
	if out[2].Status != ExclusionNotOnVolume {
		t.Fatalf("Y:\\dir1 against X:: %+v, want not-on-volume", out[2])
	}

	// Authority (§3): the scanner's extents for dir1/data.bin are all in the
	// map, and the map is about that file's size, not the volume's.
	res, err := volumefs.ScanVolume(context.Background(), img, 8<<20, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var exts int
	var fileBytes int64
	for _, f := range res.Files {
		if strings.TrimPrefix(f.Path, "./") != "dir1/data.bin" {
			continue
		}
		for _, e := range f.VolumeExtents {
			exts++
			fileBytes += e.Length
			if !m.IsExcluded(e.VolumeOffset, e.Length) {
				t.Errorf("dir1/data.bin extent [%d,+%d) is not excluded", e.VolumeOffset, e.Length)
			}
		}
	}
	if exts == 0 {
		t.Fatal("fixture has no extents for dir1/data.bin — the assertion above checked nothing")
	}
	if out[0].Bytes < fileBytes || out[0].Bytes > 4*fileBytes {
		t.Fatalf("applied bytes = %d for a %d-byte file — the reported size is not the file's", out[0].Bytes, fileBytes)
	}
	if m.IsExcluded(0, 512) {
		t.Error("the boot sector is excluded — over-exclusion")
	}

	// An image with no letter takes every exclusion as its own.
	m2 := volume.NewExclusionMap()
	if o := ApplyExclusions(img, "", m2, excls[2:]); o[0].Status != ExclusionApplied {
		t.Fatalf("letterless capture: Y:\\dir1 → %+v, want applied", o[0])
	}

	// Non-NTFS: named as such, never silently skipped.
	m3 := volume.NewExclusionMap()
	if o := ApplyExclusions(ext4Fixture(t), "X:", m3, excls[:1]); o[0].Status != ExclusionUnsupported || o[0].Detail != "ext4" {
		t.Fatalf("ext4 volume: %+v, want unsupported-filesystem/ext4", o[0])
	}
	// A source that cannot be opened: failed, with the error.
	if o := ApplyExclusions(filepath.Join(t.TempDir(), "missing.img"), "X:", volume.NewExclusionMap(), excls[:1]); o[0].Status != ExclusionFailed || o[0].Detail == "" {
		t.Fatalf("unopenable source: %+v, want failed with a detail", o[0])
	}

	// The operator-facing lines: only the applied one says "excluding";
	// every other outcome says the data is IN the backup.
	for _, o := range out[1:] {
		if o.Status != ExclusionNotOnVolume && !strings.Contains(o.Describe(), "IN this backup") {
			t.Errorf("%s: %q does not say the data is in the backup", o.Status, o.Describe())
		}
	}
	if d := out[0].Describe(); !strings.HasPrefix(d, `excluding X:\dir1 (`) {
		t.Errorf("applied line = %q", d)
	}
}

func mustParse(t *testing.T, raw string) Exclusion {
	t.Helper()
	e, err := ParseExclusion(raw)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
