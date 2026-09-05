// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline

import (
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/chunker"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/klauspost/compress/zstd"
)

// nopCloser is a no-op io.Closer for use when no resource cleanup is needed.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// HashedChunk is a chunk with its computed hashes.
type HashedChunk struct {
	Chunk chunker.Chunk
	ID    hasher.ChunkID
}

// PreparedChunk is a hashed chunk with a pre-built frame ready for StoreRaw.
// The Frame contains [4B payload len][4B raw len][compressed+encrypted payload].
type PreparedChunk struct {
	Chunk       chunker.Chunk
	ID          hasher.ChunkID
	Frame       []byte // complete frame for StoreRaw
	PayloadSize int    // len(payload) excluding 8-byte header
	seq         uint64 // original chunk index; used by the sequencer to restore order

	// digestStateBefore is the content digest's marshaled state over chunks
	// 0..seq-1 (#455). Attached by the chunker — the one place the stream is
	// still sequential — and consumed by emitCheckpoint, whose resume point
	// re-processes THIS chunk, so the persisted fold must exclude it.
	digestStateBefore []byte
}

// seqChunk pairs a raw chunk with its position in the input stream so the
// sequencer goroutine can re-order results from parallel hash workers.
type seqChunk struct {
	chunk chunker.Chunk
	seq   uint64
	// digestStateBefore: see PreparedChunk.digestStateBefore.
	digestStateBefore []byte
}

// seqHeap is a min-heap of PreparedChunk values ordered by seq.
// It is used by the sequencer to buffer out-of-order worker results and
// emit them in the original chunk order.
type seqHeap []PreparedChunk

func (h seqHeap) Len() int           { return len(h) }
func (h seqHeap) Less(i, j int) bool { return h[i].seq < h[j].seq }
func (h seqHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *seqHeap) Push(x any)        { *h = append(*h, x.(PreparedChunk)) }
func (h *seqHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// Result contains the outcome of a backup or analyze operation.
type Result struct {
	BackupID     string
	TotalChunks  int64
	UniqueChunks int64
	DedupChunks  int64
	RawBytes     int64
	StoredBytes  int64
	DedupRatio   float64
	CompRatio    float64
	Duration     time.Duration

	// Chunk size distribution
	MinChunkSize int
	MaxChunkSize int
	AvgChunkSize int

	// Index stats
	BloomMisses int64 // definite negatives (fast path)
	BloomHits   int64 // possible positives (needed disk check)

	// Incremental backup stats
	ParentBackupID  string
	ChangedChunks   int64
	UnchangedChunks int64
}

// fileCatalogData holds file-mode metadata to embed in the manifest.
type fileCatalogData struct {
	Mode        string
	SourcePaths []string
	Files       []manifest.FileEntry // nil after sidecar is written
	SidecarPath string               // temp file with pre-serialized catalog records
	Count       int64                // number of FileEntry records
}

// ProgressInfo holds data passed to the progress callback.
type ProgressInfo struct {
	BytesProcessed int64
	TotalBytes     int64
	ChunksTotal    int64
	ChunksNew      int64
	ChunksDedup    int64
	StoredBytes    int64
	Elapsed        time.Duration
}

// Pipeline orchestrates the backup processing stages.
type Pipeline struct {
	cfg         config.Config
	logger      *slog.Logger
	normalizer  preprocess.Normalizer
	fileCatalog *fileCatalogData  // nil for volume mode
	key         *crypto.MasterKey // chunk encryption key
	indexKey    *crypto.MasterKey // index encryption key (nil under managed encryption, on purpose)

	// mode and bound record what the Binding said, so run() can re-state the
	// invariant the constructor enforced (#265). Pipeline has exported
	// fields, so &Pipeline{BackupID: "x"} compiles from any package and would
	// otherwise reach the write path with no key and no mode.
	mode  store.EncryptionMode
	bound bool

	StartPackNum uint32                 // >0 to use NewChunkStoreAt
	OnPackSealed store.OnPackSealedFunc // wired to ChunkStore on creation

	// BackupID, when non-empty, is used as the backup's ID instead of a
	// freshly generated UUID. Cloud backups set it to the controller-issued ID
	// so the uploaded manifest is named by the same ID the controller lists —
	// otherwise `restore --backup <listed-id>` can never find the manifest.
	BackupID string

	OnProgress       func(ProgressInfo) // called periodically during backup
	ProgressInterval time.Duration      // defaults to 1s if OnProgress is set

	// OnActivity is called on the same tick as OnProgress with the running
	// byte count. It exists separately from OnProgress for two reasons.
	//
	// One: OnProgress fires on a TICKER whether or not any bytes moved, so
	// "the callback ran" proves the progress goroutine is alive and nothing
	// more. A watchdog keyed on it could never see a wedged capture. The byte
	// count is the part that actually advances.
	//
	// Two: every capture flow assigns OnProgress itself, in its own order
	// relative to the rest of its wiring, so a hook installed by wrapping
	// OnProgress is silently overwritten by two of the three. This one is set
	// by the pack-seal door and nothing downstream touches it.
	OnActivity func(bytesProcessed int64)

	// IndexDeltaPath arms index-delta capture for this run (#357 phase 2):
	// the entries this backup adds are journalled there as they are inserted,
	// and the cloud session publishes that file as the run's index change
	// instead of overwriting the repository's whole index. Empty (the default,
	// and every local repo) means no capture — a local repo's index IS the
	// file on disk, so there is nobody to send a change to.
	IndexDeltaPath string

	// OnIndexReady is called once the dedup index is loaded and ready.
	// entries is the number of index entries; elapsed is the load time.
	OnIndexReady func(entries int64, elapsed time.Duration)

	// Resumable, when true, makes the backup checkpointable (#42): each pack
	// seal invokes CheckpointFn, and a SIGINT/SIGTERM interrupt after the first
	// checkpoint returns ErrSuspended while preserving the sealed packs and the
	// entries sidecar (rather than discarding them as a failed backup).
	Resumable bool

	// StreamManifest (lever 4): when set, entry records stream to it in
	// bounded windows instead of the local .entries sidecar, no local .dnm is
	// written, and FinishManifest is called with the completed metadata so
	// the caller can emit the tail part and request server-side composition.
	// Incompatible with Resumable.
	StreamManifest *manifest.DNMStreamer
	FinishManifest func(*manifest.Backup) error
	// StampManifest, when set, is called with the completed metadata BEFORE
	// it is saved or streamed — for fields only the caller knows and every
	// manifest path must carry (#468: the operator's exclusions and which
	// of them did not apply). Unlike FinishManifest it never replaces the
	// save; it decorates what is about to be written.
	StampManifest func(*manifest.Backup)

	// CheckpointFn, when set, is invoked from the single-threaded store loop
	// immediately after a pack seals and BEFORE the triggering chunk is indexed
	// or written to the sidecar — so the checkpoint lags the durable seal and
	// names a resume point that is re-stored exactly once on resume. The Delta
	// carries what changed since the previous checkpoint (sidecar bytes + index
	// inserts) so the storage layer can persist replayable segments (#50/#55).
	// The CLI closure adds source/config identity and writes both atomically
	// (segment first, then the checkpoint record).
	CheckpointFn func(checkpoint.Progress, checkpoint.Delta) error

	// Resume inputs (set via BackupResume); zero for a fresh backup.
	// Content digest (#455): folded in the chunker goroutine, where the
	// stream is still sequential. digestValid goes false when a resume
	// carries no state (a suspend that predates the digest) — the manifest
	// then records NO digest rather than a wrong one, because a wrong
	// stored digest makes an honest verify condemn a healthy backup.
	digest      hash.Hash
	digestValid bool
	digestFinal []byte // hex-ready sum, set by the chunker goroutine at EOF

	resuming          bool
	resumeOffset      int64
	resumeEntriesLen  int64
	resumeDigestState []byte
	resumePrefix      Stats
	resumeSeq         uint32
	preloadInserts    []checkpoint.InsertTuple
}

// Stats is the running manifest tally carried across a resume so the completed
// manifest covers both the pre-interrupt prefix and the resumed suffix.
type Stats struct {
	TotalChunks  int64
	RawBytes     int64
	UniqueChunks int64
	DedupChunks  int64
	StoredBytes  int64
}

// ResumeState carries the validated-checkpoint inputs needed to continue an
// interrupted backup.
type ResumeState struct {
	BackupID     string
	StartPackNum uint32 // append point: max existing pack + 1
	ResumeOffset int64
	EntriesLen   int64
	PrefixStats  Stats

	// DigestState resumes the #455 content digest from the checkpoint. Empty
	// means the suspended run predates the digest; the resumed backup then
	// records none rather than a wrong one.
	DigestState []byte

	// PreloadInserts are the suspended session's dedup-index inserts, replayed
	// from checkpoint segments (#50/#55). They are inserted (unflushed) into the
	// resumed run's index so the prefix's chunks resolve and dedup hits them —
	// without rebuilding the index from packs. Empty means the caller already
	// reconciled the index another way (e.g. local rebuild fallback).
	PreloadInserts []checkpoint.InsertTuple

	// NextCheckpointSeq continues segment numbering: the suspended checkpoint's
	// CheckpointSeq + 1 (0 for a fresh backup).
	NextCheckpointSeq uint32
}

// ErrSuspended is returned by a resumable backup that was interrupted
// (SIGINT/SIGTERM) after at least one checkpoint. The sealed packs and the
// entries sidecar are preserved; re-running the backup resumes from the
// checkpoint. It is distinct from a genuine failure, which discards them.
var ErrSuspended = fmt.Errorf("backup suspended (resumable)")

// New creates a Pipeline for the repo the Binding was made for.
//
// The Binding is mandatory (#265). It used to be a variadic list of keys with
// a separate optional SetNormalizer call, and both were routinely omitted: the
// agent's three cloud write paths passed no key at all, so a
// managed-encryption repo received plaintext chunks, and only one of the
// module's fifteen write paths ever set a normalizer. Making
// store.RepoConfig — the thing store.RepoConfigFromRecord and
// store.LoadRepoConfig produce — the mandatory input means a write path that
// has not named the repo it is writing for does not compile.
func New(cfg config.Config, logger *slog.Logger, b Binding) *Pipeline {
	return &Pipeline{
		cfg:        cfg,
		logger:     logger,
		key:        b.key,
		indexKey:   b.indexKey,
		normalizer: b.norm,
		mode:       b.mode,
		bound:      b.bound,
	}
}

// SetFileCatalog configures file-mode metadata to be written into the manifest.
func (p *Pipeline) SetFileCatalog(mode string, sourcePaths []string, files []manifest.FileEntry) {
	p.fileCatalog = &fileCatalogData{Mode: mode, SourcePaths: sourcePaths, Files: files}
}

// BackupResume continues an interrupted backup from a validated checkpoint.
// The caller must have reopened the SAME source and seeked it to
// rs.ResumeOffset; the pipeline re-chunks from there, appends to the truncated
// entries sidecar, and numbers new packs from rs.StartPackNum. The completed
// restore is byte-identical to an uninterrupted backup of the same source.
func (p *Pipeline) BackupResume(ctx context.Context, reader io.Reader, sourceVolume string, totalSize int64, repoPath string, rs ResumeState) (*Result, error) {
	p.BackupID = rs.BackupID
	p.StartPackNum = rs.StartPackNum
	p.resuming = true
	p.resumeOffset = rs.ResumeOffset
	p.resumeEntriesLen = rs.EntriesLen
	p.resumePrefix = rs.PrefixStats
	p.resumeSeq = rs.NextCheckpointSeq
	p.preloadInserts = rs.PreloadInserts
	p.resumeDigestState = rs.DigestState
	return p.run(ctx, reader, sourceVolume, totalSize, repoPath, false)
}

// Backup runs a full backup from reader to the repository.
func (p *Pipeline) Backup(ctx context.Context, reader io.Reader, sourceVolume string, totalSize int64, repoPath string) (*Result, error) {
	return p.run(ctx, reader, sourceVolume, totalSize, repoPath, false)
}

// Analyze runs the chunking and dedup pipeline without storing data.
// Useful for measuring dedup effectiveness.
func (p *Pipeline) Analyze(ctx context.Context, reader io.Reader, sourceVolume string, totalSize int64, repoPath string) (*Result, error) {
	return p.run(ctx, reader, sourceVolume, totalSize, repoPath, true)
}

// emitCheckpoint builds and writes a resume checkpoint at a pack seal. It
// fsyncs the entries sidecar first so the recorded EntriesLen is durable on
// disk (never a buffered count), then hands the progress to CheckpointFn (the
// CLI closure that adds identity and writes the file atomically). The prefix
// tallies and boundary entry describe state as of the chunk BEFORE pc, so pc is
// the resume point re-stored exactly once on resume.
func (p *Pipeline) emitCheckpoint(ew *manifest.EntryWriter, pc PreparedChunk, packNum uint32, prefix Stats, lastEntry manifest.Entry, haveLast bool, seq uint32, prevEntriesLen int64, inserts []checkpoint.InsertTuple) (int64, error) {
	if err := ew.Sync(); err != nil {
		return 0, fmt.Errorf("syncing entries for checkpoint: %w", err)
	}
	entriesLen, err := ew.Len()
	if err != nil {
		return 0, fmt.Errorf("measuring entries for checkpoint: %w", err)
	}
	prog := checkpoint.Progress{
		LastSealedPack: packNum - 1,
		CheckpointSeq:  seq,
		ResumeOffset:   pc.Chunk.Offset,
		EntriesLen:     entriesLen,
		EntriesCount:   entriesLen / manifest.EntryRecordSize,
		TotalChunks:    prefix.TotalChunks,
		RawBytes:       prefix.RawBytes,
		UniqueChunks:   prefix.UniqueChunks,
		DedupChunks:    prefix.DedupChunks,
		StoredBytes:    prefix.StoredBytes,
	}
	prog.DigestState = pc.digestStateBefore
	if haveLast {
		prog.BoundaryChunkHash = lastEntry.ChunkHash
		prog.BoundaryChunkOffset = lastEntry.VolumeOffset
		prog.BoundaryChunkLength = lastEntry.ChunkLength
	}
	// Delta since the previous checkpoint: the sidecar bytes just made durable
	// plus the index inserts accumulated by the store loop (#50/#55).
	delta := checkpoint.Delta{Inserts: inserts}
	if entriesLen > prevEntriesLen {
		delta.SidecarBytes = make([]byte, entriesLen-prevEntriesLen)
		if _, err := ew.ReadAt(delta.SidecarBytes, prevEntriesLen); err != nil {
			return 0, fmt.Errorf("reading sidecar delta for checkpoint: %w", err)
		}
	}
	if err := p.CheckpointFn(prog, delta); err != nil {
		return 0, err
	}
	return entriesLen, nil
}

func (p *Pipeline) run(parentCtx context.Context, reader io.Reader, sourceVolume string, totalSize int64, repoPath string, analyzeOnly bool) (*Result, error) {
	// Defence in depth for #265, stated before the chunker and before any
	// file is created. New/Bind already make these unreachable for a pipeline
	// built the normal way; this closes the struct-literal path the
	// constructor cannot.
	//
	// Analyze is deliberately not carved out. It writes no chunks, so
	// requiring a key is stricter than necessary — accepted, because an
	// analyze-without-key path would be a second constructor, and a second
	// constructor is the hole.
	if !p.bound {
		return nil, fmt.Errorf("%w: constructed without pipeline.New/Bind (#265)", ErrUnbound)
	}
	if p.mode != store.EncryptNone && p.key == nil {
		return nil, fmt.Errorf("%w: mode %q reached the write path with no key (#265)", ErrKeyRequired, p.mode)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	start := time.Now()

	// Open the dedup index
	indexDir := repoPath + "/index"
	expectedChunks := uint64(totalSize/int64(p.cfg.ChunkAvgSize)) + 1000
	indexStart := time.Now()
	dedupIdx, err := index.NewDedupIndex(indexDir, expectedChunks, p.cfg.BloomFPRate, p.cfg.IndexCacheMB, p.indexKey)
	if err != nil {
		return nil, fmt.Errorf("opening dedup index: %w", err)
	}
	if p.OnIndexReady != nil {
		p.OnIndexReady(int64(dedupIdx.Stats().IndexEntries), time.Since(indexStart))
	}
	// Arm delta capture AFTER the open, which is also after any pending
	// deltas were merged — so this run's delta holds this run's work and not
	// the repository's (index.Delta.ApplyTo bypasses capture on purpose).
	if p.IndexDeltaPath != "" && !analyzeOnly {
		if err := dedupIdx.CaptureDelta(p.IndexDeltaPath); err != nil {
			dedupIdx.CloseDiscard()
			return nil, fmt.Errorf("arming index delta capture: %w", err)
		}
	}
	// Refuse to BACK UP against a missing-bloom/populated-index repo: the empty
	// bloom reports every chunk as new and the whole source would be re-stored
	// as duplicates. This guard lives here — on the write path — rather than in
	// NewDedupIndex, because restore/verify/export use LookupDirect (bloom
	// bypassed) and must keep working to recover intact data.
	if !analyzeOnly && dedupIdx.BloomSuspect() {
		dedupIdx.CloseDiscard()
		return nil, fmt.Errorf("bloom filter is missing but the hash index is populated in %s; run 'disknexus index --rebuild-all' before backing up (continuing would bypass dedup and re-store everything)", indexDir)
	}
	// Persist index state only when a real backup completes. Analyze must leave
	// the repo untouched (an unconditional Close used to flush a fresh —
	// undersized and unresizable — bloom.bin even with zero inserts), and a
	// FAILED backup must not durably record inserts referencing chunks in a
	// pack that was never sealed/uploaded.
	backupSucceeded := false
	// suspended: a resumable backup interrupted after a checkpoint. Its sealed
	// packs and entries sidecar are preserved (not discarded) so resume can
	// continue. checkpointWritten records that at least one checkpoint is durable.
	suspended := false
	checkpointWritten := false
	defer func() {
		if backupSucceeded && !analyzeOnly {
			dedupIdx.Close()
		} else {
			// CloseDiscard drops only in-memory session inserts and the
			// ephemeral hash table; it never deletes sealed pack files, so a
			// suspended backup's packs survive for resume to rebuild from.
			dedupIdx.CloseDiscard()
		}
	}()
	if p.cfg.MemFlushedIndex {
		dedupIdx.SetMemFlushed(true)
	}

	// Resume preload (#50/#55): replay the suspended session's index inserts
	// (from checkpoint segments) into this run's index as ordinary session
	// inserts. They resolve the prefix's chunks for dedup and the final restore,
	// and become durable only via the success-path Flush — no rebuild needed.
	//
	// The weak hash is replayed with them (#365). Without it the preload fed
	// the bloom nothing but weak-hash zero, and the bloom this run flushes on
	// success is the repo's — so the prefix's chunks were missing from tier-1
	// forever and every later backup re-stored them. (A segment written by a
	// build older than #365 has no weak hash to give; that one resumed run
	// keeps the old behavior rather than being refused.)
	if p.resuming && len(p.preloadInserts) > 0 {
		for _, t := range p.preloadInserts {
			dedupIdx.Insert(hasher.ChunkID{WeakHash: t.WeakHash, StrongHash: t.StrongHash}, t.PackNumber, t.StoreOffset, t.ChunkLength)
		}
		p.logger.Debug("preloaded resume index inserts", "count", len(p.preloadInserts))
	}

	// Open the chunk store (only needed for actual backup)
	var chunkStore *store.ChunkStore
	if !analyzeOnly {
		if p.StartPackNum > 0 {
			chunkStore, err = store.NewChunkStoreAt(repoPath, p.cfg.PackFileMaxSize, p.cfg.CompressionLevel, p.StartPackNum, p.key)
		} else {
			chunkStore, err = store.NewChunkStore(repoPath, p.cfg.PackFileMaxSize, p.cfg.CompressionLevel, p.key)
		}
		if err != nil {
			return nil, fmt.Errorf("opening chunk store: %w", err)
		}
		if p.OnPackSealed != nil {
			chunkStore.OnPackSealed = p.OnPackSealed
		}
		// Close on early-return paths only; the success path closes
		// explicitly before saving the manifest (Close seals the final pack,
		// which for cloud backups uploads it — that error must fail the
		// backup, not vanish in a deferred call).
		defer func() {
			if chunkStore != nil {
				chunkStore.Close()
			}
		}()
	}

	// Create a zstd encoder for parallel compression in workers.
	// This is separate from the store's encoder so they don't interfere.
	var pipelineEncoder *zstd.Encoder
	if !analyzeOnly {
		level := zstd.SpeedDefault
		switch {
		case p.cfg.CompressionLevel <= 1:
			level = zstd.SpeedFastest
		case p.cfg.CompressionLevel <= 3:
			level = zstd.SpeedDefault
		case p.cfg.CompressionLevel <= 6:
			level = zstd.SpeedBetterCompression
		default:
			level = zstd.SpeedBestCompression
		}
		pipelineEncoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
		if err != nil {
			return nil, fmt.Errorf("creating pipeline zstd encoder: %w", err)
		}
		defer pipelineEncoder.Close()
	}

	// Spill the file catalog to a temp sidecar before starting the chunk
	// goroutines. This frees the full []FileEntry slice (~320 MB for 1M files)
	// for the duration of the chunk phase. The DNM writer will stream from the
	// sidecar instead of encoding FileCatalog in-process.
	if p.fileCatalog != nil && len(p.fileCatalog.Files) > 0 {
		if tmp, tmpErr := os.CreateTemp("", "disknexus-catalog-*.tmp"); tmpErr != nil {
			p.logger.Warn("catalog sidecar create failed, keeping in memory", "err", tmpErr)
		} else {
			sidecarPath := tmp.Name()
			tmp.Close() // WriteCatalogSidecar re-opens by name
			if count, spillErr := manifest.WriteCatalogSidecar(p.fileCatalog.Files, sidecarPath); spillErr != nil {
				p.logger.Warn("catalog sidecar write failed, keeping in memory", "err", spillErr)
				os.Remove(sidecarPath)
			} else {
				p.fileCatalog.SidecarPath = sidecarPath
				p.fileCatalog.Count = count
				p.fileCatalog.Files = nil // release ~320 MB
				defer os.Remove(sidecarPath)
			}
		}
	}

	// Set up pipeline channels
	chunkCh := make(chan seqChunk, 64)
	preparedCh := make(chan PreparedChunk, 64)

	var result Result
	var minChunk, maxChunk int
	minChunk = int(^uint(0) >> 1) // max int

	// Atomic counters for progress reporting
	var bytesProcessed atomic.Int64
	var atomicTotalChunks, atomicUniqueChunks, atomicDedupChunks, atomicStoredBytes atomic.Int64

	// Stage 1: Chunker (single-threaded, deterministic)
	// #455: the digest starts fresh, or from the checkpoint's state on
	// resume. A resume WITHOUT state (suspended before the digest existed)
	// disables it for this backup rather than recording a fold of half the
	// stream as though it covered all of it.
	p.digest = sha256.New()
	p.digestValid = true
	if p.resuming {
		if len(p.resumeDigestState) == 0 {
			p.digestValid = false
		} else if um, ok := p.digest.(encoding.BinaryUnmarshaler); ok {
			if err := um.UnmarshalBinary(p.resumeDigestState); err != nil {
				return nil, fmt.Errorf("restoring content-digest state from checkpoint: %w", err)
			}
		}
	}

	chunkerDone := make(chan error, 1)
	go func() {
		defer close(chunkCh)
		c := chunker.New(reader,
			chunker.WithMinSize(p.cfg.ChunkMinSize),
			chunker.WithMaxSize(p.cfg.ChunkMaxSize),
			chunker.WithMask(p.cfg.BuzhashMask),
		)
		// Resume: the caller has positioned reader at resumeOffset (a prior
		// chunk boundary); re-seed the chunker so the first emitted chunk's
		// Offset == resumeOffset and downstream coverage is contiguous.
		if p.resuming && p.resumeOffset > 0 {
			c.Reset(reader, p.resumeOffset)
		}
		var seq uint64
		for {
			if ctx.Err() != nil {
				chunkerDone <- ctx.Err()
				return
			}
			chunk, err := c.Next()
			if err == io.EOF {
				if p.digestValid {
					p.digestFinal = p.digest.Sum(nil)
				}
				chunkerDone <- nil
				return
			}
			if err != nil {
				chunkerDone <- fmt.Errorf("chunker: %w", err)
				return
			}
			// #455: state BEFORE this chunk travels with it — a checkpoint's
			// resume point re-processes exactly this chunk, so the persisted
			// fold must exclude it. Marshaled only when a checkpoint could
			// consume it; folded always.
			var stateBefore []byte
			if p.digestValid && p.Resumable {
				if bm, ok := p.digest.(encoding.BinaryMarshaler); ok {
					if b, merr := bm.MarshalBinary(); merr == nil {
						stateBefore = b
					}
				}
			}
			if p.digestValid {
				p.digest.Write(chunk.Data)
			}
			bytesProcessed.Add(int64(chunk.Length))
			select {
			case chunkCh <- seqChunk{chunk: chunk, seq: seq, digestStateBefore: stateBefore}:
				seq++
			case <-ctx.Done():
				chunkerDone <- ctx.Err()
				return
			}
		}
	}()

	// Stage 2: Hash workers (parallel)
	numWorkers := p.cfg.HashWorkers
	if numWorkers <= 0 {
		n := runtime.NumCPU()
		if n > 1 {
			numWorkers = n - 1
		} else {
			numWorkers = 1
		}
	}

	var workerErr atomic.Pointer[error]
	var hashWg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		hashWg.Add(1)
		go func() {
			defer hashWg.Done()
			for sc := range chunkCh {
				hashData := sc.chunk.Data
				if p.normalizer != nil {
					hashData = p.normalizer.Normalize(sc.chunk.Data)
				}
				id := hasher.Sum(hashData)

				pc := PreparedChunk{Chunk: sc.chunk, ID: id, seq: sc.seq, digestStateBefore: sc.digestStateBefore}

				// Pre-compress and encrypt in the worker (parallelized).
				// Skip when analyze-only since no data is stored.
				if !analyzeOnly && pipelineEncoder != nil {
					compressed := pipelineEncoder.EncodeAll(sc.chunk.Data, nil)

					payload := compressed
					if p.key != nil {
						encrypted, err := p.key.EncryptWithAAD(compressed, crypto.AADChunk)
						if err != nil {
							workerErr.CompareAndSwap(nil, &err)
							// Cancel the pipeline: this chunk's seq will never
							// reach the sequencer, so without cancellation the
							// heap would buffer the entire remaining stream in
							// memory and the error would only surface at EOF.
							cancel()
							return
						}
						payload = encrypted
					}

					// Build frame: [4B payload len][4B raw len][payload]
					frame := make([]byte, 8+len(payload))
					binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
					binary.LittleEndian.PutUint32(frame[4:8], uint32(len(sc.chunk.Data)))
					copy(frame[8:], payload)

					pc.Frame = frame
					pc.PayloadSize = len(payload)
				}

				select {
				case preparedCh <- pc:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// On any return (including early error returns from the store loop),
	// cancel the pipeline context and wait for all hash workers to exit
	// before deferred cleanup (e.g. pipelineEncoder.Close) runs. This
	// prevents both goroutine leaks and use-after-close of the encoder.
	defer func() {
		cancel()
		hashWg.Wait()
	}()

	go func() {
		hashWg.Wait()
		close(preparedCh)
	}()

	// Sequencer: re-emits PreparedChunks in their original input order.
	// Parallel hash workers deliver results to preparedCh out of order;
	// the sequencer buffers them in a min-heap and emits each chunk only
	// when all earlier chunks have been emitted. This keeps entries in the
	// sidecar sorted by VolumeOffset without any post-processing sort.
	orderedCh := make(chan PreparedChunk, 64)
	go func() {
		defer close(orderedCh)
		h := &seqHeap{}
		heap.Init(h)
		var nextSeq uint64
		for pc := range preparedCh {
			heap.Push(h, pc)
			for h.Len() > 0 && (*h)[0].seq == nextSeq {
				ordered := heap.Pop(h).(PreparedChunk)
				select {
				case orderedCh <- ordered:
				case <-ctx.Done():
					return
				}
				nextSeq++
			}
		}
		// Drain any remaining chunks after preparedCh closes.
		for h.Len() > 0 {
			select {
			case orderedCh <- heap.Pop(h).(PreparedChunk):
			case <-ctx.Done():
				return
			}
		}
	}()

	// Progress reporting goroutine
	var progressStop chan struct{}
	var progressDone chan struct{}
	if p.OnProgress != nil || p.OnActivity != nil {
		progressStop = make(chan struct{})
		progressDone = make(chan struct{})
		interval := p.ProgressInterval
		if interval <= 0 {
			interval = time.Second
		}
		go func() {
			defer close(progressDone)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if p.OnActivity != nil {
						p.OnActivity(bytesProcessed.Load())
					}
					if p.OnProgress == nil {
						continue
					}
					p.OnProgress(ProgressInfo{
						BytesProcessed: bytesProcessed.Load(),
						TotalBytes:     totalSize,
						ChunksTotal:    atomicTotalChunks.Load(),
						ChunksNew:      atomicUniqueChunks.Load(),
						ChunksDedup:    atomicDedupChunks.Load(),
						StoredBytes:    atomicStoredBytes.Load(),
						Elapsed:        time.Since(start),
					})
				case <-progressStop:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	// Stage 3: Dedup + Store (single-threaded)
	backupID := p.BackupID
	if backupID == "" {
		backupID = manifest.NewBackupID()
	}
	var storedBytes int64

	// Open the binary entries sidecar for streaming writes (backup mode only).
	// With StreamManifest set (lever 4), entry records go to the streamer
	// instead and no local sidecar or .dnm is ever written.
	var entryWriter *manifest.EntryWriter
	if p.StreamManifest != nil && (p.Resumable || p.resuming) {
		return nil, fmt.Errorf("streamed manifests are incompatible with resumable backups")
	}
	if !analyzeOnly && p.StreamManifest == nil {
		if p.resuming {
			// Reopen and truncate to the checkpoint's durable length, dropping
			// the interrupted run's un-checkpointed tail, then append.
			entryWriter, err = manifest.OpenEntryWriterResume(repoPath, backupID, p.resumeEntriesLen)
		} else {
			entryWriter, err = manifest.OpenEntryWriter(repoPath, backupID)
		}
		if err != nil {
			return nil, fmt.Errorf("opening entry writer: %w", err)
		}
		// On failure, close the writer (freeing the fd) and remove the
		// half-written sidecar — error returns used to leak both, and in a
		// long-lived process (watcher, cloud agent) the leaked fds and stale
		// .entries files accumulate. On success the sidecar must SURVIVE this
		// function: manifest.Save embeds and then deletes it. On SUSPEND it must
		// also survive: resume reads it (do not remove).
		defer func() {
			if !backupSucceeded {
				entryWriter.Close() // best-effort; may already be closed
				if !suspended {
					os.Remove(manifest.EntriesPath(repoPath, backupID))
				}
			}
		}()
	}

	// Analyze mode uses a local hash set for within-session dedup tracking.
	// This avoids writing any data to the persistent index (which would corrupt
	// it with zero pack-number entries). Only 32 bytes per unique chunk.
	var analyzeSeenHashes map[[32]byte]struct{}
	if analyzeOnly {
		analyzeSeenHashes = make(map[[32]byte]struct{})
	}

	// Resume: seed running tallies from the checkpoint so the completed manifest
	// covers the pre-interrupt prefix plus the resumed suffix.
	if p.resuming {
		result.TotalChunks = p.resumePrefix.TotalChunks
		result.RawBytes = p.resumePrefix.RawBytes
		result.UniqueChunks = p.resumePrefix.UniqueChunks
		result.DedupChunks = p.resumePrefix.DedupChunks
		storedBytes = p.resumePrefix.StoredBytes
	}
	// curPack tracks the highest pack written into; a StoreRaw returning a higher
	// number means the previous pack just sealed. prefix snapshots the tallies as
	// of the last completed chunk (so a seal checkpoint excludes the triggering
	// chunk, which becomes the resume point). lastEntry is that chunk's sidecar
	// entry, the boundary-probe target.
	curPack := p.StartPackNum
	prefix := Stats{
		TotalChunks: result.TotalChunks, RawBytes: result.RawBytes,
		UniqueChunks: result.UniqueChunks, DedupChunks: result.DedupChunks,
		StoredBytes: storedBytes,
	}
	var lastEntry manifest.Entry
	var haveLastEntry bool
	// Segment bookkeeping (#50/#55): sequence continues across a resume, the
	// previous checkpoint's durable sidecar length bounds the next delta, and
	// pendingInserts accumulates this run's index inserts since that checkpoint.
	ckptSeq := p.resumeSeq
	prevCkptEntriesLen := p.resumeEntriesLen
	var pendingInserts []checkpoint.InsertTuple

	for pc := range orderedCh {
		result.TotalChunks++
		atomicTotalChunks.Add(1)
		result.RawBytes += int64(pc.Chunk.Length)

		if pc.Chunk.Length < minChunk {
			minChunk = pc.Chunk.Length
		}
		if pc.Chunk.Length > maxChunk {
			maxChunk = pc.Chunk.Length
		}

		// Dedup check
		dedupResult, err := dedupIdx.Check(pc.ID)
		if err != nil {
			return nil, fmt.Errorf("dedup check: %w", err)
		}

		if dedupResult.BloomMiss {
			result.BloomMisses++
		}
		if dedupResult.BloomHit {
			result.BloomHits++
		}

		// In analyze mode, also check chunks seen earlier this session.
		if analyzeOnly && dedupResult.IsNew {
			if _, ok := analyzeSeenHashes[pc.ID.StrongHash]; ok {
				dedupResult.IsNew = false
			}
		}

		if dedupResult.IsNew {
			result.UniqueChunks++
			atomicUniqueChunks.Add(1)

			if analyzeOnly {
				// Track in local set only — never write to the persistent index.
				analyzeSeenHashes[pc.ID.StrongHash] = struct{}{}
			} else {
				packNum, offset, compSize, err := chunkStore.StoreRaw(pc.Frame)
				if err != nil {
					return nil, fmt.Errorf("storing chunk: %w", err)
				}
				// A pack just sealed (StoreRaw already fsynced its data and the
				// chunks/ dirent). Checkpoint the resume point BEFORE this chunk
				// is indexed or written to the sidecar, so the checkpoint lags
				// the durable seal: a crash re-stores exactly this one chunk
				// (harmless dup) rather than losing data.
				if p.CheckpointFn != nil && packNum > curPack {
					newLen, err := p.emitCheckpoint(entryWriter, pc, packNum, prefix, lastEntry, haveLastEntry, ckptSeq, prevCkptEntriesLen, pendingInserts)
					if err != nil {
						return nil, fmt.Errorf("writing checkpoint: %w", err)
					}
					checkpointWritten = true
					curPack = packNum
					ckptSeq++
					prevCkptEntriesLen = newLen
					pendingInserts = nil
				}
				storedBytes += int64(compSize) + 8 // +8 for header
				atomicStoredBytes.Add(int64(compSize) + 8)
				dedupIdx.Insert(pc.ID, packNum, uint64(offset), uint32(pc.Chunk.Length))
				if p.CheckpointFn != nil {
					// The WEAK hash rides along (#365): it is the key the
					// dedup index's bloom filter is built on, a resumed run
					// cannot recompute it (the plaintext went with the
					// suspended process, and rebuilding the bloom from packs
					// is impossible for a cloud repo), and the bloom that run
					// flushes on success becomes the repo's own.
					pendingInserts = append(pendingInserts, checkpoint.InsertTuple{
						StrongHash: pc.ID.StrongHash, WeakHash: pc.ID.WeakHash, PackNumber: packNum,
						StoreOffset: uint64(offset), ChunkLength: uint32(pc.Chunk.Length),
					})
				}

				// Flush the hash index buffer every 500K new chunks to cap memory usage.
				if result.UniqueChunks%500_000 == 0 {
					if err := dedupIdx.FlushHashIndex(); err != nil {
						return nil, fmt.Errorf("periodic index flush: %w", err)
					}
				}
			}
		} else {
			result.DedupChunks++
			atomicDedupChunks.Add(1)
		}

		if entryWriter != nil || p.StreamManifest != nil {
			e := manifest.Entry{
				VolumeOffset: pc.Chunk.Offset,
				ChunkHash:    pc.ID.StrongHash,
				ChunkLength:  pc.Chunk.Length,
			}
			if p.StreamManifest != nil {
				if err := p.StreamManifest.WriteEntry(e); err != nil {
					return nil, fmt.Errorf("streaming entry: %w", err)
				}
			} else if err := entryWriter.WriteEntry(e); err != nil {
				return nil, fmt.Errorf("writing entry: %w", err)
			}
			lastEntry = e
			haveLastEntry = true
		}

		// Snapshot tallies as of this completed chunk for the next pack-seal
		// checkpoint (which must exclude its own triggering chunk).
		if p.CheckpointFn != nil {
			prefix = Stats{
				TotalChunks: result.TotalChunks, RawBytes: result.RawBytes,
				UniqueChunks: result.UniqueChunks, DedupChunks: result.DedupChunks,
				StoredBytes: storedBytes,
			}
		}
	}

	// Flush and close the entries sidecar before saving the manifest.
	if entryWriter != nil {
		if err := entryWriter.Close(); err != nil {
			return nil, fmt.Errorf("closing entry writer: %w", err)
		}
	}

	// Check for worker errors (e.g. encryption failure).
	if errPtr := workerErr.Load(); errPtr != nil {
		return nil, fmt.Errorf("hash worker: %w", *errPtr)
	}

	// Wait for chunker to finish
	if err := <-chunkerDone; err != nil {
		// A resumable backup interrupted (parent context cancelled by
		// SIGINT/SIGTERM) after at least one durable checkpoint is SUSPENDED,
		// not failed: preserve its sealed packs and entries sidecar so
		// re-running resumes from the checkpoint. The deferred cleanups honor
		// `suspended` (sidecar kept; CloseDiscard keeps packs).
		if p.Resumable && checkpointWritten && parentCtx.Err() != nil {
			suspended = true
			return nil, ErrSuspended
		}
		return nil, err
	}

	// Stop progress reporting and wait for the goroutine to exit.
	if progressStop != nil {
		close(progressStop)
		<-progressDone
		// Send a final update after the goroutine has stopped. Both hooks are
		// checked: progressStop being non-nil no longer implies OnProgress is
		// set, because OnActivity can start the goroutine on its own.
		if p.OnActivity != nil {
			p.OnActivity(bytesProcessed.Load())
		}
		if p.OnProgress != nil {
			p.OnProgress(ProgressInfo{
				BytesProcessed: bytesProcessed.Load(),
				TotalBytes:     totalSize,
				ChunksTotal:    atomicTotalChunks.Load(),
				ChunksNew:      atomicUniqueChunks.Load(),
				ChunksDedup:    atomicDedupChunks.Load(),
				StoredBytes:    atomicStoredBytes.Load(),
				Elapsed:        time.Since(start),
			})
		}
	}

	// Compute result stats
	result.Duration = time.Since(start)
	result.StoredBytes = storedBytes
	if result.TotalChunks > 0 {
		result.MinChunkSize = minChunk
		result.MaxChunkSize = maxChunk
	}
	if result.TotalChunks > 0 {
		result.AvgChunkSize = int(result.RawBytes / result.TotalChunks)
	}
	if result.RawBytes > 0 {
		result.DedupRatio = 1.0 - float64(result.UniqueChunks)/float64(result.TotalChunks)
		if storedBytes > 0 {
			result.CompRatio = float64(result.RawBytes) / float64(storedBytes)
		}
	}
	result.BackupID = backupID

	// Seal the final pack before saving the manifest. Close syncs the last
	// pack and fires OnPackSealed (for S3 backups this is what uploads it);
	// if that fails, the chunk data is not durably persisted and the backup
	// must fail rather than record a manifest referencing missing data.
	if chunkStore != nil {
		cs := chunkStore
		chunkStore = nil
		if err := cs.Close(); err != nil {
			return nil, fmt.Errorf("sealing final pack: %w", err)
		}
	}

	// Flush the index BEFORE saving the manifest: the manifest is what makes a
	// backup visible to list/restore, and prune deletes any pack chunk absent
	// from the index. If the flush failed after the manifest was saved, the
	// deferred CloseDiscard would drop the session's index inserts while the
	// manifest stayed installed — a listable backup whose chunks the next prune
	// permanently deletes. A flushed index with no manifest is just a harmless
	// orphan. (Same ordering rule cloudsync's UploadResults documents.)
	if !analyzeOnly {
		if err := dedupIdx.Flush(); err != nil {
			return nil, fmt.Errorf("flushing index: %w", err)
		}
	}

	// Save manifest (only for actual backup)
	if !analyzeOnly {
		m := &manifest.Backup{
			BackupID:     backupID,
			Timestamp:    time.Now(),
			SourceVolume: sourceVolume,
			TotalBytes:   totalSize,
			TotalChunks:  result.TotalChunks,
			UniqueChunks: result.UniqueChunks,
			DedupChunks:  result.DedupChunks,
			RawBytes:     result.RawBytes,
			StoredBytes:  result.StoredBytes,
			DedupRatio:   result.DedupRatio,
			CompRatio:    result.CompRatio,
			Duration:     result.Duration.String(),
		}
		// #455: the digest, when this run could compute a complete one. A
		// backup resumed from a pre-digest suspend records none — verify
		// reports it not-verifiable, which is honest; a half-stream fold
		// stamped as whole would fail verification forever.
		if p.digestValid && len(p.digestFinal) > 0 {
			m.ContentDigest = hex.EncodeToString(p.digestFinal)
			m.ContentDigestCovers = manifest.DigestCoversSourceStreamV1
		}
		if p.fileCatalog != nil {
			m.BackupMode = p.fileCatalog.Mode
			m.SourcePaths = p.fileCatalog.SourcePaths
			if p.fileCatalog.SidecarPath != "" {
				m.CatalogSidecarPath = p.fileCatalog.SidecarPath
			} else {
				m.FileCatalog = p.fileCatalog.Files
			}
		}
		if p.StampManifest != nil {
			p.StampManifest(m)
		}
		if p.StreamManifest != nil {
			// Lever 4: the caller finalizes the streamed manifest (sets any
			// wrapped DEK, emits the tail part, requests server-side compose).
			if err := p.FinishManifest(m); err != nil {
				return nil, fmt.Errorf("finishing streamed manifest: %w", err)
			}
		} else if err := m.Save(repoPath); err != nil {
			return nil, fmt.Errorf("saving manifest: %w", err)
		}
	}

	// Only now may the deferred close persist index state (the flush itself
	// happened before the manifest save above).
	backupSucceeded = true
	return &result, nil
}

// BackupIncremental runs a backup with a parent reference, computing change stats
// against the parent manifest. The dedup index already prevents re-storing known chunks,
// so the pipeline itself is identical — we just record lineage and diff stats.
func (p *Pipeline) BackupIncremental(ctx context.Context, reader io.Reader, sourceVolume string, totalSize int64, repoPath string, parentBackupID string) (*Result, error) {
	// Run the normal backup pipeline — dedup handles everything; parent lineage
	// is stamped afterward.
	result, err := p.run(ctx, reader, sourceVolume, totalSize, repoPath, false)
	if err != nil {
		return nil, err
	}

	changed, unchanged, err := ApplyParentLineage(repoPath, result.BackupID, parentBackupID)
	if err != nil {
		return nil, err
	}
	result.ParentBackupID = parentBackupID
	result.ChangedChunks = changed
	result.UnchangedChunks = unchanged
	return result, nil
}

// ApplyParentLineage stamps incremental lineage onto a just-completed backup's
// manifest: it marks the parent, sets BackupType=incremental, and records
// approximate changed/unchanged chunk counts (via a bloom of the parent's chunk
// hashes). It runs the same whether the backup ran start-to-finish or resumed
// from a checkpoint (#54), so both paths share it.
func ApplyParentLineage(repoPath, backupID, parentBackupID string) (changed, unchanged int64, err error) {
	parentEA, parentClose, err := manifest.NewEntryAccessor(repoPath, parentBackupID)
	if err != nil {
		parent, err2 := manifest.Load(repoPath, parentBackupID)
		if err2 != nil {
			return 0, 0, fmt.Errorf("loading parent manifest %s: %w", parentBackupID, err2)
		}
		parentEA = manifest.NewSliceEntryAccessor(parent.Entries)
		parentClose = nopCloser{}
	}
	n := uint64(parentEA.Count())
	if n == 0 {
		n = 1
	}
	parentBloom := index.NewBloomFilter(n, 0.01)
	for i := int64(0); i < parentEA.Count(); i++ {
		e, berr := parentEA.At(i)
		if berr != nil {
			break
		}
		parentBloom.Add(binary.LittleEndian.Uint64(e.ChunkHash[:8]))
	}
	parentClose.Close()

	newManifest, err := manifest.Load(repoPath, backupID)
	if err != nil {
		return 0, 0, fmt.Errorf("loading new manifest: %w", err)
	}
	for _, e := range newManifest.Entries {
		if parentBloom.MayContain(binary.LittleEndian.Uint64(e.ChunkHash[:8])) {
			unchanged++
		} else {
			changed++
		}
	}
	newManifest.ParentBackupID = parentBackupID
	newManifest.BackupType = "incremental"
	newManifest.ChangedChunks = changed
	newManifest.UnchangedChunks = unchanged
	if err := newManifest.Save(repoPath); err != nil {
		return 0, 0, fmt.Errorf("updating manifest with incremental info: %w", err)
	}
	return changed, unchanged, nil
}
