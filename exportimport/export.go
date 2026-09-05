// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package exportimport

import (
	"archive/zip"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Export packages one or more backups (manifests + chunk data) into a self-contained zip file.
// Chunks are exported as raw frames (compressed + encrypted as-is), so chunk data itself needs
// no key — but for encrypted repos the dedup index is stored encrypted, so the repo's CHUNK key
// must be provided (nil for unencrypted repos) to resolve chunk hashes to pack locations.
// Whether that key is also the index's key is derived here (store.IndexKeyFor), not by the caller.
//
// The export is transitively closed over backup references: a watcher-mode incremental keeps
// unchanged files as FileEntry.DataBackupID pointers into an ancestor backup, and an incremental
// carries a ParentBackupID. Those ancestors' manifests and chunks are pulled in automatically, so
// the resulting archive is self-contained and restorable. Unique chunk hashes across every
// included backup are deduplicated — each appears once in the zip.
func Export(repoPath string, backupIDs []string, outputZip string, key *crypto.MasterKey) error {
	if len(backupIDs) == 0 {
		return fmt.Errorf("at least one backup ID is required")
	}

	// Resolve all requested backups, then transitively pull in every backup
	// they reference — shared with seed-and-ship (#258), which needs the same
	// closure for the same reason: a child without its ancestors cannot
	// restore.
	order, err := CollectBackupSet(repoPath, backupIDs)
	if err != nil {
		return err
	}

	// Collect the union of unique chunk hashes across all included backups,
	// streaming each backup's entries in bounded blocks (never holding more
	// than one block of one backup in memory).
	seen := make(map[[32]byte]struct{})
	for _, id := range order {
		if err := collectBackupHashes(repoPath, id, seen); err != nil {
			return fmt.Errorf("collecting chunk hashes for %q: %w", id, err)
		}
	}

	cfg, err := store.LoadRepoConfig(repoPath)
	if err != nil {
		return fmt.Errorf("loading repo config: %w", err)
	}

	// Open the dedup index to resolve hash → pack location. Per-chunk
	// LookupDirect needs the in-memory hash table, so this uses the full
	// (htab-building) constructor rather than the read-only one. For encrypted
	// repos the index exists only in .enc form; the key is required to decrypt
	// it, otherwise every LookupDirect misses against a fresh empty index.
	// Close re-encrypts and removes the plaintext working copy for encrypted
	// repos.
	//
	// A MANAGED repo's index is plaintext, so the DEK must not reach this open
	// — store.IndexKeyFor is the one place that rule lives (#265). Deriving it
	// here rather than at the call site is what makes the rule impossible for a
	// caller of Export to get wrong.
	indexDir := filepath.Join(repoPath, "index")
	dedup, err := index.NewDedupIndex(indexDir, 0, 0.01, 0, store.IndexKeyFor(cfg, key))
	if err != nil {
		return fmt.Errorf("opening dedup index: %w", err)
	}
	defer dedup.Close()

	// Open chunk store (read-only). Frames are exported raw, so no key.
	chunkStore, err := store.NewChunkStore(repoPath, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		return fmt.Errorf("opening chunk store: %w", err)
	}
	defer chunkStore.Close()

	// Create staging directory.
	stageDir, err := os.MkdirTemp("", "disknexus-export-*")
	if err != nil {
		return fmt.Errorf("creating staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)

	stageManifests := filepath.Join(stageDir, "manifests")
	stageChunks := filepath.Join(stageDir, "chunks")
	for _, d := range []string{stageManifests, stageChunks} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating staging dir %s: %w", d, err)
		}
	}

	// Stage every manifest as .dnm. Backups already in .dnm form are copied
	// verbatim; legacy .manifest(+.entries) backups — still reachable as
	// ancestors of modern incrementals — are loaded fully and re-serialized to
	// .dnm (copying a non-existent DNMPath used to abort the whole export).
	for _, id := range order {
		src := manifest.DNMPath(repoPath, id)
		if _, statErr := os.Stat(src); statErr == nil {
			dst := filepath.Join(stageManifests, id+".dnm")
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("copying manifest for %s: %w", id, err)
			}
			continue
		}
		full, err := manifest.Load(repoPath, id)
		if err != nil {
			return fmt.Errorf("loading legacy manifest %s: %w", id, err)
		}
		// Save writes stageDir/manifests/<id>.dnm — the staging layout matches
		// a repo's manifests/ dir, so stageDir acts as the repo path.
		if err := full.Save(stageDir); err != nil {
			return fmt.Errorf("staging legacy manifest %s as dnm: %w", id, err)
		}
	}

	// Retrieve and stage each unique chunk as a raw frame.
	for hash := range seen {
		entry, found, err := dedup.LookupDirect(hash)
		if err != nil {
			return fmt.Errorf("looking up chunk %x: %w", hash, err)
		}
		if !found {
			return fmt.Errorf("chunk %x referenced in backup but not found in index", hash)
		}

		frame, _, err := chunkStore.RetrieveRaw(entry.PackNumber, int64(entry.StoreOffset))
		if err != nil {
			return fmt.Errorf("retrieving raw chunk %x: %w", hash, err)
		}

		framePath := filepath.Join(stageChunks, hex.EncodeToString(hash[:])+".frame")
		if err := os.WriteFile(framePath, frame, 0644); err != nil {
			return fmt.Errorf("writing chunk frame %x: %w", hash, err)
		}
	}

	// Zip the staging directory to outputZip.
	if err := zipDirectory(stageDir, outputZip); err != nil {
		return fmt.Errorf("creating zip: %w", err)
	}

	return nil
}

// CollectBackupSet resolves the given backup IDs and returns the
// transitively-closed set every one of them references — ParentBackupID
// chains and per-file DataBackupID pointers — in VISIT order: requested
// backups first, ancestors after (reverse it for a parents-first walk).
// Export stages this whole set so archives are self-contained; seed-and-ship
// (#258) ships it whole so a restorable-looking backup in the cloud can
// never be missing its parents.
//
// The traversal uses LoadCatalog (metadata + catalog only) — Load would hold
// every backup's full ENTRIES section in memory at once, several GB for a
// chain of large incrementals.
func CollectBackupSet(repoPath string, backupIDs []string) ([]string, error) {
	visited := make(map[string]bool)
	var order []string
	var queue []string
	for _, id := range backupIDs {
		fullID, err := manifest.ResolveID(repoPath, id)
		if err != nil {
			return nil, fmt.Errorf("resolving backup ID %q: %w", id, err)
		}
		queue = append(queue, fullID)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		order = append(order, id)

		b, err := manifest.LoadCatalog(repoPath, id)
		if err != nil {
			return nil, fmt.Errorf("loading backup %q: %w", id, err)
		}
		if b.ParentBackupID != "" {
			queue = append(queue, b.ParentBackupID)
		}
		for _, fe := range b.FileCatalog {
			if fe.DataBackupID != "" {
				queue = append(queue, fe.DataBackupID)
			}
		}
	}
	return order, nil
}

// collectBackupHashes adds every non-excluded chunk hash of the backup to
// seen. For .dnm backups the entries are streamed in bounded blocks; legacy
// backups fall back to a full Load (one legacy backup's entries at a time).
func collectBackupHashes(repoPath, backupID string, seen map[[32]byte]struct{}) error {
	dnmPath := manifest.DNMPath(repoPath, backupID)
	if _, err := os.Stat(dnmPath); err == nil {
		r, err := manifest.OpenDNMReader(dnmPath)
		if err != nil {
			return err
		}
		defer r.Close()

		const block = 65536
		total := uint64(r.EntriesCount())
		for start := uint64(0); start < total; start += block {
			end := start + block
			if end > total {
				end = total
			}
			entries, err := r.EntriesRange(start, end)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !e.IsExcluded {
					seen[e.ChunkHash] = struct{}{}
				}
			}
		}
		return nil
	}

	// Legacy .manifest(+.entries): load fully (bounded to one backup at a time).
	b, err := manifest.Load(repoPath, backupID)
	if err != nil {
		return err
	}
	for _, e := range b.Entries {
		if !e.IsExcluded {
			seen[e.ChunkHash] = struct{}{}
		}
	}
	return nil
}

// copyFile atomically copies the file at src to dst: it writes to a sibling
// temp file, fsyncs, and renames into place. A crash mid-copy therefore never
// leaves a truncated dst — important for manifests, where a partial .dnm hard-
// blocks prune (which errors on any unreadable manifest).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// zipDirectory creates a zip archive of the directory tree rooted at srcDir.
// Entry paths in the zip are relative to srcDir and use forward slashes.
//
// Close/Sync errors are surfaced: zip.Writer.Close writes the central directory,
// so discarding its error (or the underlying file's) would let Export return nil
// with a truncated, unreadable archive on ENOSPC or an I/O fault.
func zipDirectory(srcDir, destZip string) (err error) {
	f, err := os.Create(destZip)
	if err != nil {
		return err
	}
	defer func() {
		// Sync then Close the file; report either error only if the archive
		// otherwise looked good, so the first real failure is preserved.
		if syncErr := f.Sync(); syncErr != nil && err == nil {
			err = fmt.Errorf("syncing zip: %w", syncErr)
		}
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("closing zip file: %w", closeErr)
		}
	}()

	if err = zipDirectoryTo(f, srcDir); err != nil {
		return err
	}
	return nil
}

// zipDirectoryTo writes a zip archive of srcDir to w. It is the testable core of
// zipDirectory: passing a failing writer exercises the central-directory flush
// error path that the wrapper must not swallow.
func zipDirectoryTo(w io.Writer, srcDir string) (err error) {
	zw := zip.NewWriter(w)
	defer func() {
		// zip.Writer.Close flushes the central directory; its error is fatal to
		// archive readability and must be reported.
		if closeErr := zw.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("finalizing zip (central directory): %w", closeErr)
		}
	}()

	return filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(srcDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)

		zf, createErr := zw.Create(rel)
		if createErr != nil {
			return createErr
		}

		in, openErr := os.Open(path)
		if openErr != nil {
			return openErr
		}
		defer in.Close()

		_, copyErr := io.Copy(zf, in)
		return copyErr
	})
}
