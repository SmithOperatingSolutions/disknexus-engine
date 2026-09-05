// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "testing"

// isDevicePath decides whether NewReader/NewWriter open a raw device (with
// platform-specific alignment and no O_CREATE) or an ordinary file. Getting it
// wrong in either direction is operator-visible: a device misread as a file
// path gets a plain os.OpenFile that fails or, worse for the writer, tries to
// CREATE the device node; a file misread as a device goes through the
// device-open path and fails restore on a perfectly good image file.
//
// The previous version of this table (reader_test.go, package volume_test)
// could not call the unexported function and asserted nothing — every row
// ended in `_ = tests` (#402). This file is in-package so every row is real.
func TestIsDevicePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{`\\.\PhysicalDrive0`, true},
		{`//./PhysicalDrive1`, true},
		{"/dev/sda", true},
		{"/dev/nvme0n1", true},
		{"/home/user/disk.img", false},
		{`C:\backup\disk.img`, false},
		{"", false},
	}

	for _, tt := range tests {
		if got := isDevicePath(tt.path); got != tt.want {
			t.Errorf("isDevicePath(%q) = %v, want %v — a wrong classification sends this path down the "+
				"wrong open path (raw device open for a file, or plain OpenFile for a device)",
				tt.path, got, tt.want)
		}
	}
}
