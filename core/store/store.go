// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/klauspost/compress/zstd"
)

// PackEntry describes a single chunk's location within a pack file.
type PackEntry struct {
	Offset         int64 // offset within the pack file
	CompressedSize int   // size on disk after compression
	RawSize        int   // original uncompressed size
}

// ErrChunkFetchDecline: an OnChunkFetch hook returns this to route the
// chunk through the whole-pack path instead (#157: the planner declines
// for DENSE packs, where amortized pack download beats per-chunk ranges).
var ErrChunkFetchDecline = errors.New("chunk fetch declined; use pack path")

// OnPackSealedFunc is called when a pack file is finalized (rotated or on Close).
// It receives the pack's file path, sequence number, and final size in bytes.
type OnPackSealedFunc func(packPath string, packNum uint32, size int64) error

// frameKey identifies a cached frame by pack number and offset.
type frameKey struct {
	PackNum     uint32
	StoreOffset int64
}

// ChunkLoc identifies one stored chunk frame by pack number and offset.
type ChunkLoc struct {
	PackNum     uint32
	StoreOffset int64
}

// ChunkRef is one chunk to fetch in a batch. RawLen (the uncompressed length
// recorded in the dedup index) lets the fetcher size its range read so the
// whole frame arrives in ONE request — the per-chunk path has to read the
// 8-byte header first just to learn the payload length (#204).
type ChunkRef struct {
	ChunkLoc
	RawLen int
}

// ChunkStore manages deduplicated chunk storage in pack files.
type ChunkStore struct {
	mu sync.Mutex

	dir              string
	maxPackSize      int64
	compressionLevel int

	currentPack    *os.File
	currentPackNum uint32
	currentOffset  int64

	encoder *zstd.Encoder
	decoder *zstd.Decoder

	key *crypto.MasterKey // nil for unencrypted repos

	frameCache sync.Map // map[frameKey][]byte — pre-fetched raw frames
	// frameCacheBytes/frameOrder bound the cache (#482): it retained every
	// fetched frame for the process's life, and a full verify walking
	// scattered dedup references retained one frame per ENTRY — the RSS
	// ratchet that OOMkilled the 512Mi controller under a healthy GC
	// sawtooth. FIFO eviction under frameCacheMu; sync.Map stays the read
	// path so Retrieve's hot lookups are untouched.
	frameCacheMu    sync.Mutex
	frameCacheBytes int64
	frameOrder      []frameKey

	OnPackSealed  OnPackSealedFunc
	OnPackMissing func(packNum uint32) error // called when a pack file is not found during Retrieve
	// OnPackAccess (#153): observes every successful local pack open, so an
	// LRU pack cache can refresh recency on HITS (misses go via
	// OnPackMissing). Must be cheap; called on the read path.
	OnPackAccess func(packNum uint32)
	// OnChunkFetch (#153): fetch ONE chunk's frame (header+payload) without
	// materializing the whole pack — an S3 range GET in production. When
	// set, it takes precedence over OnPackMissing for absent packs, keeping
	// recovery's disk/RAM footprint independent of pack size.
	OnChunkFetch func(packNum uint32, offset int64) ([]byte, error)
	// OnChunkFetchBatch (#204): fetch MANY chunk frames in one pack-grouped,
	// range-coalesced round trip — the restore path's default, since fetching
	// chunk-by-chunk costs two presigned requests EACH. Frames come back keyed
	// by location; a key the fetcher could not supply is simply absent and the
	// caller falls back to Retrieve (so --ignore-error keeps its per-file
	// granularity and a partial batch never corrupts a restore). Callers own
	// the returned frames, which is what keeps a batch's memory bounded — this
	// hook must NOT populate the frame cache.
	OnChunkFetchBatch func(refs []ChunkRef) (map[ChunkLoc][]byte, error)
}

// CanBatchFetch reports whether batched frame prefetching is wired.
func (cs *ChunkStore) CanBatchFetch() bool { return cs.OnChunkFetchBatch != nil }

// FetchBatch fetches many chunk frames in one round trip via OnChunkFetchBatch.
// It returns (nil, nil) when no batch fetcher is wired. The result may cover
// only some of the requested refs; missing ones must be read individually.
func (cs *ChunkStore) FetchBatch(refs []ChunkRef) (map[ChunkLoc][]byte, error) {
	if cs.OnChunkFetchBatch == nil || len(refs) == 0 {
		return nil, nil
	}
	return cs.OnChunkFetchBatch(refs)
}

// HasFrame reports whether a chunk's frame is already in the frame cache, so
// batch planners skip chunks a bulk pre-fetch already paid for.
func (cs *ChunkStore) HasFrame(packNum uint32, offset int64) bool {
	_, ok := cs.frameCache.Load(frameKey{packNum, offset})
	return ok
}

// maxChunkFrameLen bounds a single frame's stated payload length before any
// allocation. Chunks top out at ChunkMaxSize (default 512KB) plus small
// compression/encryption overhead, and whole packs at 128MB — a header
// claiming more than 64MB is a corrupt pack stating garbage, and the four
// flipped bytes must produce a refusal, not a 4GB make([]byte) (the OOM
// class the corrupted-pack restore tests exposed).
const maxChunkFrameLen = 64 << 20

// NewChunkStore creates or opens a chunk store in the given directory.
// An optional MasterKey enables AES-256-GCM encryption of chunk payloads.
func NewChunkStore(dir string, maxPackSize int64, compressionLevel int, key ...*crypto.MasterKey) (*ChunkStore, error) {
	var k *crypto.MasterKey
	if len(key) > 0 {
		k = key[0]
	}
	cs, err := newChunkStore(dir, maxPackSize, compressionLevel, 0, k)
	if err != nil {
		return nil, err
	}
	// Auto-detect the starting pack number by scanning existing packs.
	cs.currentPackNum = cs.findLatestPack()
	// The pack opens on the FIRST WRITE, not here (#357): a restore builds a
	// store it never writes to, and an eager open left an empty 0000.pack
	// holding the name a concurrently-downloaded real pack needs. POSIX
	// renames over it and hides the mistake; Windows denies the rename.
	return cs, nil
}

// NewChunkStoreAt creates a ChunkStore starting at the given pack number.
// Used for S3-backed pipelines where pack numbering continues from the
// remote pack_names.json count.
func NewChunkStoreAt(dir string, maxPackSize int64, compressionLevel int, startPack uint32, key ...*crypto.MasterKey) (*ChunkStore, error) {
	var k *crypto.MasterKey
	if len(key) > 0 {
		k = key[0]
	}
	cs, err := newChunkStore(dir, maxPackSize, compressionLevel, startPack, k)
	if err != nil {
		return nil, err
	}
	// Opened on first write; see NewChunkStore.
	return cs, nil
}

// newChunkStore is the shared constructor used by NewChunkStore and NewChunkStoreAt.
func newChunkStore(dir string, maxPackSize int64, compressionLevel int, startPack uint32, key *crypto.MasterKey) (*ChunkStore, error) {
	chunksDir := filepath.Join(dir, "chunks")
	if err := os.MkdirAll(chunksDir, 0755); err != nil {
		return nil, fmt.Errorf("creating chunks dir: %w", err)
	}

	// The integer collapses onto four presets; see compression.go, which is
	// also what the controller derives the panel's choices from.
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(CompressionPreset(compressionLevel)))
	if err != nil {
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}

	decoder, err := zstd.NewReader(nil)
	if err != nil {
		encoder.Close()
		return nil, fmt.Errorf("creating zstd decoder: %w", err)
	}

	cs := &ChunkStore{
		dir:              dir,
		maxPackSize:      maxPackSize,
		compressionLevel: compressionLevel,
		currentPackNum:   startPack,
		encoder:          encoder,
		decoder:          decoder,
		key:              key,
	}
	return cs, nil
}

// Store compresses and writes a chunk to the current pack file.
// Returns the pack number, offset, and compressed size.
func (cs *ChunkStore) Store(data []byte) (packNum uint32, offset int64, compressedSize int, err error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err := cs.ensureCurrentPack(); err != nil {
		return 0, 0, 0, err
	}

	compressed := cs.encoder.EncodeAll(data, nil)

	// Encrypt the compressed payload if encryption is enabled.
	payload := compressed
	if cs.key != nil {
		encrypted, err := cs.key.EncryptWithAAD(compressed, crypto.AADChunk)
		if err != nil {
			return 0, 0, 0, fmt.Errorf("encrypting chunk: %w", err)
		}
		payload = encrypted
	}

	// Each stored chunk: [4 bytes: payload length][4 bytes: raw length][payload]
	headerSize := 8
	totalSize := int64(headerSize + len(payload))

	// Rotate pack if needed
	if cs.currentOffset+totalSize > cs.maxPackSize && cs.currentOffset > 0 {
		if err := cs.rotatePack(); err != nil {
			return 0, 0, 0, err
		}
	}

	packNum = cs.currentPackNum
	offset = cs.currentOffset

	// Write header
	var header [8]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(header[4:8], uint32(len(data)))
	if _, err := cs.currentPack.Write(header[:]); err != nil {
		return 0, 0, 0, fmt.Errorf("writing chunk header: %w", err)
	}

	// Write payload (compressed, optionally encrypted)
	if _, err := cs.currentPack.Write(payload); err != nil {
		return 0, 0, 0, fmt.Errorf("writing chunk data: %w", err)
	}

	cs.currentOffset += totalSize
	return packNum, offset, len(payload), nil
}

// Retrieve reads and decompresses a chunk from a pack file.
// It checks the frame cache first (populated by CacheFrame).
func (cs *ChunkStore) Retrieve(packNum uint32, offset int64) ([]byte, error) {
	// Check frame cache first (populated by batch range-read fetcher).
	// Use Load (not LoadAndDelete) because the same chunk may be
	// referenced multiple times by different manifest entries (dedup).
	key := frameKey{packNum, offset}
	if cached, ok := cs.frameCache.Load(key); ok {
		return cs.RetrieveFromFrame(cached.([]byte))
	}

	// Chunk-granular fetch (#153): when the pack's bytes aren't available
	// locally — file missing, OR the eagerly-created EMPTY current pack
	// (NewChunkStore opens one, so an existence check alone would skip the
	// fetch for pack 0 and fall back to a whole-pack download) — fetch just
	// this chunk's frame.
	chunkFetch := func() ([]byte, bool, error) {
		frame, err := cs.OnChunkFetch(packNum, offset)
		if errors.Is(err, ErrChunkFetchDecline) {
			return nil, false, nil // planner says: this pack is dense, download it
		}
		if err != nil {
			return nil, true, fmt.Errorf("fetching chunk (pack %d offset %d): %w", packNum, offset, err)
		}
		cs.CacheFrame(packNum, offset, frame)
		data, err := cs.RetrieveFromFrame(frame)
		return data, true, err
	}
	if cs.OnChunkFetch != nil && !cs.packFileExists(packNum) {
		if data, handled, err := chunkFetch(); handled {
			return data, err
		}
	}

	f, err := cs.openPackForRead(packNum)
	if err != nil {
		if cs.OnChunkFetch != nil {
			if data, handled, ferr := chunkFetch(); handled {
				return data, ferr
			}
		}
		return nil, err
	}
	defer f.Close()

	// Read header
	var header [8]byte
	if _, err := f.ReadAt(header[:], offset); err != nil {
		if err == io.EOF && cs.OnChunkFetch != nil {
			if data, handled, ferr := chunkFetch(); handled {
				f.Close()
				return data, ferr
			}
		}
		if err == io.EOF && cs.OnPackMissing != nil {
			f.Close()
			f, err = cs.fetchAndOpenPack(packNum)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			if _, err := f.ReadAt(header[:], offset); err != nil {
				return nil, fmt.Errorf("reading chunk header at offset %d: %w", offset, err)
			}
		} else {
			return nil, fmt.Errorf("reading chunk header at offset %d: %w", offset, err)
		}
	}

	payloadLen := binary.LittleEndian.Uint32(header[0:4])
	if payloadLen > maxChunkFrameLen {
		return nil, fmt.Errorf("chunk frame at pack %d offset %d claims %d bytes, exceeds the %d-byte bound: corrupt pack", packNum, offset, payloadLen, maxChunkFrameLen)
	}

	// Read payload
	payload := make([]byte, payloadLen)
	if _, err := f.ReadAt(payload, offset+8); err != nil {
		return nil, fmt.Errorf("reading chunk data: %w", err)
	}

	// Decrypt if encryption is enabled
	compressed := payload
	if cs.key != nil {
		decrypted, err := cs.key.DecryptWithAAD(payload, crypto.AADChunk)
		if err != nil {
			return nil, fmt.Errorf("decrypting chunk: %w", err)
		}
		compressed = decrypted
	}

	// Decompress
	data, err := cs.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing chunk: %w", err)
	}

	return data, nil
}

// StoreRaw writes a pre-compressed frame (8-byte header + compressed data)
// directly to the current pack file without re-compressing.
// Returns the pack number, offset, and compressed size (excluding header).
func (cs *ChunkStore) StoreRaw(frame []byte) (packNum uint32, offset int64, compressedSize int, err error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if err := cs.ensureCurrentPack(); err != nil {
		return 0, 0, 0, err
	}

	totalSize := int64(len(frame))

	// Rotate pack if needed.
	if cs.currentOffset+totalSize > cs.maxPackSize && cs.currentOffset > 0 {
		if err := cs.rotatePack(); err != nil {
			return 0, 0, 0, err
		}
	}

	packNum = cs.currentPackNum
	offset = cs.currentOffset

	if _, err := cs.currentPack.Write(frame); err != nil {
		return 0, 0, 0, fmt.Errorf("writing raw chunk frame: %w", err)
	}

	cs.currentOffset += totalSize
	compressedSize = len(frame) - 8 // exclude 8-byte header
	return packNum, offset, compressedSize, nil
}

// RetrieveRaw reads the raw compressed frame (8-byte header + compressed data)
// from a pack file without decompressing. Returns the raw frame and the
// original uncompressed size from the header.
func (cs *ChunkStore) RetrieveRaw(packNum uint32, offset int64) (frame []byte, rawSize uint32, err error) {
	packPath := cs.packPath(packNum)

	f, err := os.Open(packPath)
	if err != nil {
		return nil, 0, fmt.Errorf("opening pack %d: %w", packNum, err)
	}
	defer f.Close()

	// Read header
	var header [8]byte
	if _, err := f.ReadAt(header[:], offset); err != nil {
		return nil, 0, fmt.Errorf("reading chunk header at offset %d: %w", offset, err)
	}

	compressedLen := binary.LittleEndian.Uint32(header[0:4])
	rawSize = binary.LittleEndian.Uint32(header[4:8])
	if compressedLen > maxChunkFrameLen {
		return nil, 0, fmt.Errorf("chunk frame at pack %d offset %d claims %d bytes, exceeds the %d-byte bound: corrupt pack", packNum, offset, compressedLen, maxChunkFrameLen)
	}

	// Read entire frame: header + compressed data
	frame = make([]byte, 8+compressedLen)
	copy(frame[:8], header[:])
	if _, err := f.ReadAt(frame[8:], offset+8); err != nil {
		return nil, 0, fmt.Errorf("reading chunk data: %w", err)
	}

	return frame, rawSize, nil
}

// RetrieveFromFrame decrypts and decompresses a chunk from a raw frame
// (the same format stored in pack files: 4B payloadLen + 4B rawLen + payload).
func (cs *ChunkStore) RetrieveFromFrame(frame []byte) ([]byte, error) {
	if len(frame) < 8 {
		return nil, fmt.Errorf("frame too short: %d bytes", len(frame))
	}

	payloadLen := binary.LittleEndian.Uint32(frame[0:4])
	if int(payloadLen)+8 > len(frame) {
		return nil, fmt.Errorf("frame payload length %d exceeds frame size %d", payloadLen, len(frame))
	}

	payload := frame[8 : 8+payloadLen]

	// Decrypt if encryption is enabled.
	compressed := payload
	if cs.key != nil {
		decrypted, err := cs.key.DecryptWithAAD(payload, crypto.AADChunk)
		if err != nil {
			return nil, fmt.Errorf("decrypting chunk from frame: %w", err)
		}
		compressed = decrypted
	}

	// Decompress.
	data, err := cs.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("decompressing chunk from frame: %w", err)
	}

	return data, nil
}

// CacheFrame stores a raw frame for later retrieval by Retrieve.
// Used by the batch range-read fetcher to pre-populate chunks
// before the restore loop reads them.
// DropFrames evicts all cached frames belonging to a pack. Cloud verify calls
// this after finishing a pack so the frame cache holds at most one pack's frames
// at a time (a --full verify would otherwise accumulate the whole repo in RAM).
func (cs *ChunkStore) DropFrames(packNum uint32) {
	cs.frameCacheMu.Lock()
	defer cs.frameCacheMu.Unlock()
	kept := cs.frameOrder[:0]
	for _, fk := range cs.frameOrder {
		if fk.PackNum != packNum {
			kept = append(kept, fk)
			continue
		}
		if v, ok := cs.frameCache.LoadAndDelete(fk); ok {
			cs.frameCacheBytes -= int64(len(v.([]byte)))
		}
	}
	cs.frameOrder = kept
}

func (cs *ChunkStore) CacheFrame(packNum uint32, offset int64, frame []byte) {
	k := frameKey{packNum, offset}
	if _, loaded := cs.frameCache.LoadOrStore(k, frame); loaded {
		return // already cached; no accounting change
	}
	cs.frameCacheMu.Lock()
	cs.frameCacheBytes += int64(len(frame))
	cs.frameOrder = append(cs.frameOrder, k)
	// FIFO under the ceiling: the cache serves dedup re-reads within a
	// batch window, so the frames worth keeping are the recent ones.
	for cs.frameCacheBytes > FrameCacheMaxBytes && len(cs.frameOrder) > 1 {
		old := cs.frameOrder[0]
		cs.frameOrder = cs.frameOrder[1:]
		if v, ok := cs.frameCache.LoadAndDelete(old); ok {
			cs.frameCacheBytes -= int64(len(v.([]byte)))
		}
	}
	cs.frameCacheMu.Unlock()
}

// FrameCacheMaxBytes is the frame cache's ceiling (#482). 64 MB holds
// several batch windows of even large-chunk frames; what it must never hold
// is one frame per entry of a scattered full verify.
const FrameCacheMaxBytes = 64 << 20

// FrameCacheBytes reports the cache's current retained size.
func (cs *ChunkStore) FrameCacheBytes() int64 {
	cs.frameCacheMu.Lock()
	defer cs.frameCacheMu.Unlock()
	return cs.frameCacheBytes
}

// PackPath returns the file path for the given pack number.
func (cs *ChunkStore) PackPath(num uint32) string {
	return cs.packPath(num)
}

// Flush syncs the current pack file.
func (cs *ChunkStore) Flush() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	if cs.currentPack != nil {
		return cs.currentPack.Sync()
	}
	return nil
}

// Close closes the chunk store.
func (cs *ChunkStore) Close() error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	cs.encoder.Close()
	cs.decoder.Close()

	if cs.currentPack != nil {
		if err := cs.currentPack.Sync(); err != nil {
			return fmt.Errorf("syncing final pack: %w", err)
		}
		if err := cs.currentPack.Close(); err != nil {
			return fmt.Errorf("closing final pack: %w", err)
		}
		_ = syncDir(cs.chunksDir()) // best-effort (see rotatePack)
		if cs.OnPackSealed != nil && cs.currentOffset > 0 {
			if err := cs.OnPackSealed(
				cs.packPath(cs.currentPackNum), cs.currentPackNum, cs.currentOffset,
			); err != nil {
				return fmt.Errorf("OnPackSealed final pack %d: %w", cs.currentPackNum, err)
			}
		}
	}
	return nil
}

// StoreStats returns storage statistics.
type StoreStats struct {
	PackFiles        int
	CurrentPackNum   uint32
	CurrentPackBytes int64
	TotalBytes       int64
}

// Stats returns storage statistics.
func (cs *ChunkStore) Stats() StoreStats {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	var totalBytes int64
	chunksDir := filepath.Join(cs.dir, "chunks")
	entries, _ := os.ReadDir(chunksDir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			totalBytes += info.Size()
		}
	}

	return StoreStats{
		PackFiles:        len(entries),
		CurrentPackNum:   cs.currentPackNum,
		CurrentPackBytes: cs.currentOffset,
		TotalBytes:       totalBytes,
	}
}

// packFileExists reports whether the pack is present locally.
func (cs *ChunkStore) packFileExists(packNum uint32) bool {
	_, err := os.Stat(cs.packPath(packNum))
	return err == nil
}

// openPackForRead opens a pack file for reading, calling OnPackMissing if it doesn't exist.
func (cs *ChunkStore) openPackForRead(packNum uint32) (*os.File, error) {
	packPath := cs.packPath(packNum)
	f, err := os.Open(packPath)
	if err != nil && os.IsNotExist(err) && cs.OnPackMissing != nil {
		return cs.fetchAndOpenPack(packNum)
	}
	if err != nil {
		return nil, fmt.Errorf("opening pack %d: %w", packNum, err)
	}
	if cs.OnPackAccess != nil {
		cs.OnPackAccess(packNum)
	}
	return f, nil
}

// fetchAndOpenPack calls OnPackMissing and then re-opens the pack file.
func (cs *ChunkStore) fetchAndOpenPack(packNum uint32) (*os.File, error) {
	if err := cs.OnPackMissing(packNum); err != nil {
		return nil, fmt.Errorf("OnPackMissing pack %d: %w", packNum, err)
	}
	f, err := os.Open(cs.packPath(packNum))
	if err != nil {
		return nil, fmt.Errorf("opening pack %d after download: %w", packNum, err)
	}
	return f, nil
}

func (cs *ChunkStore) packPath(num uint32) string {
	return filepath.Join(cs.dir, "chunks", fmt.Sprintf("%04d.pack", num))
}

// parsePackFileName returns the numeric pack id for names like "0002.pack" or
// "10000.pack" (pack numbers past 9999 use more than 4 digits). Names with
// extra prefixes or suffixes such as "0002.pack.tmp" are rejected.
func parsePackFileName(name string) (uint32, bool) {
	numStr, ok := strings.CutSuffix(name, ".pack")
	if !ok || numStr == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(numStr, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}

// packsGenerationFile marks the "generation" of a repo's pack layout. Prune
// bumps it after renumbering packs, so resume checkpoints (which record the
// generation they were written under) can detect that their segment-replayed
// pack references are stale and fall back to an index rebuild (#55/#56).
const packsGenerationFile = ".generation"

// PacksGeneration returns the current pack-layout generation ("" for a repo
// that has never been pruned).
func PacksGeneration(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, "chunks", packsGenerationFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteGenerationFile writes a fresh pack-layout generation into the given
// chunks directory. Prune writes it into its STAGING chunks dir before the
// atomic swap, so the new generation is installed atomically WITH the
// renumbered packs — a crash around the swap can never leave new packs under
// an old (or empty) generation, which would let a suspended backup's fast-path
// resume replay stale pack references.
func WriteGenerationFile(chunksDir string) error {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(chunksDir, packsGenerationFile), []byte(hex.EncodeToString(b[:])), 0644)
}

// BumpPacksGeneration records a new pack-layout generation on a live repo.
func BumpPacksGeneration(repoPath string) error {
	return WriteGenerationFile(filepath.Join(repoPath, "chunks"))
}

// MaxPackNum returns the highest pack number present in the repo and whether any
// pack exists. Resume uses it to append new packs after all existing ones,
// independent of the checkpoint's recorded pack number — which a prune (that
// renumbers packs) would otherwise invalidate.
func MaxPackNum(repoPath string) (uint32, bool, error) {
	chunksDir := filepath.Join(repoPath, "chunks")
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	var max uint32
	var found bool
	for _, e := range entries {
		if n, ok := parsePackFileName(e.Name()); ok {
			if !found || n > max {
				max = n
			}
			found = true
		}
	}
	return max, found, nil
}

// DeletePacksAbove removes pack files whose number is greater than keepMax.
// Resume uses it to discard an interrupted run's un-checkpointed tail packs
// before rebuilding the index from the surviving packs (#42). Because pack
// numbering is monotonic and a resumable backup holds the repo backup lock,
// packs above the checkpoint's last-sealed pack belong exclusively to that run.
func DeletePacksAbove(repoPath string, keepMax uint32) error {
	chunksDir := filepath.Join(repoPath, "chunks")
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if n, ok := parsePackFileName(e.Name()); ok && n > keepMax {
			if err := os.Remove(filepath.Join(chunksDir, e.Name())); err != nil {
				return fmt.Errorf("removing pack %s: %w", e.Name(), err)
			}
		}
	}
	return nil
}

func (cs *ChunkStore) findLatestPack() uint32 {
	chunksDir := filepath.Join(cs.dir, "chunks")
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		return 0
	}

	var max uint32
	for _, e := range entries {
		if n, ok := parsePackFileName(e.Name()); ok && n >= max {
			max = n
		}
	}
	return max
}

// syncDir fsyncs a directory so that a newly created or renamed directory
// entry (e.g. a freshly sealed pack file) is itself durable — fsyncing a
// file's contents does not commit the parent directory's entry for it. It is a
// package var so tests can observe seal-time directory syncs and inject faults.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// chunksDir returns the directory holding this repo's pack files.
func (cs *ChunkStore) chunksDir() string {
	return filepath.Join(cs.dir, "chunks")
}

// ensureCurrentPack opens the pack this store appends to, if it has not been
// opened yet. Callers hold cs.mu.
func (cs *ChunkStore) ensureCurrentPack() error {
	if cs.currentPack != nil {
		return nil
	}
	return cs.openCurrentPack()
}

func (cs *ChunkStore) openCurrentPack() error {
	path := cs.packPath(cs.currentPackNum)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return fmt.Errorf("opening pack file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat pack file: %w", err)
	}

	// Seek to end for appending
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return fmt.Errorf("seeking pack end: %w", err)
	}

	cs.currentPack = f
	cs.currentOffset = info.Size()
	return nil
}

func (cs *ChunkStore) rotatePack() error {
	if err := cs.currentPack.Sync(); err != nil {
		return fmt.Errorf("syncing pack: %w", err)
	}
	if err := cs.currentPack.Close(); err != nil {
		return fmt.Errorf("closing pack: %w", err)
	}
	// Commit the sealed pack's directory entry before anyone (OnPackSealed, a
	// resume checkpoint) records that this pack is durable. Best-effort: some
	// platforms/filesystems reject fsync on a directory handle (e.g. Windows
	// FlushFileBuffers returns ACCESS_DENIED), and a seal must not fail there.
	_ = syncDir(cs.chunksDir())

	if cs.OnPackSealed != nil {
		if err := cs.OnPackSealed(
			cs.packPath(cs.currentPackNum), cs.currentPackNum, cs.currentOffset,
		); err != nil {
			return fmt.Errorf("OnPackSealed pack %d: %w", cs.currentPackNum, err)
		}
	}

	cs.currentPackNum++
	return cs.openCurrentPack()
}

// EncryptionMode specifies how a repository is encrypted.
type EncryptionMode string

const (
	EncryptNone       EncryptionMode = ""           // no encryption
	EncryptPassphrase EncryptionMode = "passphrase" // passphrase-based AES-256-GCM
	EncryptManaged    EncryptionMode = "managed"    // server-managed X25519 keypair
)

// RepoConfig is written to config.json in the repository root.
type RepoConfig struct {
	Version          int            `json:"version"`
	ChunkMinSize     int            `json:"chunk_min_size"`
	ChunkAvgSize     int            `json:"chunk_avg_size"`
	ChunkMaxSize     int            `json:"chunk_max_size"`
	BuzhashMask      uint64         `json:"buzhash_mask"`
	PackFileMaxSize  int64          `json:"pack_file_max_size"`
	CompressionLevel int            `json:"compression_level"`
	Encrypted        bool           `json:"encrypted,omitempty"`
	EncryptionMode   EncryptionMode `json:"encryption_mode,omitempty"`

	// Normalizers records the chunk normalizers applied before hashing, as
	// stable names (see preprocess.Names). It is a repo-wide, immutable
	// setting: chunk identity is the hash of normalized bytes while original
	// bytes are stored, and the dedup index is shared across all backups, so
	// every read path (restore, verify, index rebuild, prune) must apply the
	// same normalizer. Empty means no normalization.
	Normalizers []string `json:"normalizers,omitempty"`
}

// EffectiveEncryptionMode returns the encryption mode, accounting for v1
// configs that only have the Encrypted bool.
func (rc RepoConfig) EffectiveEncryptionMode() EncryptionMode {
	if rc.EncryptionMode != "" {
		return rc.EncryptionMode
	}
	if rc.Encrypted {
		return EncryptPassphrase
	}
	return EncryptNone
}

// InitRepo initializes a new backup repository.
// RepoExists reports whether repoPath already contains a config.json.
func RepoExists(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, "config.json"))
	return err == nil
}

func InitRepo(repoPath string, cfg RepoConfig) error {
	cfg.Version = 1

	dirs := []string{
		filepath.Join(repoPath, "chunks"),
		filepath.Join(repoPath, "index"),
		filepath.Join(repoPath, "manifests"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	configPath := filepath.Join(repoPath, "config.json")
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// LoadRepoConfig reads the repository configuration.
func LoadRepoConfig(repoPath string) (RepoConfig, error) {
	configPath := filepath.Join(repoPath, "config.json")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return RepoConfig{}, fmt.Errorf("reading repo config: %w", err)
	}

	var cfg RepoConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return RepoConfig{}, fmt.Errorf("parsing repo config: %w", err)
	}
	return cfg, nil
}

// SaveRepoConfig atomically rewrites the repository configuration. Used to
// record repo-wide settings established on first use (e.g. the normalizer).
func SaveRepoConfig(repoPath string, cfg RepoConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	configPath := filepath.Join(repoPath, "config.json")
	tmp := configPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	if err := os.Rename(tmp, configPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replacing config: %w", err)
	}
	return nil
}
