// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
)

// entryBatch is how many entries pass 1 reads per Range call: one
// sequential read of ~3 MB from a DNM, and the only slice of Entry structs
// alive at any moment.
const entryBatch = 1 << 16

// chunkRef is what pass 1 keeps per non-excluded entry: 24 bytes, against
// ~48 for the Entry it came from plus the slice that held it. At 1.26M
// entries that is ~30 MB of refs instead of ~100 MB of entries — and the
// refs are what the pack-major sort needs anyway.
type chunkRef struct {
	entryIdx int64
	pack     uint32
	off      uint64
}

// RestoreEntries writes the backup to the target, reading its entries
// through ea instead of a materialized []Entry (#506). Two passes:
//
//  1. stream the entries in batches: write zeros for excluded regions as
//     they are met (one reused buffer, not one per region) and collect a
//     chunkRef for every real chunk — the index lookup happens here;
//  2. sort the refs pack-major (#83: every pack fetched exactly once) and
//     restore each chunk, re-reading only that entry through ea.At, an O(1)
//     seek on a DNM.
//
// Restore is this with a slice accessor over backup.Entries; callers that
// hold a DNM (or a chain of them — an incremental's parents) pass its
// accessor and never hold the entries whole.
func (r *Restorer) RestoreEntries(ctx context.Context, backup *manifest.Backup, ea manifest.EntryAccessor, writer Target) (*RestoreResult, error) {
	start := time.Now()
	count := ea.Count()

	// A backup claiming data but carrying no entries (e.g. a legacy manifest
	// whose .entries sidecar was lost) cannot be restored. Without this guard
	// the loop below runs zero iterations and reports success after truncating
	// the target — an all-zero "restore" of the whole volume.
	if count == 0 && backup.TotalBytes > 0 {
		return nil, fmt.Errorf("backup %s has no chunk entries but claims %d bytes; its entries are missing or corrupt (try 'index --rebuild-all' or re-import)", backup.BackupID, backup.TotalBytes)
	}
	if err := writer.Truncate(backup.TotalBytes); err != nil {
		return nil, fmt.Errorf("truncating target to %d bytes: %w", backup.TotalBytes, err)
	}

	var result RestoreResult
	result.TotalChunks = count

	// Pass 1: excluded regions and refs.
	var refs []chunkRef
	var zeros []byte
	for lo := int64(0); lo < count; lo += entryBatch {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		hi := lo + entryBatch
		if hi > count {
			hi = count
		}
		batch, err := ea.Range(lo, hi)
		if err != nil {
			return nil, fmt.Errorf("reading entries [%d,%d): %w", lo, hi, err)
		}
		for j, entry := range batch {
			i := lo + int64(j)
			if entry.IsExcluded {
				if cap(zeros) < entry.ChunkLength {
					zeros = make([]byte, entry.ChunkLength)
				}
				if _, err := writer.WriteAt(zeros[:entry.ChunkLength], entry.VolumeOffset); err != nil {
					return nil, fmt.Errorf("writing zeros at offset %d: %w", entry.VolumeOffset, err)
				}
				result.ExcludedChunks++
				result.BytesWritten += int64(entry.ChunkLength)
				continue
			}
			idxEntry, found, err := r.index.LookupDirect(entry.ChunkHash)
			if err != nil {
				return nil, fmt.Errorf("looking up chunk %d: %w", i, err)
			}
			if !found {
				return nil, fmt.Errorf("chunk %d not found in index (hash %x)", i, entry.ChunkHash[:8])
			}
			refs = append(refs, chunkRef{i, idxEntry.PackNumber, idxEntry.StoreOffset})
		}
	}
	sort.Slice(refs, func(a, b int) bool {
		if refs[a].pack != refs[b].pack {
			return refs[a].pack < refs[b].pack
		}
		return refs[a].off < refs[b].off
	})

	// Pass 2: pack-major chunk restore.
	for n, ref := range refs {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entry, err := ea.At(ref.entryIdx)
		if err != nil {
			return nil, fmt.Errorf("reading entry %d: %w", ref.entryIdx, err)
		}
		data, err := r.store.Retrieve(ref.pack, int64(ref.off))
		if err != nil {
			return nil, fmt.Errorf("retrieving chunk %d from pack %d offset %d: %w", ref.entryIdx, ref.pack, ref.off, err)
		}
		// Verify integrity. When the backup used a normalizer, chunk identity
		// is the hash of the NORMALIZED bytes, so hash what the normalizer
		// would have produced before comparing.
		actualHash := sha256.Sum256(preprocess.IdentityHashInput(r.normalizer, data))
		if actualHash != entry.ChunkHash {
			return nil, fmt.Errorf("chunk %d integrity error: expected %x, got %x", ref.entryIdx, entry.ChunkHash[:8], actualHash[:8])
		}
		if len(data) != entry.ChunkLength {
			return nil, fmt.Errorf("chunk %d size mismatch: expected %d, got %d", ref.entryIdx, entry.ChunkLength, len(data))
		}
		if _, err := writer.WriteAt(data, entry.VolumeOffset); err != nil {
			return nil, fmt.Errorf("writing chunk %d at offset %d: %w", ref.entryIdx, entry.VolumeOffset, err)
		}
		result.RestoredChunks++
		result.BytesWritten += int64(len(data))
		if r.OnProgress != nil && (n%256 == 0 || n == len(refs)-1) {
			r.OnProgress(result.BytesWritten, backup.TotalBytes)
		}
	}

	if err := writer.Sync(); err != nil {
		return nil, fmt.Errorf("syncing target: %w", err)
	}
	result.Duration = time.Since(start)
	return &result, nil
}
