// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// nopCloser is a no-op io.Closer for use when no cleanup is needed.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// verifyExtractCovering recomputes a file's covering-chunk-hash derivation
// (manifest.ComputeContentHashes* — the #353 capture-side pass) over the
// entry accessor an extract used, and compares it to the catalog's stored
// ContentHash. Inline files carry their bytes in the catalog itself and
// need no covering check.
func verifyExtractCovering(f manifest.FileEntry, entries manifest.EntryAccessor, _ string) error {
	var zero [32]byte
	if f.ContentHash == zero || f.InlineData != nil || f.IsExcluded {
		return nil // pre-#353 row (not verifiable), or content not chunk-covered
	}
	check := f // the recompute writes into its argument; work on a copy
	check.ContentHash = zero
	if len(check.VolumeExtents) > 0 {
		manifest.ComputeContentHashesForVolumeFile(&check, entries)
	} else if check.StreamLength > 0 {
		manifest.ComputeContentHashesForFile(&check, entries)
	} else {
		return nil // empty file: nothing covered, nothing to disagree with
	}
	if check.ContentHash == zero {
		// The derivation could not resolve the covering entries — the same
		// condition would have produced a zero hash at capture, so a stored
		// NON-zero hash meeting it now means the entries changed shape.
		return fmt.Errorf("restoring %s: the file's covering chunk entries cannot be resolved against "+
			"this backup, but its catalog carries a content hash — the extraction used entries the "+
			"capture did not", f.Path)
	}
	if check.ContentHash != f.ContentHash {
		return fmt.Errorf("restoring %s: the extracted chunks' covering hash does not match the "+
			"catalog's — the output is a byte-perfect assembly of the wrong chunks; do not trust it "+
			"(catalog %x, extracted %x)", f.Path, f.ContentHash[:8], check.ContentHash[:8])
	}
	return nil
}

// FileRestoreResult contains the outcome of a file-mode restore.
type FileRestoreResult struct {
	TotalFiles      int
	RestoredFiles   int
	DirsCreated     int
	SymlinksCreated int
	BytesWritten    int64
	Duration        time.Duration
}

// FileRestorer restores files from a file-mode backup.
type FileRestorer struct {
	index        *index.DedupIndex
	store        *store.ChunkStore
	repoPath     string
	logger       *slog.Logger
	normalizer   preprocess.Normalizer
	IgnoreErrors bool // if true, log and skip files with incomplete catalog entries rather than aborting
}

// NewFileRestorer creates a new file restore engine.
// repoPath is needed to load referenced manifests for unchanged files in watcher backups.
func NewFileRestorer(idx *index.DedupIndex, st *store.ChunkStore, repoPath string, logger *slog.Logger) *FileRestorer {
	return &FileRestorer{
		index:    idx,
		store:    st,
		repoPath: repoPath,
		logger:   logger,
	}
}

// SetNormalizer configures the normalizer used to verify chunk integrity;
// it MUST match the normalizer the backup was created with. See
// Restorer.SetNormalizer.
func (r *FileRestorer) SetNormalizer(n preprocess.Normalizer) {
	r.normalizer = n
}

// openEntryAccessor opens an EntryAccessor for the given backup. It prefers
// the on-disk .dnm file for O(log n) seeks. If no disk file is available (e.g.
// in tests) it falls back to the already-loaded backup.Entries slice.
func OpenEntryAccessor(repoPath string, backup *manifest.Backup) (manifest.EntryAccessor, io.Closer) {
	ea, closer, err := manifest.NewEntryAccessor(repoPath, backup.BackupID)
	if err == nil && ea.Count() > 0 {
		return ea, closer
	}
	// Fallback: use in-memory entries already loaded with the backup.
	if closer != nil {
		closer.Close()
	}
	return manifest.NewSliceEntryAccessor(backup.Entries), nopCloser{}
}

// RestoreFiles restores files from a file-mode backup to targetDir.
// If filePatterns is non-empty, only matching files (and their parent dirs) are restored.
func (r *FileRestorer) RestoreFiles(ctx context.Context, backup *manifest.Backup, targetDir string, filePatterns []string) (*FileRestoreResult, error) {
	start := time.Now()

	if len(backup.FileCatalog) == 0 {
		return nil, fmt.Errorf("backup has no file catalog (use --file-mode or --capture-files during backup)")
	}

	// Filter catalog if patterns provided
	files := backup.FileCatalog
	if len(filePatterns) > 0 {
		files = FilterFiles(backup.FileCatalog, filePatterns)
	}

	result := &FileRestoreResult{
		TotalFiles: len(files),
	}

	// Sort entries into dirs, regular files, and symlinks
	var dirs, regulars, symlinks []manifest.FileEntry
	for _, f := range files {
		switch {
		case f.IsDir:
			dirs = append(dirs, f)
		case f.IsSymlink:
			symlinks = append(symlinks, f)
		default:
			regulars = append(regulars, f)
		}
	}

	// Open entry accessor. Entries are already sorted by VolumeOffset in .dnm
	// and sidecar files, so no copy or sort is needed.
	entries, entriesClose := OpenEntryAccessor(r.repoPath, backup)
	defer entriesClose.Close()

	// Phase 1: Create directories
	for _, d := range dirs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dirPath := filepath.Join(targetDir, filepath.FromSlash(d.Path))
		if err := os.MkdirAll(dirPath, os.FileMode(d.Mode)|0700); err != nil {
			return nil, fmt.Errorf("creating dir %s: %w", d.Path, err)
		}
		result.DirsCreated++
	}

	// Cache for accessors of referenced manifests (unchanged files in watcher backups).
	refAccessors := make(map[string]manifest.EntryAccessor)
	var refClosers []io.Closer
	defer func() {
		for _, c := range refClosers {
			c.Close()
		}
	}()

	// Chunk-frame prefetching (#204): one pack-grouped batch per window
	// instead of two ranged reads per chunk. The lookahead lets a window span
	// following files, so a tree of small files does not pay a round trip
	// each. nil (and a no-op) unless a batch fetcher is wired.
	pf := newFramePrefetcher(r.store, r.logger)
	defer pf.release()
	var la *fileLookahead
	if pf != nil {
		la = &fileLookahead{files: regulars, entries: entries, idx: r.index}
		pf.lookahead = la.refs
	}

	// Phase 2: Restore regular files
	for fi, f := range regulars {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		la.advanceTo(fi)

		filePath := filepath.Join(targetDir, filepath.FromSlash(f.Path))

		// Ensure parent dir exists
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			return nil, fmt.Errorf("creating parent dir for %s: %w", f.Path, err)
		}

		// Determine which entries to use for this file
		fileEntries := entries
		if f.Unchanged && f.DataBackupID != "" {
			re, err := r.getRefEntries(f.DataBackupID, refAccessors, &refClosers)
			if err != nil {
				return nil, fmt.Errorf("loading referenced backup for %s: %w", f.Path, err)
			}
			// Find this file in the referenced backup's catalog to get correct offsets
			refFile, err := r.findFileInBackup(f.DataBackupID, f.SourceIndex, f.Path)
			if err != nil {
				return nil, fmt.Errorf("finding %s in referenced backup: %w", f.Path, err)
			}
			// Take only the data location from the referenced entry; the
			// current backup's catalog holds the authoritative metadata
			// (a chmod with no content change leaves a file "unchanged").
			cur := f
			f = refFile
			f.Mode = cur.Mode
			f.ModTime = cur.ModTime
			fileEntries = re
		}

		var written int64
		var restoreErr error
		switch {
		case f.IsExcluded:
			// Blocks deliberately zeroed at capture (#94): refusing here (before
			// any output exists) gives --ignore-error its skip semantics below.
			restoreErr = excludedFileError(backup, f)
		case f.InlineData != nil:
			written, restoreErr = r.restoreInlineFile(f, filePath)
		case f.VolumeExtents != nil:
			written, restoreErr = r.restoreVolumeFile(f, filePath, fileEntries, pf)
		default:
			written, restoreErr = r.restoreFile(f, filePath, fileEntries, pf)
		}
		if restoreErr != nil {
			// Partial-output cleanup is owned by restoreFile/restoreVolumeFile/
			// restoreInlineFile (they know whether the output was created); a
			// remove here would delete a PRE-EXISTING file when the error fired
			// before the output was ever opened.
			if r.IgnoreErrors {
				r.logger.Warn("skipping file with restore error", "path", f.Path, "error", restoreErr)
				continue
			}
			return nil, fmt.Errorf("restoring %s: %w", f.Path, restoreErr)
		}
		result.BytesWritten += written
		result.RestoredFiles++

		// Set permissions and modification time
		os.Chmod(filePath, os.FileMode(f.Mode))
		os.Chtimes(filePath, f.ModTime, f.ModTime)
	}

	// Phase 3: Restore symlinks
	for _, f := range symlinks {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		linkPath := filepath.Join(targetDir, filepath.FromSlash(f.Path))

		// Ensure parent dir exists
		if err := os.MkdirAll(filepath.Dir(linkPath), 0755); err != nil {
			return nil, fmt.Errorf("creating parent dir for symlink %s: %w", f.Path, err)
		}

		if err := os.Symlink(f.LinkTarget, linkPath); err != nil {
			return nil, fmt.Errorf("creating symlink %s -> %s: %w", f.Path, f.LinkTarget, err)
		}
		result.SymlinksCreated++
	}

	// Phase 4: Restore directory permissions and timestamps (deepest first).
	// Phase 1 creates dirs with mode|0700 so they are writable during restore;
	// now we restore the original permissions.
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i].Path) > len(dirs[j].Path)
	})
	for _, d := range dirs {
		dirPath := filepath.Join(targetDir, filepath.FromSlash(d.Path))
		os.Chtimes(dirPath, d.ModTime, d.ModTime)
		os.Chmod(dirPath, os.FileMode(d.Mode))
	}

	result.Duration = time.Since(start)
	return result, nil
}

// ExtractFile extracts a single file from a file-mode backup to outputPath.
// Unlike RestoreFiles, this writes the file directly to outputPath without
// reconstructing the directory tree.
func (r *FileRestorer) ExtractFile(ctx context.Context, backup *manifest.Backup, filePath, outputPath string) (*FileRestoreResult, error) {
	start := time.Now()

	if len(backup.FileCatalog) == 0 {
		return nil, fmt.Errorf("backup has no file catalog (use --file-mode or --capture-files during backup)")
	}

	// Find exact file entry in catalog
	var found *manifest.FileEntry
	for i := range backup.FileCatalog {
		if backup.FileCatalog[i].Path == filePath {
			found = &backup.FileCatalog[i]
			break
		}
	}
	if found == nil {
		return nil, fmt.Errorf("file %q not found in backup catalog", filePath)
	}

	if found.IsDir {
		return nil, fmt.Errorf("%q is a directory, not a file", filePath)
	}
	if found.IsSymlink {
		return nil, fmt.Errorf("%q is a symlink, not a regular file", filePath)
	}

	// Open entry accessor — entries are pre-sorted, no copy/sort needed.
	entries, entriesClose := OpenEntryAccessor(r.repoPath, backup)
	defer entriesClose.Close()

	f := *found

	// Handle unchanged files that reference another backup
	fileEntries := entries
	if f.Unchanged && f.DataBackupID != "" {
		refAccessors := make(map[string]manifest.EntryAccessor)
		var refClosers []io.Closer
		defer func() {
			for _, c := range refClosers {
				c.Close()
			}
		}()
		re, err := r.getRefEntries(f.DataBackupID, refAccessors, &refClosers)
		if err != nil {
			return nil, fmt.Errorf("loading referenced backup for %s: %w", f.Path, err)
		}
		refFile, err := r.findFileInBackup(f.DataBackupID, f.SourceIndex, f.Path)
		if err != nil {
			return nil, fmt.Errorf("finding %s in referenced backup: %w", f.Path, err)
		}
		// Take only the data location from the referenced entry; the current
		// backup's catalog holds the authoritative metadata (a chmod with no
		// content change leaves a file "unchanged").
		cur := f
		f = refFile
		f.Mode = cur.Mode
		f.ModTime = cur.ModTime
		fileEntries = re
	}

	// Ensure parent directory of output path exists
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return nil, fmt.Errorf("creating parent dir for output: %w", err)
	}

	// Single-file extraction batches within the file (#204); there is no
	// following file to look ahead to.
	pf := newFramePrefetcher(r.store, r.logger)
	defer pf.release()

	var written int64
	var restoreErr error
	switch {
	case f.IsExcluded:
		restoreErr = excludedFileError(backup, f)
	case f.InlineData != nil:
		written, restoreErr = r.restoreInlineFile(f, outputPath)
	case f.VolumeExtents != nil:
		written, restoreErr = r.restoreVolumeFile(f, outputPath, fileEntries, pf)
	default:
		written, restoreErr = r.restoreFile(f, outputPath, fileEntries, pf)
	}
	if restoreErr != nil {
		// Partial-output cleanup is owned by the restore helpers (see the
		// multi-file twin above).
		if r.IgnoreErrors {
			r.logger.Warn("skipping file with restore error", "path", filePath, "error", restoreErr)
			return &FileRestoreResult{
				TotalFiles:    1,
				RestoredFiles: 0,
				Duration:      time.Since(start),
			}, nil
		}
		return nil, fmt.Errorf("restoring %s: %w", filePath, restoreErr)
	}

	// #465: hold the extraction against the catalog's ContentHash — a hash
	// of the COVERING CHUNK HASHES (#353), recomputed here over the entries
	// this extract actually used. Every retrieved chunk verified against its
	// own hash above; only this derivation can see that the chunks were the
	// WRONG ones — the file-mode analog of #376, produced by exactly the
	// extent/entry-mapping bug class findEntry has had. A zero hash is a
	// pre-#353 catalog row: not verifiable, extracted as always (§9).
	if err := verifyExtractCovering(f, fileEntries, outputPath); err != nil {
		os.Remove(outputPath) // a file that failed verification must not pose as the real one
		return nil, err
	}

	// Set permissions and modification time
	os.Chmod(outputPath, os.FileMode(f.Mode))
	os.Chtimes(outputPath, f.ModTime, f.ModTime)

	return &FileRestoreResult{
		TotalFiles:    1,
		RestoredFiles: 1,
		BytesWritten:  written,
		Duration:      time.Since(start),
	}, nil
}

// chunkData returns one chunk's bytes, preferring a frame the prefetcher
// already holds. A frame that fails to decode falls through to the
// authoritative per-chunk read, which re-fetches the bytes and reports the
// canonical error — a bad batch can slow a restore, never break one.
func (r *FileRestorer) chunkData(res chunkResolution, pf *framePrefetcher) ([]byte, error) {
	if frame, ok := pf.frame(res.ref.ChunkLoc); ok {
		if data, err := r.store.RetrieveFromFrame(frame); err == nil {
			return data, nil
		} else if r.logger != nil {
			r.logger.Warn("pre-fetched frame unusable; re-reading chunk",
				"pack", res.ref.PackNum, "offset", res.ref.StoreOffset, "error", err)
		}
	}
	data, err := r.store.Retrieve(res.ref.PackNum, res.ref.StoreOffset)
	if err != nil {
		return nil, fmt.Errorf("retrieving chunk from pack %d: %w", res.ref.PackNum, err)
	}
	return data, nil
}

// restoreFile restores a single regular file by finding overlapping chunks.
func (r *FileRestorer) restoreFile(f manifest.FileEntry, targetPath string, entries manifest.EntryAccessor, pf *framePrefetcher) (written int64, retErr error) {
	if f.StreamLength == 0 {
		if f.Size > 0 {
			// Non-empty file with no stream data: catalog entry is incomplete.
			// This happens when --capture-files was used on a volume backup whose
			// filesystem scanner did not populate VolumeExtents or StreamOffset/Length.
			// NOTE: this returns BEFORE the output is created, so no cleanup runs
			// — a pre-existing file at targetPath is left untouched.
			return 0, fmt.Errorf("catalog entry for %q has size %d but no stream data (re-backup with a fixed --capture-files)", f.Path, f.Size)
		}
		// Truly empty file
		out, err := os.Create(targetPath)
		if err != nil {
			return 0, err
		}
		return 0, out.Close()
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	// From this point WE own the output: on failure remove the partial file
	// (short-and-silent is data loss). The cleanup lives here — not in the
	// callers — because only this function knows whether the file was actually
	// created/truncated; a caller-side remove used to delete a PRE-EXISTING
	// file when the error fired before Create.
	defer func() {
		if retErr != nil {
			out.Close()
			os.Remove(targetPath)
		}
	}()

	fileStart := f.StreamOffset
	fileEnd := f.StreamOffset + f.StreamLength

	startIdx, err := manifest.SearchEntries(entries, fileStart)
	if err != nil {
		return 0, fmt.Errorf("searching entries for file start: %w", err)
	}
	endIdx, err := manifest.SearchEntriesEnd(entries, fileEnd)
	if err != nil {
		return 0, fmt.Errorf("searching entries for file end: %w", err)
	}

	chunk, err := entries.Range(startIdx, endIdx)
	if err != nil {
		return 0, fmt.Errorf("loading entry range [%d,%d): %w", startIdx, endIdx, err)
	}

	// Resolve every chunk's location up front so the fetcher can pull a whole
	// window in one round trip (#204) instead of one chunk at a time.
	res := r.resolveChunks(chunk)

	for i, e := range chunk {
		chunkStart := e.VolumeOffset
		chunkEnd := chunkStart + int64(e.ChunkLength)

		// Retrieve chunk data
		if res[i].err != nil {
			return written, fmt.Errorf("looking up chunk at offset %d: %w", chunkStart, res[i].err)
		}
		if !res[i].found {
			return written, fmt.Errorf("chunk not found in index (hash %x)", e.ChunkHash[:8])
		}

		pf.ensure(res, i)
		data, err := r.chunkData(res[i], pf)
		if err != nil {
			return written, err
		}

		// Verify integrity (re-normalize when the backup used a normalizer).
		actualHash := sha256.Sum256(preprocess.IdentityHashInput(r.normalizer, data))
		if actualHash != e.ChunkHash {
			return written, fmt.Errorf("chunk integrity error at offset %d", chunkStart)
		}

		// Extract the bytes that belong to this file
		sliceStart := int64(0)
		if chunkStart < fileStart {
			sliceStart = fileStart - chunkStart
		}
		sliceEnd := int64(len(data))
		if chunkEnd > fileEnd {
			sliceEnd = int64(len(data)) - (chunkEnd - fileEnd)
		}

		n, err := out.Write(data[sliceStart:sliceEnd])
		if err != nil {
			return written, err
		}
		written += int64(n)
	}

	// The entries covering [fileStart,fileEnd) must reconstruct exactly
	// StreamLength bytes. A short total means the range was empty or had a gap
	// (a missing middle entry), which would otherwise return "success" with a
	// truncated file — silent data loss. Fail loud instead.
	if written != f.StreamLength {
		return written, fmt.Errorf("restored %q incomplete: wrote %d of %d bytes (missing or misaligned chunk entries)", f.Path, written, f.StreamLength)
	}

	return written, nil
}

// restoreInlineFile writes InlineData directly to targetPath. Used for NTFS
// resident files whose data is embedded in the MFT entry rather than in
// cluster runs, so FSCTL_GET_RETRIEVAL_POINTERS returns no extents.
func (r *FileRestorer) restoreInlineFile(f manifest.FileEntry, targetPath string) (int64, error) {
	if err := os.WriteFile(targetPath, f.InlineData, os.FileMode(f.Mode)); err != nil {
		// WriteFile may have created/truncated the file before failing; remove
		// the partial output rather than leave a short file behind.
		os.Remove(targetPath)
		return 0, err
	}
	return int64(len(f.InlineData)), nil
}

// restoreVolumeFile restores a file using VolumeExtents (from --capture-files).
// Each extent maps a region of the file to a physical location on the volume,
// where the overlapping chunks are retrieved and reassembled.
func (r *FileRestorer) restoreVolumeFile(f manifest.FileEntry, targetPath string, entries manifest.EntryAccessor, pf *framePrefetcher) (written int64, retErr error) {
	if len(f.VolumeExtents) == 0 {
		// No extents: empty file
		out, err := os.Create(targetPath)
		if err != nil {
			return 0, err
		}
		return 0, out.Close()
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	// Owner-side cleanup of the partial output on failure (see restoreFile).
	defer func() {
		if retErr != nil {
			out.Close()
			os.Remove(targetPath)
		}
	}()

	var totalWritten int64

	for _, ext := range f.VolumeExtents {
		extStart := ext.VolumeOffset
		extEnd := ext.VolumeOffset + ext.Length

		startIdx, err := manifest.SearchEntries(entries, extStart)
		if err != nil {
			return totalWritten, fmt.Errorf("searching entries for extent start: %w", err)
		}
		endIdx, err := manifest.SearchEntriesEnd(entries, extEnd)
		if err != nil {
			return totalWritten, fmt.Errorf("searching entries for extent end: %w", err)
		}

		chunk, err := entries.Range(startIdx, endIdx)
		if err != nil {
			return totalWritten, fmt.Errorf("loading entry range [%d,%d): %w", startIdx, endIdx, err)
		}

		var extWritten int64

		// Batch this extent's chunk fetches (#204).
		res := r.resolveChunks(chunk)

		for i, e := range chunk {
			chunkStart := e.VolumeOffset
			chunkEnd := chunkStart + int64(e.ChunkLength)

			// Retrieve chunk data
			if res[i].err != nil {
				return totalWritten, fmt.Errorf("looking up chunk at offset %d: %w", chunkStart, res[i].err)
			}
			if !res[i].found {
				return totalWritten, fmt.Errorf("chunk not found in index (hash %x)", e.ChunkHash[:8])
			}

			pf.ensure(res, i)
			data, err := r.chunkData(res[i], pf)
			if err != nil {
				return totalWritten, err
			}

			// Verify integrity (re-normalize when the backup used a normalizer).
			actualHash := sha256.Sum256(preprocess.IdentityHashInput(r.normalizer, data))
			if actualHash != e.ChunkHash {
				return totalWritten, fmt.Errorf("chunk integrity error at offset %d", chunkStart)
			}

			// Calculate the slice of this chunk that overlaps the extent
			sliceStart := int64(0)
			if chunkStart < extStart {
				sliceStart = extStart - chunkStart
			}
			sliceEnd := int64(len(data))
			if chunkEnd > extEnd {
				sliceEnd = int64(len(data)) - (chunkEnd - extEnd)
			}

			// Write at the correct file offset
			fileWriteOffset := ext.FileOffset + extWritten
			n, err := out.WriteAt(data[sliceStart:sliceEnd], fileWriteOffset)
			if err != nil {
				return totalWritten, err
			}
			totalWritten += int64(n)
			extWritten += int64(n)
		}
	}

	// Set the file to its logical size unconditionally:
	//   - Extents are cluster-aligned on NTFS/FAT/exFAT, so totalWritten may
	//     exceed f.Size; truncating strips the cluster slack.
	//   - A file whose tail is a sparse hole has no extent covering it (the
	//     scanner skips sparse runs), so the writes above end short of
	//     f.Size; extending fills the tail with zeros as on the source.
	if f.Size > 0 && totalWritten != f.Size {
		if err := out.Truncate(f.Size); err != nil {
			return totalWritten, fmt.Errorf("setting logical size: %w", err)
		}
		if totalWritten > f.Size {
			totalWritten = f.Size
		}
	}

	return totalWritten, nil
}

// getRefEntries loads and caches an EntryAccessor for a referenced backup.
// Closers are appended to the provided slice for deferred cleanup.
func (r *FileRestorer) getRefEntries(backupID string, cache map[string]manifest.EntryAccessor, closers *[]io.Closer) (manifest.EntryAccessor, error) {
	if cached, ok := cache[backupID]; ok {
		return cached, nil
	}

	ea, closer, err := manifest.NewEntryAccessor(r.repoPath, backupID)
	if err != nil || ea.Count() == 0 {
		// Fallback: load from manifest and use in-memory slice
		if closer != nil {
			closer.Close()
		}
		ref, err2 := manifest.Load(r.repoPath, backupID)
		if err2 != nil {
			return nil, fmt.Errorf("loading manifest %s: %w", backupID, err2)
		}
		ea = manifest.NewSliceEntryAccessor(ref.Entries)
		closer = nopCloser{}
	}

	cache[backupID] = ea
	*closers = append(*closers, closer)
	return ea, nil
}

// findFileInBackup loads a backup and finds the FileEntry for the given file.
// The match is on (SourceIndex, Path): two source directories can contain the
// same relative path, so matching on path alone would resolve an unchanged
// file to another source's data.
func (r *FileRestorer) findFileInBackup(backupID string, sourceIndex int, filePath string) (manifest.FileEntry, error) {
	ref, err := manifest.Load(r.repoPath, backupID)
	if err != nil {
		return manifest.FileEntry{}, fmt.Errorf("loading manifest %s: %w", backupID, err)
	}

	f, ok := findEntry(ref.FileCatalog, sourceIndex, filePath)
	if !ok {
		return manifest.FileEntry{}, fmt.Errorf("file %q (source %d) not found in backup %s", filePath, sourceIndex, backupID)
	}
	return f, nil
}

// findEntry returns the catalog entry for (sourceIndex, path). It prefers an
// exact (SourceIndex, Path) match; if none exists it falls back to a path-only
// match for backward compatibility with single-source backups written before
// SourceIndex was recorded consistently.
func findEntry(catalog []manifest.FileEntry, sourceIndex int, path string) (manifest.FileEntry, bool) {
	var pathOnly manifest.FileEntry
	var havePathOnly bool
	for _, f := range catalog {
		if f.Path != path {
			continue
		}
		if f.SourceIndex == sourceIndex {
			return f, true
		}
		if !havePathOnly {
			pathOnly, havePathOnly = f, true
		}
	}
	return pathOnly, havePathOnly
}

// filterFiles returns catalog entries matching any of the given patterns,
// plus parent directories needed to contain them.
func FilterFiles(catalog []manifest.FileEntry, patterns []string) []manifest.FileEntry {
	// Collect matching entries
	var matched []manifest.FileEntry
	neededDirs := make(map[string]bool)

	for _, f := range catalog {
		if f.IsDir {
			continue // handle dirs separately
		}

		for _, pat := range patterns {
			if MatchFilePattern(f.Path, pat) {
				matched = append(matched, f)
				// Track parent dirs needed
				parts := strings.Split(f.Path, "/")
				for i := 1; i < len(parts); i++ {
					neededDirs[strings.Join(parts[:i], "/")] = true
				}
				break
			}
		}
	}

	// Add needed parent directories
	var dirs []manifest.FileEntry
	for _, f := range catalog {
		if f.IsDir && neededDirs[f.Path] {
			dirs = append(dirs, f)
		}
	}

	return append(dirs, matched...)
}

// matchFilePattern checks if a file path matches a glob pattern.
func MatchFilePattern(path, pattern string) bool {
	// Handle ** recursive glob
	if strings.Contains(pattern, "**") {
		parts := strings.SplitN(pattern, "**", 2)
		prefix := strings.TrimSuffix(parts[0], "/")
		suffix := strings.TrimPrefix(parts[1], "/")

		if prefix != "" && !strings.HasPrefix(path, prefix+"/") && path != prefix {
			return false
		}

		if suffix == "" {
			return true
		}

		// Check if any suffix of the path matches the suffix pattern
		pathParts := strings.Split(path, "/")
		for i := 0; i <= len(pathParts); i++ {
			remaining := strings.Join(pathParts[i:], "/")
			if ok, _ := filepath.Match(suffix, remaining); ok {
				return true
			}
			// Also try matching just the basename
			if i == len(pathParts)-1 {
				if ok, _ := filepath.Match(suffix, pathParts[i]); ok {
					return true
				}
			}
		}
		return false
	}

	// Simple glob against basename
	basename := path
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		basename = path[idx+1:]
	}

	if ok, _ := filepath.Match(pattern, basename); ok {
		return true
	}
	// Try matching full path
	if ok, _ := filepath.Match(pattern, path); ok {
		return true
	}
	return false
}
