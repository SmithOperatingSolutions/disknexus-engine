// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// #455 slice 3: the restored bytes are held against the manifest's digest
// before the restore is called complete. The restorer writes PACK-MAJOR
// (#83) — correct and deliberate — which means no write-time fold can see
// the stream; the check is a sequential read-back of the target. A restore
// that dropped, duplicated or misplaced a region currently completes green;
// this is the door that closes.

func TestVerifyWrittenDigestJudgesTheBytesOnDisk(t *testing.T) {
	data := bytes.Repeat([]byte{0x5A, 0x3C}, 8<<10)
	sum := sha256.Sum256(data)
	b := &manifest.Backup{
		BackupID:            "wd",
		TotalBytes:          int64(len(data)),
		ContentDigest:       hex.EncodeToString(sum[:]),
		ContentDigestCovers: manifest.DigestCoversSourceStreamV1,
	}

	// Healthy (§4): the written image matches.
	verdict, err := VerifyWrittenDigest(b, bytes.NewReader(data))
	if err != nil || verdict != DigestMatch {
		t.Fatalf("intact read-back: verdict %q err %v, want %q", verdict, err, DigestMatch)
	}

	// One byte wrong, deep in the image: every chunk of this restore was
	// individually verified as it was written — only the read-back can see
	// that what LANDED is not what was captured.
	bad := append([]byte{}, data...)
	bad[9000] ^= 0xFF
	verdict, err = VerifyWrittenDigest(b, bytes.NewReader(bad))
	if err != nil || verdict != DigestMismatch {
		t.Fatalf("a corrupted target read back as verdict %q err %v.\n"+
			"The restore completes green holding bytes that are not the backup — the operator's machine "+
			"boots (or does not) on data nothing ever checked as a whole.", verdict, err)
	}

	// Short image: fewer bytes than the manifest claims is a mismatch, not
	// an io error to shrug at — truncation is the likeliest real failure.
	verdict, err = VerifyWrittenDigest(b, bytes.NewReader(data[:len(data)-512]))
	if err != nil || verdict != DigestMismatch {
		t.Fatalf("a truncated target read back as verdict %q err %v, want %q", verdict, err, DigestMismatch)
	}

	// Pre-digest backup: not verifiable, distinct from both outcomes.
	b2 := &manifest.Backup{BackupID: "old", TotalBytes: int64(len(data))}
	verdict, err = VerifyWrittenDigest(b2, bytes.NewReader(data))
	if err != nil || verdict != DigestNotVerifiable {
		t.Fatalf("pre-digest backup: verdict %q err %v, want %q", verdict, err, DigestNotVerifiable)
	}
}
