// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import "testing"

// TestFileEntryExcludedFlagRoundTrip (#94): the IsExcluded catalog mark must
// survive the binary .dnm encoding, and its flag bit must not disturb the
// existing bits.
func TestFileEntryExcludedFlagRoundTrip(t *testing.T) {
	fe := FileEntry{
		Path: "pagefile.sys", Size: 4096, Mode: 0644,
		IsExcluded:    true,
		VolumeExtents: []VolumeExtent{{FileOffset: 0, VolumeOffset: 8192, Length: 4096}},
	}
	got, err := decodeFileEntry(encodeFileEntry(fe))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsExcluded {
		t.Fatal("IsExcluded lost in dnm round-trip")
	}
	if got.IsDir || got.IsSymlink || got.Unchanged {
		t.Fatal("excluded flag bled into other flag bits")
	}

	fe.IsExcluded = false
	got, err = decodeFileEntry(encodeFileEntry(fe))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsExcluded {
		t.Fatal("IsExcluded false did not round-trip")
	}
}
