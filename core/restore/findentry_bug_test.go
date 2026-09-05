// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// TestFindEntryDisambiguatesBySource proves that resolving an unchanged file
// in a referenced backup matches on (SourceIndex, Path), not path alone. Two
// source directories can contain the same relative path; matching on path
// alone returned the first match, so a file from source 1 would restore with
// source 0's data — silent cross-source data corruption.
func TestFindEntryDisambiguatesBySource(t *testing.T) {
	catalog := []manifest.FileEntry{
		{SourceIndex: 0, Path: "config.txt", DataBackupID: "from-source-0"},
		{SourceIndex: 1, Path: "config.txt", DataBackupID: "from-source-1"},
	}

	got, ok := findEntry(catalog, 1, "config.txt")
	if !ok {
		t.Fatal("findEntry did not find source-1 config.txt")
	}
	if got.DataBackupID != "from-source-1" {
		t.Fatalf("findEntry resolved to the wrong source: got %q, want %q", got.DataBackupID, "from-source-1")
	}

	got0, ok := findEntry(catalog, 0, "config.txt")
	if !ok || got0.DataBackupID != "from-source-0" {
		t.Fatalf("findEntry(source 0) resolved wrong: ok=%v id=%q", ok, got0.DataBackupID)
	}
}

// TestFindEntryPathOnlyFallback proves backward compatibility: when no entry
// matches the requested SourceIndex (e.g. a single-source backup written
// before SourceIndex was recorded), the path-only match is used.
func TestFindEntryPathOnlyFallback(t *testing.T) {
	catalog := []manifest.FileEntry{
		{SourceIndex: 0, Path: "only.txt", DataBackupID: "legacy"},
	}
	got, ok := findEntry(catalog, 3, "only.txt")
	if !ok || got.DataBackupID != "legacy" {
		t.Fatalf("path-only fallback failed: ok=%v id=%q", ok, got.DataBackupID)
	}
}
