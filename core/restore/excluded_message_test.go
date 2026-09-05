// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// An excluded file discovered at restore must point at the exclusion that
// caused it (#468): the operator's own, by name, or the built-in set.
func TestExcludedFileErrorNamesTheOperatorExclusion(t *testing.T) {
	b := &manifest.Backup{ExcludePaths: []string{`C:\Users\x\VMs`, `C:\Scratch\*`}}

	cases := []struct {
		path string
		want string // substring of the message
	}{
		{"Users/x/VMs/win11.vhdx", `C:\Users\x\VMs`},
		{"./Users/X/vms/nested/deep.bin", `C:\Users\x\VMs`}, // NTFS is case-insensitive; "./" prefix is the scanner's
		{"Users/x/VMs", `C:\Users\x\VMs`},                   // the directory entry itself
		{"scratch/tmp.dat", `C:\Scratch\*`},                 // trailing \* form is still the operator's spelling
	}
	for _, c := range cases {
		err := excludedFileError(b, manifest.FileEntry{Path: c.path, IsExcluded: true})
		if !strings.Contains(err.Error(), "in this device's backup settings") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v\n  want the message to name %s and the device's backup settings", c.path, err, c.want)
		}
	}

	// Built-in (§4 positive control for the other branch): pagefile.sys is
	// on no operator list, and a SIBLING of an excluded directory is not
	// under it — "Users/x/VMs2" must not be blamed on "Users/x/VMs".
	for _, p := range []string{"pagefile.sys", "Users/x/VMs2/other.bin", "Users/x/VM/x.bin"} {
		err := excludedFileError(b, manifest.FileEntry{Path: p, IsExcluded: true})
		if !strings.Contains(err.Error(), "built-in") || strings.Contains(err.Error(), "backup settings") {
			t.Errorf("%s: %v\n  want the built-in wording, not an operator exclusion it is not under", p, err)
		}
	}
	// No backup at all (a caller with no manifest in hand) still explains.
	if err := excludedFileError(nil, manifest.FileEntry{Path: "hiberfil.sys"}); !strings.Contains(err.Error(), "built-in") {
		t.Errorf("nil backup: %v", err)
	}
}
