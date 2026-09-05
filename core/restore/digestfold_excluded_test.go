// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// The stream fold's excluded-entry contribution (#455): exclusion zeroes
// blocks BEFORE the chunker (#94), so the captured stream held zeros there
// and the reconstruction must fold zeros of exactly ChunkLength — nothing,
// or the wrong count, silently shifts every later byte of the fold and a
// healthy backup fails its digest.
func TestTheFoldCountsExcludedEntriesAsTheZerosTheCaptureSaw(t *testing.T) {
	repoPath, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	_ = repoPath
	defer dedupIdx.Close()
	defer chunkStore.Close()

	const excludedLen = 3000
	// The digest a capture of [chunk][zeros] would have folded.
	h := sha256.New()
	h.Write(chunkData)
	h.Write(make([]byte, excludedLen))
	b := &manifest.Backup{
		BackupID:            "fold-excluded",
		ContentDigest:       hex.EncodeToString(h.Sum(nil)),
		ContentDigestCovers: manifest.DigestCoversSourceStreamV1,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
			{VolumeOffset: int64(len(chunkData)), ChunkLength: excludedLen, IsExcluded: true},
		},
	}
	res, err := Verify(context.Background(), b, dedupIdx, chunkStore)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("chunk verification failed: %v — the verdict below is not about the fold", res.Errors)
	}
	if res.DigestVerdict != DigestMatch {
		t.Fatalf("verdict %q (expected %s, actual %s) — a backup whose only unusual feature is an "+
			"excluded region fails its digest, so every Windows machine with a pagefile reads as "+
			"corrupt and the verdict trains operators to ignore mismatches", res.DigestVerdict,
			res.DigestExpected, res.DigestActual)
	}
}

// A verify that could not reconstruct the stream makes NO digest claim.
// With chunk errors present the fold is a fold of a partial stream: calling
// it a mismatch double-reports the same damage in a scarier word, and any
// other verdict is a claim about bytes that were never read. The chunk
// errors are the report.
func TestAChunkErrorSilencesTheDigestVerdict(t *testing.T) {
	_, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	b := &manifest.Backup{
		BackupID:            "fold-aborted",
		ContentDigest:       "0000000000000000000000000000000000000000000000000000000000000000",
		ContentDigestCovers: manifest.DigestCoversSourceStreamV1,
		Entries: []manifest.Entry{
			{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: len(chunkData)},
			// A hash the repository has never seen: this chunk cannot verify.
			{VolumeOffset: int64(len(chunkData)), ChunkHash: [32]byte{0xEE, 0xBB}, ChunkLength: 512},
		},
	}
	res, err := Verify(context.Background(), b, dedupIdx, chunkStore)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) == 0 {
		t.Fatal("the unknown-hash entry verified; the fixture is broken and the verdict below proves nothing")
	}
	if res.DigestVerdict != "" {
		t.Fatalf("a verify with %d chunk errors still issued digest verdict %q — a fold over a partial "+
			"reconstruction supports no claim in either direction", len(res.Errors), res.DigestVerdict)
	}
}
