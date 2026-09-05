// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/klauspost/compress/zstd"
)

// RebuildOptions controls which index components are rebuilt and how.
type RebuildOptions struct {
	RepoPath          string
	RebuildBloom      bool
	RebuildHashIndex  bool
	Key               *crypto.MasterKey // nil for unencrypted repos
	BloomFPRate       float64           // default 0.001 when zero
	DeltaFlushEvery   int               // default 500_000 when zero
	ExpectedChunks    uint64            // skip pre-scan when non-zero
	OnProgress        func(packs, chunksScanned int)
	OnPreScanProgress func(packs, chunksFound int) // called after each pack during pre-scan
}

// RebuildResult summarises the completed rebuild.
type RebuildResult struct {
	PacksScanned  int
	ChunksScanned uint64
	Elapsed       time.Duration
}

// Rebuild reconstructs the dedup index by streaming pack files from disk.
// It requires no running backup session and uses bounded RAM via delta flushing.
func Rebuild(ctx context.Context, opts RebuildOptions) (RebuildResult, error) {
	start := time.Now()

	// Apply defaults.
	fpRate := opts.BloomFPRate
	if fpRate == 0 {
		fpRate = 0.001
	}
	deltaFlushEvery := opts.DeltaFlushEvery
	if deltaFlushEvery == 0 {
		deltaFlushEvery = 500_000
	}

	// Prerequisite checks.
	if !store.RepoExists(opts.RepoPath) {
		return RebuildResult{}, fmt.Errorf("repository not found at %s", opts.RepoPath)
	}

	repoCfg, err := store.LoadRepoConfig(opts.RepoPath)
	if err != nil {
		return RebuildResult{}, fmt.Errorf("loading repo config: %w", err)
	}
	if repoCfg.EffectiveEncryptionMode() == store.EncryptManaged {
		return RebuildResult{}, fmt.Errorf("index rebuild is not supported for managed-encryption repositories")
	}
	// Chunk identity is the hash of normalized bytes; reconstruct the repo's
	// normalizer so the rebuilt index is keyed the same way the pipeline
	// wrote the manifest entries. Without this, restore's LookupDirect would
	// miss every chunk the normalizer altered.
	normalizer, err := preprocess.FromNames(repoCfg.Normalizers)
	if err != nil {
		return RebuildResult{}, fmt.Errorf("reconstructing normalizer from repo config: %w", err)
	}

	chunksDir := filepath.Join(opts.RepoPath, "chunks")
	indexDir := filepath.Join(opts.RepoPath, "index")

	packs, err := listPackFiles(chunksDir)
	if err != nil {
		return RebuildResult{}, fmt.Errorf("listing pack files: %w", err)
	}
	if len(packs) == 0 {
		return RebuildResult{}, fmt.Errorf("no pack files found in %s", chunksDir)
	}

	// Warn if target files already exist.
	if opts.RebuildBloom {
		bloomPath := filepath.Join(indexDir, "bloom.bin")
		for _, p := range []string{bloomPath, bloomPath + ".enc"} {
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(os.Stderr, "warning: overwriting existing %s\n", filepath.Base(p))
			}
		}
	}
	if opts.RebuildHashIndex {
		indexPath := filepath.Join(indexDir, "hash-index.db")
		for _, p := range []string{indexPath, indexPath + ".enc"} {
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(os.Stderr, "warning: overwriting existing %s\n", filepath.Base(p))
			}
		}
	}

	// Determine total chunk count for bloom sizing.
	totalChunks := opts.ExpectedChunks
	if totalChunks == 0 && opts.RebuildBloom {
		fmt.Fprintln(os.Stderr, "Pre-scanning packs to size bloom filter...")
		totalChunks, err = countPackChunks(ctx, chunksDir, opts.OnPreScanProgress)
		if err != nil {
			return RebuildResult{}, fmt.Errorf("pre-scanning packs: %w", err)
		}
		fmt.Fprintf(os.Stderr, "Found %d chunks across %d pack files.\n", totalChunks, len(packs))
	}

	// Create a fresh zstd decoder for this rebuild.
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return RebuildResult{}, fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer decoder.Close()

	// Set up bloom filter (fresh).
	var bloom *BloomFilter
	if opts.RebuildBloom {
		if totalChunks == 0 {
			totalChunks = 1_000_000 // safe minimum when skipping pre-scan
		}
		bloom = NewBloomFilter(totalChunks, fpRate)
	}

	// Set up hash index (fresh — remove stale file first).
	var hashIdx *HashIndex
	if opts.RebuildHashIndex {
		indexPath := filepath.Join(indexDir, "hash-index.db")
		if err := os.MkdirAll(indexDir, 0755); err != nil {
			return RebuildResult{}, fmt.Errorf("creating index dir: %w", err)
		}
		// Remove stale db so NewHashIndex starts empty.
		os.Remove(indexPath)
		hashIdx, err = NewHashIndex(indexPath, 0, false)
		if err != nil {
			return RebuildResult{}, fmt.Errorf("opening hash index: %w", err)
		}
	}

	// Main streaming pass.
	var chunksScanned uint64
	for packIdx, packPath := range packs {
		select {
		case <-ctx.Done():
			if hashIdx != nil {
				hashIdx.Close()
			}
			return RebuildResult{}, ctx.Err()
		default:
		}

		packNum, parseErr := parsePackNumber(packPath)
		if parseErr != nil {
			return RebuildResult{}, fmt.Errorf("parsing pack number from %s: %w", packPath, parseErr)
		}
		err := streamPack(ctx, packPath, packNum, opts.Key, decoder, func(pNum uint32, offset int64, rawLen uint32, raw []byte) error {
			id := hasher.Sum(preprocess.IdentityHashInput(normalizer, raw))
			if bloom != nil {
				bloom.Add(id.WeakHash)
			}
			if hashIdx != nil {
				hashIdx.Insert(id, pNum, uint64(offset), rawLen)
			}
			chunksScanned++
			if hashIdx != nil && int(chunksScanned)%deltaFlushEvery == 0 {
				if err := hashIdx.FlushDelta(); err != nil {
					return fmt.Errorf("flushing hash index delta: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			if hashIdx != nil {
				hashIdx.Close()
			}
			return RebuildResult{}, fmt.Errorf("streaming pack %s: %w", filepath.Base(packPath), err)
		}

		if opts.OnProgress != nil {
			opts.OnProgress(packIdx+1, int(chunksScanned))
		}
	}

	// Persist bloom filter.
	if bloom != nil {
		if err := os.MkdirAll(indexDir, 0755); err != nil {
			return RebuildResult{}, fmt.Errorf("creating index dir: %w", err)
		}
		bloomPath := filepath.Join(indexDir, "bloom.bin")
		if err := bloom.Save(bloomPath); err != nil {
			return RebuildResult{}, fmt.Errorf("saving bloom filter: %w", err)
		}
		if opts.Key != nil {
			if err := crypto.EncryptFile(opts.Key, bloomPath, bloomPath+".enc"); err != nil {
				return RebuildResult{}, fmt.Errorf("encrypting bloom filter: %w", err)
			}
			os.Remove(bloomPath)
		}
	}

	// Persist hash index.
	if hashIdx != nil {
		if err := hashIdx.Flush(); err != nil {
			hashIdx.Close()
			return RebuildResult{}, fmt.Errorf("flushing hash index: %w", err)
		}
		if err := hashIdx.Close(); err != nil {
			return RebuildResult{}, fmt.Errorf("closing hash index: %w", err)
		}
		if opts.Key != nil {
			indexPath := filepath.Join(indexDir, "hash-index.db")
			encPath := indexPath + ".enc"
			if err := crypto.EncryptFile(opts.Key, indexPath, encPath); err != nil {
				return RebuildResult{}, fmt.Errorf("encrypting hash index: %w", err)
			}
			os.Remove(indexPath)
		}
	}

	return RebuildResult{
		PacksScanned:  len(packs),
		ChunksScanned: chunksScanned,
		Elapsed:       time.Since(start),
	}, nil
}

// listPackFiles returns pack file paths under chunksDir in ascending numeric order.
func listPackFiles(chunksDir string) ([]string, error) {
	entries, err := os.ReadDir(chunksDir)
	if err != nil {
		return nil, fmt.Errorf("reading chunks dir: %w", err)
	}

	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pack") {
			paths = append(paths, filepath.Join(chunksDir, e.Name()))
		}
	}

	// Sort numerically: lexicographic order diverges from numeric order once
	// pack numbers exceed 9999 ("10000.pack" < "9999.pack" as strings).
	sort.Slice(paths, func(i, j int) bool {
		ni, erri := parsePackNumber(paths[i])
		nj, errj := parsePackNumber(paths[j])
		if erri != nil || errj != nil {
			return paths[i] < paths[j]
		}
		return ni < nj
	})
	return paths, nil
}

// countPackChunks performs a fast header-only scan to count total frames across
// all pack files. Only the 8-byte frame header is read; the payload is seeked past.
// onProgress (if non-nil) is called after each pack with the running totals.
func countPackChunks(ctx context.Context, chunksDir string, onProgress func(packs, chunks int)) (uint64, error) {
	packs, err := listPackFiles(chunksDir)
	if err != nil {
		return 0, err
	}

	var total uint64
	for i, p := range packs {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		n, err := countFramesInPack(p)
		if err != nil {
			return 0, fmt.Errorf("counting frames in %s: %w", filepath.Base(p), err)
		}
		total += n
		if onProgress != nil {
			onProgress(i+1, int(total))
		}
	}
	return total, nil
}

// countFramesInPack counts frames in a single pack file using header-only reads.
func countFramesInPack(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var count uint64
	var header [8]byte
	for {
		if _, err := io.ReadFull(f, header[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return 0, err
		}
		compLen := binary.LittleEndian.Uint32(header[0:4])
		if _, err := f.Seek(int64(compLen), io.SeekCurrent); err != nil {
			break
		}
		count++
	}
	return count, nil
}

// parsePackNumber extracts the numeric pack identifier from a pack file path.
// The filename format is "NNNN.pack" — zero-padded to at least 4 digits, but
// longer once pack numbers exceed 9999 (e.g. "10000.pack"), so the digit run
// is parsed without a width cap. Names with extra prefixes or suffixes
// (e.g. "0002.pack.tmp") are rejected.
func parsePackNumber(path string) (uint32, error) {
	base := filepath.Base(path)
	numStr, ok := strings.CutSuffix(base, ".pack")
	if !ok || numStr == "" {
		return 0, fmt.Errorf("cannot parse pack number from %q", base)
	}
	n, err := strconv.ParseUint(numStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("cannot parse pack number from %q: %w", base, err)
	}
	return uint32(n), nil
}

// streamPack reads every frame from a single pack file, decrypting and
// decompressing each one, then calls fn with the raw chunk data.
func streamPack(
	ctx context.Context,
	path string,
	packNum uint32,
	key *crypto.MasterKey,
	decoder *zstd.Decoder,
	fn func(packNum uint32, offset int64, rawLen uint32, raw []byte) error,
) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 4*1024*1024) // 4 MB read-ahead
	var offset int64
	var header [8]byte

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if _, err := io.ReadFull(br, header[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("reading frame header at offset %d: %w", offset, err)
		}

		compLen := binary.LittleEndian.Uint32(header[0:4])
		rawLen := binary.LittleEndian.Uint32(header[4:8])

		payload := make([]byte, compLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return fmt.Errorf("reading frame payload at offset %d: %w", offset, err)
		}

		frameOffset := offset
		offset += 8 + int64(compLen)

		// Decrypt if needed.
		compressed := payload
		if key != nil {
			decrypted, err := key.DecryptWithAAD(payload, crypto.AADChunk)
			if err != nil {
				return fmt.Errorf("decrypting frame at offset %d: %w", frameOffset, err)
			}
			compressed = decrypted
		}

		// Decompress.
		raw, err := decoder.DecodeAll(compressed, nil)
		if err != nil {
			return fmt.Errorf("decompressing frame at offset %d: %w", frameOffset, err)
		}

		if err := fn(packNum, frameOffset, rawLen, raw); err != nil {
			return err
		}
	}
}
