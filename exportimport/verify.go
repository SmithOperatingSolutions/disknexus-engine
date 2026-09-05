// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/klauspost/compress/zstd"
)

// zstdMagic is the little-endian zstd frame magic (0xFD2FB528). A frame
// payload that starts with it is compressed-but-unencrypted: AES-GCM
// ciphertext of the same payload starts with a random 12-byte nonce.
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

// verifyArchiveFrames proves that every staged frame belongs in this
// repository, and is the whole of Import's safety.
//
// Import writes frames VERBATIM (store.StoreRaw never compresses and never
// encrypts), so it is the one write path that cannot make an archive fit the
// repo it is landing in — it can only refuse. Before this check it refused
// nothing: a plaintext chunk went into a managed-encryption repo, the
// manifests were installed, and Import returned success. Nothing downstream
// could notice, because the on-disk frame format carries no encryption marker
// and the export zip carries no description of the repo it came from.
//
// The check is a PROOF rather than a comparison of metadata, which is why it
// does not need the archive to describe its source (and so keeps working on
// archives written by earlier versions): a frame belongs here if and only if
// this repo's own read path can turn it back into the chunk the archive filed
// it under. That is exactly
//
//	sha256(normalize(decompress(decrypt(payload)))) == the hash in the filename
//
// with this repo's key and this repo's normalizer. It therefore catches, in
// one pass and with no false refusals:
//
//   - plaintext frames arriving at an encrypted repo (the #265 invariant, in
//     the path pipeline.Bind cannot reach);
//   - ciphertext frames arriving at an unencrypted repo;
//   - frames encrypted under a DIFFERENT key — agreeing on the mode is not
//     enough, since every managed repo has its own DEK;
//   - chunks whose identity was computed under a different normalizer, which
//     would file them under hashes this repo's restore can never reproduce.
//
// A normalizer that is the identity on this particular content is NOT
// refused, and should not be: those chunks restore correctly and dedup
// correctly. The property is that the data works here, not that two config
// blobs match.
//
// Cost: it decrypts and decompresses every chunk in the archive. Import
// already reads and writes every one of them, and correctness of a permanent,
// unrepairable write is worth a decompression pass.
func verifyArchiveFrames(stageChunks string, rc store.RepoConfig, key *crypto.MasterKey) error {
	entries, err := os.ReadDir(stageChunks)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // manifest-only archive; nothing to verify
		}
		return fmt.Errorf("reading staged chunks: %w", err)
	}

	norm, err := preprocess.FromNames(rc.Normalizers)
	if err != nil {
		return fmt.Errorf("reconstructing normalizer from repo config: %w", err)
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer decoder.Close()

	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".frame") {
			continue
		}
		hexName := strings.TrimSuffix(de.Name(), ".frame")
		hashBytes, err := hex.DecodeString(hexName)
		if err != nil || len(hashBytes) != 32 {
			return fmt.Errorf("invalid chunk filename %q", de.Name())
		}
		var want [32]byte
		copy(want[:], hashBytes)

		frame, err := os.ReadFile(filepath.Join(stageChunks, de.Name()))
		if err != nil {
			return fmt.Errorf("reading chunk frame %s: %w", hexName, err)
		}
		if _, err := VerifyFrame(frame, want, rc, key, norm, decoder); err != nil {
			return err
		}
	}
	return nil
}

// VerifyFrame is the per-chunk half of the #280 proof, shared with
// seed-and-ship (#258): it proves ONE raw frame belongs in the repository rc
// describes — this repo's read path (its key, its normalizer) turns the
// payload back into the chunk it is filed under:
//
//	sha256(normalize(decompress(decrypt(payload)))) == want
//
// On success it returns the chunk's full identity (weak + strong hash of the
// normalized bytes), which is exactly what a dedup-index insert needs. The
// refusal messages are the import family's, verbatim — a ship IS an import.
//
// norm must be the normalizer reconstructed from rc (preprocess.FromNames);
// decoder is a caller-held zstd reader so per-chunk calls stay cheap.
func VerifyFrame(frame []byte, want [32]byte, rc store.RepoConfig, key *crypto.MasterKey, norm preprocess.Normalizer, decoder *zstd.Decoder) (hasher.ChunkID, error) {
	var id hasher.ChunkID
	hexName := hex.EncodeToString(want[:])
	mode := rc.EffectiveEncryptionMode()

	if len(frame) < 8 {
		return id, fmt.Errorf("chunk %s: frame is %d bytes, too short to be a chunk frame", hexName, len(frame))
	}
	payloadLen := binary.LittleEndian.Uint32(frame[0:4])
	if int(payloadLen)+8 > len(frame) {
		return id, fmt.Errorf("chunk %s: frame declares a %d-byte payload but is %d bytes", hexName, payloadLen, len(frame))
	}
	payload := frame[8 : 8+payloadLen]

	compressed := payload
	if key != nil {
		plain, derr := key.DecryptWithAAD(payload, crypto.AADChunk)
		if derr != nil {
			if bytes.HasPrefix(payload, zstdMagic) {
				return id, fmt.Errorf("refusing to import: this repository is %s-encrypted and the archive's chunks are "+
					"PLAINTEXT (chunk %s is a bare zstd frame). Import copies frames verbatim — it cannot encrypt them — "+
					"so they would sit readable in an encrypted repo and every restore of them would fail authentication. "+
					"Export the backups again from a repo encrypted with this repository's key", mode, hexName[:16])
			}
			return id, fmt.Errorf("refusing to import: chunk %s does not authenticate under this repository's key — "+
				"the archive was encrypted with a DIFFERENT key (every %s repo has its own). Nothing this repository "+
				"can do makes those frames readable here: %w", hexName[:16], mode, derr)
		}
		compressed = plain
	}

	data, derr := decoder.DecodeAll(compressed, nil)
	if derr != nil {
		if key == nil {
			return id, fmt.Errorf("refusing to import: this repository is unencrypted and chunk %s does not decompress — "+
				"it is almost certainly ENCRYPTED, and importing it would store bytes no reader of this repo can decode. "+
				"Import into a repo holding the source repo's key instead: %w", hexName[:16], derr)
		}
		return id, fmt.Errorf("refusing to import: chunk %s decrypted but does not decompress — the archive is corrupt: %w",
			hexName[:16], derr)
	}

	id = hasher.Sum(preprocess.IdentityHashInput(norm, data))
	if !bytes.Equal(id.StrongHash[:], want[:]) {
		return hasher.ChunkID{}, fmt.Errorf("refusing to import: chunk %s does not hash to the identity the archive filed it under "+
			"(this repository computes %x). Chunk identity is the hash of NORMALIZED bytes, and this repository "+
			"normalizes with %v — an archive built under a different normalizer files its chunks under hashes no "+
			"restore here could reproduce", hexName[:16], id.StrongHash[:8], normalizerNames(rc))
	}
	return id, nil
}

func normalizerNames(rc store.RepoConfig) []string {
	if len(rc.Normalizers) == 0 {
		return []string{"none"}
	}
	return rc.Normalizers
}
