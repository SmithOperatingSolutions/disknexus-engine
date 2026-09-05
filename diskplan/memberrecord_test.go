// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"reflect"
	"testing"
)

// A disk member's manifest records the exclusions that were FOR it and the
// ones that did not apply (#468). An exclusion for another drive is another
// member's business: recording it here would make a restore-day message on
// D: blame an exclusion written for C:.
func TestMemberExclusionRecordKeepsThisVolumesExclusionsOnly(t *testing.T) {
	c1 := mustParse(t, `C:\Users\x\VMs`)
	c2 := mustParse(t, `C:\gone`)
	d1 := mustParse(t, `D:\scratch`)
	outcomes := []ExclusionOutcome{
		{Exclusion: c1, Status: ExclusionApplied, Bytes: 4096},
		{Exclusion: d1, Status: ExclusionNotOnVolume},
		{Exclusion: c2, Status: ExclusionNotFound},
	}
	paths, warnings := MemberExclusionRecord(outcomes)
	if want := []string{`C:\Users\x\VMs`, `C:\gone`}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %q, want %q — D:\\scratch is another member's, and order is the operator's", paths, want)
	}
	if len(warnings) != 1 || warnings[0] != outcomes[2].Describe() {
		t.Fatalf("warnings = %q, want exactly the not-found line for C:\\gone", warnings)
	}
	// Positive control: all applied → paths recorded, no warnings.
	p, w := MemberExclusionRecord(outcomes[:1])
	if len(p) != 1 || len(w) != 0 {
		t.Fatalf("all-applied: paths %q warnings %q", p, w)
	}
	if p, w := MemberExclusionRecord(nil); p != nil || w != nil {
		t.Fatalf("no outcomes: %q %q", p, w)
	}
}
