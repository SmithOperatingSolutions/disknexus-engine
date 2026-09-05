// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package resume holds the repo-side reconciliation a checkpointed backup needs
// before it can continue (issue #42, hardened for prune-coexistence in #56,
// segment fast path in #55).
package resume

import (
	"bytes"
	"context"
	"fmt"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Result is what a reconciled repository hands the pipeline to resume.
type Result struct {
	StartPack uint32 // append point for new packs: max existing pack + 1
	// Preload is the suspended session's index inserts replayed from checkpoint
	// segments (fast path). Empty when the index was rebuilt instead.
	Preload []checkpoint.InsertTuple
	// NextSeq continues checkpoint/segment numbering.
	NextSeq uint32
	// Rebuilt reports whether the slow path (full index rebuild) ran.
	Rebuilt bool
}

// Reconcile prepares a repository to resume the given checkpoint.
//
// Fast path (#55): replay the local checkpoint segments — CRC-validated deltas
// of sidecar bytes + index inserts written at every pack seal. When they exactly
// cover the checkpoint's durable prefix and match the on-disk sidecar, the
// session's inserts are handed back as Preload for the pipeline to re-insert
// (unflushed until success): no index rebuild, no pack rehash. This is also the
// only resume path for managed-encryption repos (index.Rebuild refuses them).
//
// Slow path: full index rebuild from the packs on disk plus a prefix-presence
// check (#42/#56 behavior) — used when segments are missing, torn, or stale
// (e.g. a prune renumbered packs and removed the segments file).
//
// Both paths are pack-number-agnostic: StartPack comes from the real max pack
// on disk, never the checkpoint's recorded pack number (#56).
func Reconcile(ctx context.Context, repoPath string, c *checkpoint.Checkpoint, key *crypto.MasterKey) (Result, error) {
	res := Result{NextSeq: c.CheckpointSeq + 1}

	if pre, ok := tryFastPath(repoPath, c); ok {
		res.Preload = pre
	} else {
		// Stale/absent segments: rebuild and verify (#42/#56), and clear the
		// segment file so a later resume never replays stale pack references.
		_ = checkpoint.RemoveSegmentsLocal(repoPath, c.BackupID)
		if _, err := index.Rebuild(ctx, index.RebuildOptions{
			RepoPath:         repoPath,
			RebuildBloom:     true,
			RebuildHashIndex: true,
			Key:              key,
		}); err != nil {
			return res, fmt.Errorf("rebuilding index from surviving packs (segments unavailable): %w", err)
		}
		if err := verifyPrefixPresent(repoPath, c, key); err != nil {
			return res, err
		}
		res.Rebuilt = true
	}

	maxPack, found, err := store.MaxPackNum(repoPath)
	if err != nil {
		return res, fmt.Errorf("scanning packs: %w", err)
	}
	if found {
		res.StartPack = maxPack + 1
	}
	return res, nil
}

// tryFastPath validates the local segments against the checkpoint and the
// on-disk sidecar. On success it truncates any over-run segments (written after
// the checkpoint — a crash window) so resumed numbering can't collide, and
// returns the replayed inserts.
func tryFastPath(repoPath string, c *checkpoint.Checkpoint) ([]checkpoint.InsertTuple, bool) {
	// A prune since the checkpoint renumbered packs; the segments' absolute
	// pack references are stale. Rebuild instead.
	if store.PacksGeneration(repoPath) != c.PacksGeneration {
		return nil, false
	}
	segs, err := checkpoint.ReadSegmentsLocal(repoPath, c.BackupID)
	if err != nil || len(segs) == 0 {
		return nil, false
	}
	// Keep exactly the segments the checkpoint covers: seq 0..CheckpointSeq.
	keep := int(c.CheckpointSeq) + 1
	if len(segs) < keep {
		return nil, false // segments missing for a checkpoint that requires them
	}
	rs, err := checkpoint.ReplaySegments(segs[:keep], c)
	if err != nil {
		return nil, false
	}
	// The replayed sidecar prefix must byte-match the durable sidecar — a cheap
	// integrity cross-check between the two independently-written artifacts.
	onDisk, err := os.ReadFile(manifest.EntriesPath(repoPath, c.BackupID))
	if err != nil || int64(len(onDisk)) < c.EntriesLen {
		return nil, false
	}
	if !bytes.Equal(onDisk[:c.EntriesLen], rs.SidecarPrefix) {
		return nil, false
	}
	// Drop over-run segments so the resumed run's next segment (seq
	// CheckpointSeq+1) never collides with a stale one.
	if len(segs) > keep {
		if err := checkpoint.TruncateSegmentsLocal(repoPath, c.BackupID, keep); err != nil {
			return nil, false
		}
	}
	return rs.Inserts, true
}

// verifyPrefixPresent confirms every chunk in the checkpoint's durable prefix
// resolves in the (freshly rebuilt) index, so the completed backup will restore.
func verifyPrefixPresent(repoPath string, c *checkpoint.Checkpoint, key *crypto.MasterKey) error {
	count := int(c.EntriesCount)
	if count <= 0 {
		return nil
	}
	entries, err := manifest.ReadEntries(repoPath, c.BackupID)
	if err != nil {
		return fmt.Errorf("reading checkpoint entries: %w", err)
	}
	if len(entries) < count {
		return fmt.Errorf("entries sidecar has %d records but checkpoint prefix expects %d", len(entries), count)
	}

	// key is the CHUNK key. A managed repo's index is plaintext, and opening
	// it with the chunk key deletes bloom.bin/hash-index.db on close rather
	// than merely encrypting them (#265) — store.IndexKeyFor is the one place
	// that rule lives.
	repoCfg, cfgErr := store.LoadRepoConfig(repoPath)
	if cfgErr != nil {
		return fmt.Errorf("loading repo config for prefix check: %w", cfgErr)
	}
	idx, err := index.NewDedupIndexReadOnly(repoPath+"/index", index.ReadOpenExpectedChunks, 0.001, 0, store.IndexKeyFor(repoCfg, key))
	if err != nil {
		return fmt.Errorf("opening index for prefix check: %w", err)
	}
	defer idx.CloseDiscard()

	// Read the full entry set directly (LookupDirect relies on in-memory state a
	// freshly-opened read-only index does not populate; ReadAllEntries reads the
	// on-disk hash index, so it reflects a just-rebuilt index).
	all, err := idx.ReadAllEntries()
	if err != nil {
		return fmt.Errorf("reading index for prefix check: %w", err)
	}
	present := make(map[[32]byte]bool, len(all))
	for _, e := range all {
		present[e.StrongHash] = true
	}

	for i := 0; i < count; i++ {
		e := entries[i]
		if e.IsExcluded {
			continue
		}
		if !present[e.ChunkHash] {
			return fmt.Errorf("resume data missing: prefix chunk %d (offset %d) is not present — a prune may have removed it; use --restart to start over", i, e.VolumeOffset)
		}
	}
	return nil
}
