//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import "testing"

func TestVolumeRelative(t *testing.T) {
	cases := []struct {
		lp, vol string
		want    string
		ok      bool
	}{
		{`C:\repo`, "C:", "repo", true},
		{`C:\Users\x\AppData\Local\Temp\disknexus-s3-1`, "C:", "Users/x/AppData/Local/Temp/disknexus-s3-1", true},
		{`c:\repo`, "C:", "repo", true},  // case-insensitive drive
		{`C:\repo`, `C:\`, "repo", true}, // trailing-backslash volume form
		{`D:\repo`, "C:", "", false},     // different volume: MUST be ignored
		{`C:\repo`, "", "", false},       // image capture: no volume identity
		{`C:\`, "C:", "", false},         // whole volume is not a subtree
	}
	for _, c := range cases {
		got, ok := volumeRelative(c.lp, c.vol)
		if got != c.want || ok != c.ok {
			t.Errorf("volumeRelative(%q, %q) = (%q, %v), want (%q, %v)", c.lp, c.vol, got, ok, c.want, c.ok)
		}
	}
}
