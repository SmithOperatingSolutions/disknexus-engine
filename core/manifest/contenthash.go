// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// Covering-content-hash derivation (#353), moved here from engine/filemode
// so engine/core/restore may recompute it at extract time (#465 slice 4)
// without a core→platform import (TestCoreDoesNotImportPlatform). It is
// manifest math: FileEntry + Entry in, hash of covering chunk hashes out.

import (
	"crypto/sha256"
)

// ComputeContentHashes computes a ContentHash for each FileEntry in the backup
// by hashing the SHA-256 chunk hashes that overlap the file's stream range.
// This is a zero-I/O operation — it only reads already-computed chunk hashes.
func ComputeContentHashes(backup *Backup) {
	if len(backup.FileCatalog) == 0 || len(backup.Entries) == 0 {
		return
	}

	// Entries are already sorted by VolumeOffset from the pipeline and .dnm format.
	entries := NewSliceEntryAccessor(backup.Entries)

	for i := range backup.FileCatalog {
		f := &backup.FileCatalog[i]
		if f.Unchanged {
			continue
		}
		if len(f.VolumeExtents) > 0 {
			ComputeContentHashesForVolumeFile(f, entries)
		} else if f.StreamLength > 0 {
			ComputeContentHashesForFile(f, entries)
		}
	}
}

// ComputeContentHashesForVolumeFile computes the ContentHash for a FileEntry
// that uses VolumeExtents (from --capture-files on volume backups).
// It hashes the chunk hashes that overlap any of the file's VolumeExtents.
// The entries accessor must be sorted by VolumeOffset.
func ComputeContentHashesForVolumeFile(f *FileEntry, entries EntryAccessor) {
	if len(f.VolumeExtents) == 0 {
		return
	}

	h := sha256.New()
	for _, ext := range f.VolumeExtents {
		extStart := ext.VolumeOffset
		extEnd := ext.VolumeOffset + ext.Length

		startIdx, err := SearchEntries(entries, extStart)
		if err != nil {
			return
		}
		endIdx, err := SearchEntriesEnd(entries, extEnd)
		if err != nil {
			return
		}

		chunk, err := entries.Range(startIdx, endIdx)
		if err != nil {
			return
		}

		for _, e := range chunk {
			h.Write(e.ChunkHash[:])
		}
	}

	var hash [32]byte
	copy(hash[:], h.Sum(nil))
	f.ContentHash = hash
}

// ComputeContentHashesForFile computes the ContentHash for a single FileEntry
// by hashing the SHA-256 chunk hashes that overlap the file's stream range.
// The entries accessor must be sorted by VolumeOffset.
func ComputeContentHashesForFile(f *FileEntry, entries EntryAccessor) {
	if f.StreamLength == 0 {
		return
	}

	fileStart := f.StreamOffset
	fileEnd := f.StreamOffset + f.StreamLength

	startIdx, err := SearchEntries(entries, fileStart)
	if err != nil {
		return
	}
	endIdx, err := SearchEntriesEnd(entries, fileEnd)
	if err != nil {
		return
	}

	chunk, err := entries.Range(startIdx, endIdx)
	if err != nil {
		return
	}

	h := sha256.New()
	for _, e := range chunk {
		h.Write(e.ChunkHash[:])
	}

	var hash [32]byte
	copy(hash[:], h.Sum(nil))
	f.ContentHash = hash
}
