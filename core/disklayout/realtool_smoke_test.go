// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"os"
	"testing"
)

// TestParseRealToolImage cross-validates the parser against a GPT written by
// standard tooling (sgdisk), when the DISKNEXUS_GPT_IMAGE env var points at
// one. Skipped otherwise — CI has no sgdisk dependency.
func TestParseRealToolImage(t *testing.T) {
	path := os.Getenv("DISKNEXUS_GPT_IMAGE")
	if path == "" {
		t.Skip("DISKNEXUS_GPT_IMAGE not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	l, err := Parse(f, st.Size())
	if err != nil {
		t.Fatalf("parse real image: %v", err)
	}
	if len(l.Partitions) != 4 {
		t.Fatalf("want 4 partitions, got %+v", l.Partitions)
	}
	// The four standard Windows partition types, as written by sgdisk codes
	// ef00 / 0c01 / 0700 / 2700 — pins our GUID constants to real tooling.
	for i, want := range []string{TypeESP, TypeMSR, TypeMSBasicData, TypeWinRE} {
		if l.Partitions[i].TypeGUID != want {
			t.Fatalf("p%d type = %s, want %s (%s)", i, l.Partitions[i].TypeGUID, want, l.Partitions[i].Name)
		}
	}
	if l.Partitions[0].Name != "EFI system partition" {
		t.Fatalf("p0 name: %+v", l.Partitions[0])
	}
	if err := l.VerifyBackupHeader(f); err != nil {
		t.Fatalf("backup header: %v", err)
	}
	t.Logf("real image OK: guid=%s, 4 Windows partition types verified", l.DiskGUID)
}
