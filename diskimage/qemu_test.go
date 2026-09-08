// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The second, external authority: qemu-img must open every export,
// report the right virtual size, pass its consistency check where the
// format has one, and convert it to a raw image identical to the source.
// The Linux unit job installs qemu-utils; elsewhere the test skips.
func TestQemuImgReadsEveryFormat(t *testing.T) {
	qemu, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img not on PATH (the Linux unit job installs qemu-utils)")
	}
	e := makeExpected()
	cases := []struct {
		name   string
		format Format
		opts   Options
		check  bool // formats whose driver implements `qemu-img check`
	}{
		{"raw", Raw, Options{}, false},
		{"vhd-dynamic", VHD, Options{}, false}, // qemu's vpc driver has no check; openVHD verifies checksums and the BAT
		{"vhd-fixed", VHD, Options{Fixed: true}, false},
		{"qcow2", QCOW2, Options{}, true},
		{"vmdk", VMDK, Options{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "disk"+tc.format.Extension())
			w, err := Create(tc.format, path, testDiskSize, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			writeTestImage(t, w, e)
			fmtName := string(tc.format)
			if tc.format == VHD {
				fmtName = "vpc"
			}
			info, err := exec.Command(qemu, "info", "-f", fmtName, "--output=json", path).CombinedOutput()
			if err != nil {
				t.Fatalf("qemu-img info: %v\n%s", err, info)
			}
			if want := `"virtual-size": ` + itoa(testDiskSize); !strings.Contains(string(info), want) {
				t.Fatalf("qemu-img info lacks %s:\n%s", want, info)
			}
			if tc.check {
				out, err := exec.Command(qemu, "check", "-f", fmtName, path).CombinedOutput()
				if err != nil {
					t.Fatalf("qemu-img check: %v\n%s", err, out)
				}
			}
			raw := filepath.Join(dir, "converted.img")
			out, err := exec.Command(qemu, "convert", "-f", fmtName, "-O", "raw", path, raw).CombinedOutput()
			if err != nil {
				t.Fatalf("qemu-img convert: %v\n%s", err, out)
			}
			r, err := openRaw(raw)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			compareImage(t, r, e)
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
