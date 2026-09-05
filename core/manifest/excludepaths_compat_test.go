// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"testing"
	"time"
)

// #468 appended the operator exclusion list to the DNM METADATA tail, after
// the #455 digest pair, by the same rule: an old reader stops after what it
// knows and ignores the rest; a new reader meets EOF where the list would
// begin and reads "no exclusions". This pins both directions and the
// round-trip.
func TestAPreExclusionsMetadataDecodesWithNoExclusions(t *testing.T) {
	b := &Backup{
		BackupID: "old-two", SourceVolume: "vol", Timestamp: time.Now(),
		TotalBytes: 7, ContentDigest: "cafef00d", ContentDigestCovers: DigestCoversSourceStreamV1,
		ExcludePaths:    []string{`C:\Users\x\VMs`, `C:\scratch`},
		ExcludeWarnings: []string{`WARNING: exclusion C:\scratch not found on the volume — its data is IN this backup`},
	}
	full := encodeMetadata(b)

	// Round-trip control (§4) first: the full encoding carries both lists
	// back, in order, verbatim.
	rt, err := decodeMetadata(full)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.ExcludePaths) != 2 || rt.ExcludePaths[0] != `C:\Users\x\VMs` || rt.ExcludePaths[1] != `C:\scratch` {
		t.Fatalf("round-trip lost the exclusions: %q — a restore could not name the exclusion that zeroed a file", rt.ExcludePaths)
	}
	if len(rt.ExcludeWarnings) != 1 || rt.ExcludeWarnings[0] != b.ExcludeWarnings[0] {
		t.Fatalf("round-trip lost the warnings: %q — 'its data is in this backup' is recorded nowhere", rt.ExcludeWarnings)
	}
	if rt.ContentDigest != "cafef00d" {
		t.Fatalf("round-trip lost the digest that precedes the list: %q", rt.ContentDigest)
	}

	// A manifest written before #468 ends after the digest covers. Cut both
	// lists off: count16 + (len16 + bytes) per entry, each.
	listLen := 2 + 2
	for _, p := range append(append([]string{}, b.ExcludePaths...), b.ExcludeWarnings...) {
		listLen += 2 + len(p)
	}
	old := full[:len(full)-listLen]
	got, err := decodeMetadata(old)
	if err != nil {
		t.Fatalf("a pre-#468 manifest fails to decode: %v — every backup made before this build becomes "+
			"unloadable on it", err)
	}
	if len(got.ExcludePaths) != 0 || len(got.ExcludeWarnings) != 0 {
		t.Fatalf("a pre-#468 manifest decoded with exclusions %q warnings %q — absence must read as none configured", got.ExcludePaths, got.ExcludeWarnings)
	}
	if got.ContentDigest != "cafef00d" || got.ContentDigestCovers != DigestCoversSourceStreamV1 || got.BackupID != "old-two" {
		t.Fatalf("the truncated fixture lost fields BEFORE the list (%+v) — it is not modeling an old manifest", got)
	}
}
