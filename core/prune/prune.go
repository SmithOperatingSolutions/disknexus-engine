// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package prune

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/cespare/xxhash/v2"
	"github.com/klauspost/compress/zstd"
)

// Options configures a prune operation.
type Options struct {
	RepoPath   string
	DryRun     bool
	Key        *crypto.MasterKey // nil for unencrypted repos
	OnProgress func(ProgressInfo)

	// ExtraReferencedHashes are chunk hashes to treat as referenced in addition
	// to those reachable from manifests. The CLI populates this with a suspended
	// backup's checkpoint-prefix hashes (#56) so pruning while a backup is
	// suspended preserves the data that backup's resume depends on, rather than
	// refusing to prune at all.
	ExtraReferencedHashes map[[32]byte]bool
}

// ProgressInfo holds data passed to the progress callback during prune.
type ProgressInfo struct {
	Phase        string
	ChunksCopied int64
	ChunksTotal  int64
	BytesCopied  int64
	BytesTotal   int64
	Elapsed      time.Duration
}

// Result contains the outcome of a prune operation.
type Result struct {
	TotalChunks      int64
	ReferencedChunks int64
	OrphanedChunks   int64
	DuplicateChunks  int64 // referenced entries sharing a StrongHash with another
	BytesBefore      int64
	BytesAfter       int64
	BytesReclaimed   int64
	PackFilesBefore  int
	PackFilesAfter   int
	BakFilesRemoved  int
	Duration         time.Duration
}

// Run executes the prune operation: identifies orphaned chunks, rewrites
// referenced chunks into new pack files, and atomically swaps them in.
func Run(ctx context.Context, opts Options) (*Result, error) {
	start := time.Now()

	// Crash recovery: clean up any leftover state from a prior interrupted
	// prune. This performs renames/RemoveAll, so it must NOT run under DryRun —
	// a dry run must never mutate the repo. If recovery is pending, refuse the
	// dry run rather than analyze an inconsistent mid-swap state.
	if opts.DryRun {
		if pruneRecoveryPending(opts.RepoPath) {
			return nil, fmt.Errorf("a prior prune was interrupted; run prune without --dry-run to recover before a dry run can report accurately")
		}
	} else {
		if err := recoverIfNeeded(opts.RepoPath); err != nil {
			return nil, fmt.Errorf("crash recovery: %w", err)
		}
	}

	repoCfg, err := store.LoadRepoConfig(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("loading repo config: %w", err)
	}
	// Prune rewrites surviving chunks, so it is a writer and must read the
	// stored config exactly the way the backup path does (#259). Applying an
	// unset pack_file_max_size literally handed the staging store a bound of
	// 0, which seals a new pack on every chunk.
	repoCfg = repoCfg.Effective()

	// Step 1: Collect referenced hashes from all manifests, plus any extra
	// hashes the caller wants protected (a suspended backup's checkpoint prefix).
	referenced, err := collectReferencedHashes(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("collecting referenced hashes: %w", err)
	}
	for h := range opts.ExtraReferencedHashes {
		referenced[h] = true
	}

	// Step 2: Read all index entries.
	//
	// opts.Key is the CHUNK key. The index has its own (store.IndexKeyFor):
	// under managed encryption it is nil, because the index is deliberately
	// plaintext so the controller's server-side restore can open it without
	// the operator. Passing the DEK here does not merely encrypt the index —
	// index.NewDedupIndex treats an existing plaintext bloom.bin as a
	// decrypted working copy and DELETES it, and CloseDiscard removes
	// hash-index.db too, writing no .enc replacement. prune then reports
	// success on a repo whose every backup has become unrestorable, and
	// `index --rebuild-all` refuses managed repos (#265).
	cfg := config.Default()
	dedupIdx, err := index.NewDedupIndexReadOnly(filepath.Join(opts.RepoPath, "index"), index.ReadOpenExpectedChunks, cfg.BloomFPRate, cfg.IndexCacheMB, store.IndexKeyFor(repoCfg, opts.Key))
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}

	allEntries, err := dedupIdx.ReadAllEntries()
	// This open is a pure read scan: nothing was inserted, so close WITHOUT
	// flushing. Close() would unconditionally persist the in-memory bloom —
	// including the fresh EMPTY one created when bloom.bin is missing — which
	// would launder the missing-bloom corruption into an innocent-looking empty
	// bloom that passes the backup-path guard and silently re-stores the whole
	// repo as duplicates on the next backup. It also violated the dry-run
	// read-only invariant (this ran before the DryRun early-return).
	dedupIdx.CloseDiscard()
	if err != nil {
		return nil, fmt.Errorf("reading index entries: %w", err)
	}

	// Step 3: Classify entries into referenced and orphaned.
	var referencedEntries []index.IndexEntry
	var orphanCount int64
	for _, e := range allEntries {
		if referenced[e.StrongHash] {
			referencedEntries = append(referencedEntries, e)
		} else {
			orphanCount++
		}
	}

	// Deduplicate referenced entries by StrongHash — keep first occurrence only.
	// Duplicates can accumulate in the index after an interrupted backup or after
	// index rebuild, since Insert does not check for prior insertions.
	seenHashes := make(map[[32]byte]bool, len(referencedEntries))
	deduped := referencedEntries[:0]
	var duplicateCount int64
	for _, e := range referencedEntries {
		if seenHashes[e.StrongHash] {
			duplicateCount++
			continue
		}
		seenHashes[e.StrongHash] = true
		deduped = append(deduped, e)
	}
	referencedEntries = deduped

	// Compute byte stats before prune.
	bytesBefore := dirSize(filepath.Join(opts.RepoPath, "chunks"))
	packFilesBefore := countFiles(filepath.Join(opts.RepoPath, "chunks"))

	result := &Result{
		TotalChunks:      int64(len(allEntries)),
		ReferencedChunks: int64(len(referencedEntries)),
		OrphanedChunks:   orphanCount,
		DuplicateChunks:  duplicateCount,
		BytesBefore:      bytesBefore,
		PackFilesBefore:  packFilesBefore,
	}

	// Step 4: If dry-run, stop here.
	if opts.DryRun {
		result.Duration = time.Since(start)
		return result, nil
	}

	// Nothing to do if no orphans or duplicates — still clean up .bak files.
	if orphanCount == 0 && duplicateCount == 0 {
		result.BakFilesRemoved = removeBakFiles(opts.RepoPath)
		result.BytesAfter = bytesBefore
		result.PackFilesAfter = packFilesBefore
		result.Duration = time.Since(start)
		return result, nil
	}

	// Step 5: Rewrite referenced chunks into staging.
	chunkStore, err := store.NewChunkStore(opts.RepoPath, repoCfg.PackFileMaxSize, repoCfg.CompressionLevel, opts.Key)
	if err != nil {
		return nil, fmt.Errorf("opening chunk store: %w", err)
	}

	err = rewriteChunks(ctx, opts, repoCfg, chunkStore, referencedEntries, cfg, start, opts.Key)
	chunkStore.Close()
	if err != nil {
		return nil, fmt.Errorf("rewriting chunks: %w", err)
	}

	// Step 6: Atomic swap.
	if err := atomicSwap(opts.RepoPath); err != nil {
		return nil, fmt.Errorf("atomic swap: %w", err)
	}

	result.BakFilesRemoved = removeBakFiles(opts.RepoPath)
	result.BytesAfter = dirSize(filepath.Join(opts.RepoPath, "chunks"))
	result.BytesReclaimed = result.BytesBefore - result.BytesAfter
	result.PackFilesAfter = countFiles(filepath.Join(opts.RepoPath, "chunks"))
	result.Duration = time.Since(start)
	return result, nil
}

// collectReferencedHashes walks all manifests and collects every chunk hash
// that is still referenced. For watcher-mode backups with DataBackupID
// cross-references, follows those to include the referenced backup's chunks.
//
// Entries are streamed directly from .dnm files (no []Entry slice allocated),
// with a fallback to the legacy .entries sidecar for unmigrated backups.
func collectReferencedHashes(repoPath string) (map[[32]byte]bool, error) {
	// Enumerate manifest IDs directly from the directory rather than via
	// manifest.List(), which silently skips unreadable manifests. Prune must
	// see EVERY manifest: an unreadable one means the referenced hash set
	// would be incomplete, and its chunks would be wrongly deleted as orphans.
	ids, err := listManifestIDs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("listing manifests: %w", err)
	}

	referenced := make(map[[32]byte]bool)

	// Collect DataBackupIDs that need to be visited for cross-references.
	dataBackupIDs := make(map[string]bool)
	loadedIDs := make(map[string]bool)

	for _, id := range ids {
		loadedIDs[id] = true
		if err := streamHashes(repoPath, id, referenced); err != nil {
			return nil, fmt.Errorf("streaming hashes for backup %s: %w (all manifests must be readable for safe prune)", id, err)
		}
		// Need file catalog for DataBackupID cross-references. LoadCatalog reads
		// metadata + catalog only — Load would also pull the full entries
		// section into RAM (GBs for large manifests), defeating the streaming
		// design of streamHashes above.
		m, err := manifest.LoadCatalog(repoPath, id)
		if err != nil {
			return nil, fmt.Errorf("loading manifest %s: %w (all manifests must be readable for safe prune)", id, err)
		}
		for _, f := range m.FileCatalog {
			if f.DataBackupID != "" && !loadedIDs[f.DataBackupID] {
				dataBackupIDs[f.DataBackupID] = true
			}
		}
	}

	// Stream hashes from cross-referenced backups.
	for id := range dataBackupIDs {
		if loadedIDs[id] {
			continue
		}
		// Referenced backup may have been deleted; skip silently.
		_ = streamHashes(repoPath, id, referenced)
	}

	return referenced, nil
}

// listManifestIDs returns every backup ID that has a .dnm or .manifest file
// in the manifests directory, without attempting to parse any of them.
func listManifestIDs(repoPath string) ([]string, error) {
	return manifest.ListIDs(repoPath)
}

// streamHashes adds every ChunkHash in the given backup's ENTRIES section to
// dst. For .dnm backups it streams directly without allocating a []Entry slice.
// For legacy .entries sidecars it reads records in a 1 MiB buffered pass.
func streamHashes(repoPath, backupID string, dst map[[32]byte]bool) error {
	dnmPath := manifest.DNMPath(repoPath, backupID)
	if _, err := os.Stat(dnmPath); err == nil {
		r, err := manifest.OpenDNMReader(dnmPath)
		if err != nil {
			return err
		}
		defer r.Close()
		return r.StreamChunkHashes(func(h [32]byte) error {
			dst[h] = true
			return nil
		})
	}
	// Legacy path: read the .entries sidecar.
	entries, err := manifest.ReadEntries(repoPath, backupID)
	if err != nil {
		return err
	}
	if entries == nil {
		// Oldest format: entries embedded in the .manifest JSON, no sidecar
		// (format 3 in manifest.Load). ReadEntries returns nil for a missing
		// sidecar, and silently contributing zero hashes here would classify
		// every chunk of a still-restorable backup as an orphan — load the
		// manifest fully instead.
		b, err := manifest.Load(repoPath, backupID)
		if err != nil {
			return fmt.Errorf("loading legacy manifest %s: %w", backupID, err)
		}
		entries = b.Entries
	}
	for _, e := range entries {
		dst[e.ChunkHash] = true
	}
	return nil
}

// rewriteChunks copies referenced chunks (raw compressed frames) into new
// sequential pack files in a staging directory. Rebuilds the index and bloom filter.
func rewriteChunks(ctx context.Context, opts Options, repoCfg store.RepoConfig, chunkStore *store.ChunkStore, entries []index.IndexEntry, cfg config.Config, start time.Time, key *crypto.MasterKey) error {
	stagingDir := filepath.Join(opts.RepoPath, ".prune-staging")
	stagingChunks := filepath.Join(stagingDir, "chunks")
	stagingIndex := filepath.Join(stagingDir, "index")

	if err := os.MkdirAll(stagingChunks, 0755); err != nil {
		return fmt.Errorf("creating staging chunks dir: %w", err)
	}
	if err := os.MkdirAll(stagingIndex, 0755); err != nil {
		return fmt.Errorf("creating staging index dir: %w", err)
	}

	// Reconstruct the repo's normalizer: the staging bloom's weak hash must
	// be keyed on normalized bytes to match how the pipeline probes dedup,
	// otherwise every normalized chunk bloom-misses and is re-stored.
	normalizer, err := preprocess.FromNames(repoCfg.Normalizers)
	if err != nil {
		return fmt.Errorf("reconstructing normalizer from repo config: %w", err)
	}

	// Sort entries by (PackNumber, StoreOffset) for sequential I/O.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].PackNumber != entries[j].PackNumber {
			return entries[i].PackNumber < entries[j].PackNumber
		}
		return entries[i].StoreOffset < entries[j].StoreOffset
	})

	// Pre-scan to compute total raw frame bytes for progress reporting.
	// We estimate total bytes from a pre-read pass or just use chunk count.
	// For simplicity, compute total during copy and report progress by chunk count.

	// Create a new chunk store in staging for writing.
	stagingStore, err := store.NewChunkStore(stagingDir, repoCfg.PackFileMaxSize, repoCfg.CompressionLevel, key)
	if err != nil {
		return fmt.Errorf("opening staging store: %w", err)
	}
	// Close on early-return paths only; the success path closes explicitly
	// below so persistence errors fail the prune before atomicSwap runs.
	defer func() {
		if stagingStore != nil {
			stagingStore.Close()
		}
	}()

	// Create a new dedup index in staging. Same split as the read above: the
	// staging index is atomically swapped in as the repo's index, so writing
	// it under the chunk key would hand a managed repo an ENCRYPTED index the
	// controller cannot open (#265).
	expectedChunks := uint64(len(entries)) + 100
	stagingIdx, err := index.NewDedupIndexReadOnly(stagingIndex, expectedChunks, cfg.BloomFPRate, cfg.IndexCacheMB, store.IndexKeyFor(repoCfg, key))
	if err != nil {
		return fmt.Errorf("opening staging index: %w", err)
	}
	defer func() {
		if stagingIdx != nil {
			stagingIdx.Close()
		}
	}()

	// zstd decoder for decompressing chunks to compute xxhash for bloom filter.
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer decoder.Close()

	var bytesCopied int64
	chunksTotal := int64(len(entries))

	for i, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Read raw frame from old store.
		frame, _, err := chunkStore.RetrieveRaw(entry.PackNumber, int64(entry.StoreOffset))
		if err != nil {
			return fmt.Errorf("reading chunk %d: %w", i, err)
		}

		// Write raw frame directly to staging store.
		newPackNum, newOffset, _, err := stagingStore.StoreRaw(frame)
		if err != nil {
			return fmt.Errorf("writing chunk %d to staging: %w", i, err)
		}

		// Decrypt (if encrypted) and decompress to compute xxhash for bloom filter.
		payloadLen := binary.LittleEndian.Uint32(frame[0:4])
		payload := frame[8 : 8+payloadLen]
		compressed := payload
		if key != nil {
			compressed, err = key.DecryptWithAAD(payload, crypto.AADChunk)
			if err != nil {
				return fmt.Errorf("decrypting chunk %d for bloom: %w", i, err)
			}
		}
		decompressed, err := decoder.DecodeAll(compressed, nil)
		if err != nil {
			return fmt.Errorf("decompressing chunk %d for bloom: %w", i, err)
		}
		weakHash := xxhash.Sum64(preprocess.IdentityHashInput(normalizer, decompressed))

		// Insert into staging index with the new pack location.
		stagingIdx.Insert(
			hasher.ChunkID{WeakHash: weakHash, StrongHash: entry.StrongHash},
			newPackNum, uint64(newOffset), entry.ChunkLength,
		)

		bytesCopied += int64(len(frame))

		if opts.OnProgress != nil {
			opts.OnProgress(ProgressInfo{
				Phase:        "Copying chunks",
				ChunksCopied: int64(i + 1),
				ChunksTotal:  chunksTotal,
				BytesCopied:  bytesCopied,
				BytesTotal:   bytesCopied, // Updated as we go
				Elapsed:      time.Since(start),
			})
		}
	}

	// Persist the staging state explicitly. stagingIdx.Close performs the
	// ONLY flush of the staging hash index and bloom filter (inserts are
	// buffered in memory until then), and stagingStore.Close syncs and seals
	// the final staging pack. If either fails (e.g. ENOSPC), the prune must
	// abort here — otherwise atomicSwap would install a chunks dir with an
	// empty or missing index and then delete the originals.
	sIdx := stagingIdx
	stagingIdx = nil
	if err := sIdx.Close(); err != nil {
		return fmt.Errorf("flushing staging index: %w", err)
	}
	sStore := stagingStore
	stagingStore = nil
	if err := sStore.Close(); err != nil {
		return fmt.Errorf("closing staging store: %w", err)
	}

	return nil
}

// atomicSwap renames old dirs aside and moves staging into place.
func atomicSwap(repoPath string) error {
	chunksDir := filepath.Join(repoPath, "chunks")
	indexDir := filepath.Join(repoPath, "index")
	stagingDir := filepath.Join(repoPath, ".prune-staging")
	stagingChunks := filepath.Join(stagingDir, "chunks")
	stagingIndex := filepath.Join(stagingDir, "index")
	oldChunks := filepath.Join(repoPath, "chunks.prune-old")
	oldIndex := filepath.Join(repoPath, "index.prune-old")

	// Install a fresh pack-layout generation INSIDE staging before the swap, so
	// the renumbered packs and their new generation become live atomically
	// (#55/#56): a crash around the swap can never leave new packs under the
	// old (or empty) generation, which would let a suspended backup's fast-path
	// resume replay stale pack references.
	if err := store.WriteGenerationFile(stagingChunks); err != nil {
		return fmt.Errorf("writing packs generation into staging: %w", err)
	}

	// Install a fresh pack-layout generation INSIDE staging before the swap, so
	// the renumbered packs and their new generation become live atomically
	// (#55/#56): a crash around the swap can never leave new packs under the
	// old generation, which would let a suspended backup's fast-path resume
	// replay stale pack references.
	if err := store.WriteGenerationFile(stagingChunks); err != nil {
		return fmt.Errorf("writing packs generation into staging: %w", err)
	}

	// Move old dirs aside.
	if err := os.Rename(chunksDir, oldChunks); err != nil {
		return fmt.Errorf("moving chunks aside: %w", err)
	}

	if err := os.Rename(stagingChunks, chunksDir); err != nil {
		// Rollback: restore chunks.
		os.Rename(oldChunks, chunksDir)
		return fmt.Errorf("moving staging chunks into place: %w", err)
	}

	if err := os.Rename(indexDir, oldIndex); err != nil {
		// Rollback: restore chunks.
		os.Rename(chunksDir, stagingChunks)
		os.Rename(oldChunks, chunksDir)
		return fmt.Errorf("moving index aside: %w", err)
	}

	if err := os.Rename(stagingIndex, indexDir); err != nil {
		// Rollback: restore index and chunks.
		os.Rename(oldIndex, indexDir)
		os.Rename(chunksDir, stagingChunks)
		os.Rename(oldChunks, chunksDir)
		return fmt.Errorf("moving staging index into place: %w", err)
	}

	// Clean up old dirs and staging.
	os.RemoveAll(oldChunks)
	os.RemoveAll(oldIndex)
	os.RemoveAll(stagingDir)

	return nil
}

// recoverIfNeeded detects leftover staging/old dirs from an interrupted prune
// and cleans them up.
//
// The key invariant: chunks/ and index/ must always be consistent with each
// other. When staging exists, it contains the index that matches the current
// (post-swap) chunks. We must prefer staging/index over old index when
// chunks/ already contains the new compacted data.
// pruneRecoveryPending reports whether a prior interrupted prune left recovery
// state behind. It is the read-only predicate matching recoverIfNeeded's early
// return, so DryRun can detect a pending recovery without mutating anything.
func pruneRecoveryPending(repoPath string) bool {
	return dirExists(filepath.Join(repoPath, ".prune-staging")) ||
		dirExists(filepath.Join(repoPath, "chunks.prune-old")) ||
		dirExists(filepath.Join(repoPath, "index.prune-old"))
}

func recoverIfNeeded(repoPath string) error {
	stagingDir := filepath.Join(repoPath, ".prune-staging")
	oldChunks := filepath.Join(repoPath, "chunks.prune-old")
	oldIndex := filepath.Join(repoPath, "index.prune-old")
	chunksDir := filepath.Join(repoPath, "chunks")
	indexDir := filepath.Join(repoPath, "index")
	stagingIndex := filepath.Join(stagingDir, "index")

	hasStaging := dirExists(stagingDir)
	hasOldChunks := dirExists(oldChunks)
	hasOldIndex := dirExists(oldIndex)

	if !hasStaging && !hasOldChunks && !hasOldIndex {
		return nil // No recovery needed.
	}

	stagingIndexExists := dirExists(stagingIndex)

	switch {
	case hasOldChunks && !dirExists(chunksDir):
		// Crash between swap steps 1 and 2: chunks were moved aside but the
		// staging chunks never made it into place. Roll back to the old
		// consistent state (old chunks + old index).
		os.Rename(oldChunks, chunksDir)
		if hasOldIndex && !dirExists(indexDir) {
			os.Rename(oldIndex, indexDir)
		}

	case hasOldChunks && dirExists(chunksDir):
		// Swap step 2 completed: chunks/ holds the NEW compacted packs.
		// The index that matches them is the staging index — the old index
		// references the old pack numbering and must not be used.
		if stagingIndexExists {
			// Steps 3-4 incomplete. Discard whatever index/ holds (it is the
			// old, mismatched index) and roll the swap forward.
			os.RemoveAll(indexDir)
			os.Rename(stagingIndex, indexDir)
		} else if !dirExists(indexDir) && hasOldIndex {
			// No staging index available (should not happen mid-swap, but be
			// defensive) — restore old index as a last resort.
			os.Rename(oldIndex, indexDir)
		}
		os.RemoveAll(oldChunks)

	default:
		// No old chunks: either a crash during rewriteChunks (partial staging
		// only — discard it below) or during final cleanup (swap fully done).
		if !dirExists(indexDir) && hasOldIndex {
			os.Rename(oldIndex, indexDir)
		}
	}

	// Clean up any remaining old/staging dirs.
	if dirExists(oldIndex) && dirExists(indexDir) {
		os.RemoveAll(oldIndex)
	}
	if hasStaging {
		os.RemoveAll(stagingDir)
	}

	return nil
}

// removeBakFiles deletes all .manifest.bak and .entries.bak files left behind
// by migrate-manifest from the manifests directory. Returns the count removed.
func removeBakFiles(repoPath string) int {
	dir := filepath.Join(repoPath, "manifests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var removed int
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".manifest.bak") || strings.HasSuffix(name, ".entries.bak") {
			if os.Remove(filepath.Join(dir, name)) == nil {
				removed++
			}
		}
	}
	return removed
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func dirSize(path string) int64 {
	var total int64
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

func countFiles(path string) int {
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0
	}
	return len(entries)
}
