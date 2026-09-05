// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// VerifyWrittenDigest holds a restored target against the manifest's
// whole-stream digest (#455 slice 3): a sequential fold over exactly
// b.TotalBytes of r, compared to b.ContentDigest.
//
// It reads the target back rather than folding at write time because the
// restorer writes PACK-MAJOR (#83) — every pack fetched exactly once, at
// the cost of stream order — so the only place the stream exists again is
// on the medium it was written to. That is also what makes this the
// STRONGEST check in the product: it judges the bytes on disk, after every
// buffer, cache and controller between the repo and the medium has had its
// say. Each chunk was verified as it was written; only this sees that what
// LANDED, as a whole, is what was captured.
//
// Verdicts are the verify vocabulary: DigestMatch, DigestMismatch (a short
// read counts — truncation is the likeliest real failure, and "the image
// ended early" IS a mismatch with the captured stream, reported as one),
// DigestNotVerifiable for pre-digest backups. The error return is for the
// reader failing, not for the bytes disagreeing.
func VerifyWrittenDigest(b *manifest.Backup, r io.Reader) (string, error) {
	if b.ContentDigest == "" || b.ContentDigestCovers != manifest.DigestCoversSourceStreamV1 {
		return DigestNotVerifiable, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(r, b.TotalBytes)); err != nil {
		return "", fmt.Errorf("reading back the restored target: %w", err)
	}
	// A truncated target needs no separate branch: the fold of a short
	// stream cannot equal the digest of the full one, so truncation IS a
	// mismatch — which is the right report, since "the image ends early"
	// is a disagreement with the captured stream, not an IO excuse.
	if hex.EncodeToString(h.Sum(nil)) != b.ContentDigest {
		return DigestMismatch, nil
	}
	return DigestMatch, nil
}
