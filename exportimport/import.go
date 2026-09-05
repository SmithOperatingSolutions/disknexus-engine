// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport

import (
	"archive/zip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Import reads a zip file produced by Export and injects its backups into repoPath.
// key is the repo's CHUNK key (nil for an unencrypted repo); the dedup index's own
// key is derived from it by store.IndexKeyFor, so a managed repo's index stays
// plaintext without the caller having to know that rule. After importing chunks, the
// dedup index is fully rebuilt so future backups dedup correctly against all imported
// data.
//
// Every staged frame is verified against this repository BEFORE the chunk store is
// opened — see verifyArchiveFrames. An archive that does not belong here is refused
// with nothing written.
//
// For managed-encryption repos, the index rebuild is skipped and a warning is printed;
// run "disknexus index --rebuild-all" manually after the controller is contacted.
func Import(ctx context.Context, repoPath string, inputZip string, key *crypto.MasterKey) error {
	if !store.RepoExists(repoPath) {
		return fmt.Errorf("repository not found at %s", repoPath)
	}

	cfg, err := store.LoadRepoConfig(repoPath)
	if err != nil {
		return fmt.Errorf("loading repo config: %w", err)
	}
	// Import appends chunks to this repo, so it is a writer and must read the
	// stored config the way the backup path does (#259). Applying an unset
	// pack_file_max_size literally bounds packs at 0 bytes, sealing one per
	// chunk.
	cfg = cfg.Effective()

	// Extract zip to a staging directory.
	stageDir, err := os.MkdirTemp("", "disknexus-import-*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	if err := unzip(inputZip, stageDir); err != nil {
		return fmt.Errorf("extracting zip: %w", err)
	}

	stageChunks := filepath.Join(stageDir, "chunks")
	stageManifests := filepath.Join(stageDir, "manifests")

	// The repo config, not the caller, decides what key this import needs.
	// verifyArchiveFrames below checks the frames against the key it is GIVEN,
	// so a caller that hands a nil key for an encrypted repo would make
	// plaintext frames verify happily and land — Bind is the same refusal
	// every other write path in the module goes through (#265), and it also
	// yields the index key and the normalizer.
	binding, err := pipeline.Bind(cfg, key)
	if err != nil {
		return fmt.Errorf("importing into this repository: %w", err)
	}

	// Refuse an archive that does not belong in this repository, before a
	// single byte of it is appended to a pack file.
	if err := verifyArchiveFrames(stageChunks, cfg, binding.Key()); err != nil {
		return err
	}

	// Open chunk store for appending.
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel, binding.Key())
	if err != nil {
		return fmt.Errorf("opening chunk store: %w", err)
	}

	// Open dedup index for chunk-presence checks. The index key is not the
	// chunk key: a managed repo's index is plaintext on purpose, and handing
	// it the DEK re-encrypts bloom.bin/hash-index.db on Close and deletes the
	// plaintext, leaving every other command staring at an empty index (#265).
	indexDir := filepath.Join(repoPath, "index")
	dedup, err := index.NewDedupIndex(indexDir, 0, 0.01, 0, binding.IndexKey())
	if err != nil {
		_ = chunkStore.Close()
		return fmt.Errorf("opening dedup index: %w", err)
	}

	// Import chunks, skipping any already present in the index.
	chunkEntries, readErr := os.ReadDir(stageChunks)
	if readErr != nil && !os.IsNotExist(readErr) {
		_ = dedup.Close()
		_ = chunkStore.Close()
		return fmt.Errorf("reading staged chunks: %w", readErr)
	}

	for _, de := range chunkEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".frame") {
			continue
		}

		hexName := strings.TrimSuffix(de.Name(), ".frame")
		hashBytes, err := hex.DecodeString(hexName)
		if err != nil || len(hashBytes) != 32 {
			_ = dedup.Close()
			_ = chunkStore.Close()
			return fmt.Errorf("invalid chunk filename %q", de.Name())
		}

		var strongHash [32]byte
		copy(strongHash[:], hashBytes)

		_, found, err := dedup.LookupDirect(strongHash)
		if err != nil {
			_ = dedup.Close()
			_ = chunkStore.Close()
			return fmt.Errorf("looking up chunk %s: %w", hexName, err)
		}
		if found {
			continue // already present — skip
		}

		frame, err := os.ReadFile(filepath.Join(stageChunks, de.Name()))
		if err != nil {
			_ = dedup.Close()
			_ = chunkStore.Close()
			return fmt.Errorf("reading chunk frame %s: %w", hexName, err)
		}

		if _, _, _, err := chunkStore.StoreRaw(frame); err != nil {
			_ = dedup.Close()
			_ = chunkStore.Close()
			return fmt.Errorf("storing chunk %s: %w", hexName, err)
		}
	}

	// Close dedup index (we're about to rebuild it from scratch anyway).
	_ = dedup.Close()

	// Flush and close the chunk store before rebuilding the index.
	if err := chunkStore.Close(); err != nil {
		return fmt.Errorf("closing chunk store: %w", err)
	}

	repoManifests := filepath.Join(repoPath, "manifests")
	if err := os.MkdirAll(repoManifests, 0755); err != nil {
		return fmt.Errorf("creating manifests directory: %w", err)
	}

	// Managed-encryption repos can't rebuild the index here (the DEK requires a
	// controller round-trip). Install the manifests and warn; the imported
	// chunks stay unresolvable until a manual 'index --rebuild-all'.
	if cfg.EffectiveEncryptionMode() == store.EncryptManaged {
		if err := installManifests(stageManifests, repoManifests); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "warning: managed-encryption repo; skipping index rebuild.")
		fmt.Fprintln(os.Stderr, "Run 'disknexus index --rebuild-all' after the controller is contacted.")
		return nil
	}

	// Rebuild the full dedup index from all pack files (imported + pre-existing)
	// BEFORE installing the manifests. Manifests are what make a backup visible
	// to list/restore; installing them only after the index is rebuilt means a
	// crash between the two can never leave a listable backup whose chunks are
	// unresolvable (it would just leave chunks with no manifest — harmless and
	// re-importable).
	if _, err := index.Rebuild(ctx, index.RebuildOptions{
		RepoPath:         repoPath,
		Key:              binding.Key(),
		RebuildBloom:     true,
		RebuildHashIndex: true,
	}); err != nil {
		return fmt.Errorf("rebuilding index: %w", err)
	}

	if err := installManifests(stageManifests, repoManifests); err != nil {
		return err
	}

	return nil
}

// installManifests atomically copies every .dnm from stageManifests into
// repoManifests (copyFile uses temp+rename), so a crash never leaves a
// truncated manifest that would block prune.
func installManifests(stageManifests, repoManifests string) error {
	manifestEntries, readErr := os.ReadDir(stageManifests)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("reading staged manifests: %w", readErr)
	}
	for _, de := range manifestEntries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".dnm") {
			continue
		}
		src := filepath.Join(stageManifests, de.Name())
		dst := filepath.Join(repoManifests, de.Name())
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copying manifest %s: %w", de.Name(), err)
		}
	}
	return nil
}

// unzip extracts a zip archive to destDir, rejecting entries with path traversal.
func unzip(src, destDir string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		rel := filepath.FromSlash(f.Name)
		if strings.Contains(rel, "..") {
			return fmt.Errorf("zip entry with unsafe path: %s", f.Name)
		}

		destPath := filepath.Join(destDir, rel)

		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.Create(destPath)
		if err != nil {
			rc.Close()
			return err
		}

		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}

		rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
	}

	return nil
}
