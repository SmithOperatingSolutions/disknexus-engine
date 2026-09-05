// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// #465 slice 4, the file-mode decision made deliberate: the catalog's
// per-file ContentHash is a hash OF THE COVERING CHUNK HASHES, not of the
// file's bytes — so byte-level per-file verification is not derivable from
// the existing format, and file mode's whole-backup answer stays the stream
// digest (slices 1–3 cover file-mode backups already; the pipeline folds
// the concatenated stream the same way). What IS derivable — and was
// unchecked — is chunk SELECTION: an extract that maps a file to the wrong
// entries retrieves perfectly valid chunks that are not this file, writes
// them out, and reports success. ExtractFile now recomputes the covering
// derivation over the entries it actually used and holds it against the
// catalog.

// extractWorld builds one repo+catalog; hashFor derives the catalog's
// ContentHash from the fixture's real chunk hash (nil = the true covering
// hash; a literal = the tamper).
func extractWorld(t *testing.T, hashFor func(chunkHash [32]byte) [32]byte) (*FileRestorer, *manifest.Backup, string) {
	t.Helper()
	repoPath, dedupIdx, chunkStore, _, chunkHash := setupTestRepo(t)
	t.Cleanup(func() { dedupIdx.Close(); chunkStore.Close() })
	contentHash := hashFor(chunkHash)
	backup := &manifest.Backup{
		BackupID:   "extract-verify",
		BackupMode: "file",
		FileCatalog: []manifest.FileEntry{
			{Path: "doc.txt", Size: 4096, Mode: 0644, StreamOffset: 0, StreamLength: 4096,
				ContentHash: contentHash},
		},
		Entries: []manifest.Entry{{VolumeOffset: 0, ChunkHash: chunkHash, ChunkLength: 4096}},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewFileRestorer(dedupIdx, chunkStore, repoPath, logger), backup,
		filepath.Join(t.TempDir(), "out.txt")
}

func coveringHash(chunkHashes ...[32]byte) [32]byte {
	h := sha256.New()
	for _, ch := range chunkHashes {
		h.Write(ch[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestExtractHoldsTheFileAgainstItsCatalogHash(t *testing.T) {
	// Positive control (§4): the true covering hash extracts clean.
	r, b, out := extractWorld(t, func(ch [32]byte) [32]byte { return coveringHash(ch) })
	if _, err := r.ExtractFile(context.Background(), b, "doc.txt", out); err != nil {
		t.Fatalf("extract with the true catalog hash failed: %v", err)
	}

	// The catalog claims different content than the entries deliver — the
	// shape a wrong extent/entry mapping produces (findEntry has had exactly
	// that bug class). Every retrieved chunk matches its own hash; only the
	// covering derivation can see the file is not what the catalog says.
	r2, b2, out2 := extractWorld(t, func([32]byte) [32]byte { return [32]byte{0xAA, 0xBB} })
	_, err := r2.ExtractFile(context.Background(), b2, "doc.txt", out2)
	if err == nil {
		t.Fatalf("an extract whose covering hashes disagree with the catalog succeeded.\n" +
			"The operator asked for doc.txt and got a byte-perfect assembly of the WRONG chunks — " +
			"the file-mode analog of #376, reported as success at the moment someone is recovering a file.")
	}
	if !strings.Contains(err.Error(), "doc.txt") {
		t.Errorf("the failure does not name the file: %v", err)
	}
	if _, serr := os.Stat(out2); serr == nil {
		t.Errorf("the mismatching extract left its output file behind — a file that failed verification " +
			"must not sit where the operator asked for the real one")
	}

	// Legacy catalogs (zero ContentHash — pre-#353 agent rows) extract as
	// before: not verifiable is not a failure (§9).
	r3, b3, out3 := extractWorld(t, func([32]byte) [32]byte { return [32]byte{} })
	if _, err := r3.ExtractFile(context.Background(), b3, "doc.txt", out3); err != nil {
		t.Fatalf("a legacy zero-hash catalog entry was refused: %v — pre-#353 backups become "+
			"unextractable, which is not verification, it is data loss with a checkmark", err)
	}
}
