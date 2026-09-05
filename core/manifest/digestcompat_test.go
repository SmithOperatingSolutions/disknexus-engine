// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"testing"
	"time"
)

// #455 appended two fields to the DNM METADATA tail. Old readers stop after
// WrappedDEK and ignore trailing bytes, so new files load on old builds;
// this pins the other direction — an OLD manifest, which simply ends where
// the digest fields would begin, loads on a NEW build with an empty digest
// and no error. Recovery media carry old builds and fleets carry old
// manifests; either direction failing is a restore that refuses to start.
func TestAPreDigestMetadataDecodesWithAnEmptyDigest(t *testing.T) {
	b := &Backup{
		BackupID: "old-one", SourceVolume: "vol", Timestamp: time.Now(),
		TotalBytes: 42, ContentDigest: "deadbeef", ContentDigestCovers: DigestCoversSourceStreamV1,
	}
	full := encodeMetadata(b)
	// The pre-#455 encoding is the same bytes minus the two appended str8s:
	// 1 length byte + the digest, 1 length byte + the covers — and minus the
	// two #468 list count16s (2 bytes each, empty lists) appended after them.
	old := full[:len(full)-(1+len(b.ContentDigest))-(1+len(b.ContentDigestCovers))-4]

	got, err := decodeMetadata(old)
	if err != nil {
		t.Fatalf("a pre-digest manifest fails to decode: %v — every backup on the fleet made before this "+
			"build becomes unloadable, which is not compatibility, it is data loss with a version number", err)
	}
	if got.ContentDigest != "" || got.ContentDigestCovers != "" {
		t.Fatalf("a pre-digest manifest decoded with digest %q covers %q — absence must stay absence, or "+
			"verification compares against an invented value", got.ContentDigest, got.ContentDigestCovers)
	}
	if got.BackupID != "old-one" || got.TotalBytes != 42 {
		t.Fatalf("the truncated fixture lost fields BEFORE the digest (%+v) — it is not modeling an old "+
			"manifest, and the assertions above prove nothing", got)
	}

	// Round-trip control (§4): the full encoding carries both fields back.
	rt, err := decodeMetadata(full)
	if err != nil {
		t.Fatal(err)
	}
	if rt.ContentDigest != "deadbeef" || rt.ContentDigestCovers != DigestCoversSourceStreamV1 {
		t.Fatalf("round-trip lost the digest: %q/%q", rt.ContentDigest, rt.ContentDigestCovers)
	}
}
