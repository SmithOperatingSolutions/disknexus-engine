// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Target is the destination a volume restore writes into. *volume.Writer
// satisfies it; the interface lives here so the core engine does not
// depend on the platform layer.
type Target interface {
	io.WriterAt
	Truncate(size int64) error
	Sync() error
}

// RestoreResult contains the outcome of a restore operation.
type RestoreResult struct {
	TotalChunks    int64
	RestoredChunks int64
	ExcludedChunks int64
	BytesWritten   int64
	Duration       time.Duration
}

// Restorer restores backups from a repository to a target writer.
type Restorer struct {
	index      *index.DedupIndex
	store      *store.ChunkStore
	logger     *slog.Logger
	normalizer preprocess.Normalizer
	// OnProgress (#153): periodic (bytesWritten, totalBytes) during Restore —
	// recovery flows render percent/rate/ETA from it.
	OnProgress func(done, total int64)
}

// NewRestorer creates a new restore engine.
func NewRestorer(idx *index.DedupIndex, st *store.ChunkStore, logger *slog.Logger) *Restorer {
	return &Restorer{
		index:  idx,
		store:  st,
		logger: logger,
	}
}

// SetNormalizer configures the normalizer used to verify chunk integrity.
// It MUST match the normalizer the backup was created with (reconstructed
// from the repo config): chunk identity is the hash of the normalized bytes
// while the stored bytes are the originals, so verification re-normalizes
// before comparing against the manifest's chunk hash. A nil normalizer
// (default) verifies the stored bytes directly.
func (r *Restorer) SetNormalizer(n preprocess.Normalizer) {
	r.normalizer = n
}

// Restore writes the backup contents to the target writer. It is
// RestoreEntries over the backup's in-memory entries; a caller holding a DNM
// (or a chain of them) should pass its accessor to RestoreEntries directly
// and never hold the entries whole (#506).
func (r *Restorer) Restore(ctx context.Context, backup *manifest.Backup, writer Target) (*RestoreResult, error) {
	return r.RestoreEntries(ctx, backup, manifest.NewSliceEntryAccessor(backup.Entries), writer)
}
