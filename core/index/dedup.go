// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// DedupResult represents the outcome of a dedup lookup.
type DedupResult struct {
	IsNew     bool        // true if this chunk has not been seen before
	Entry     *IndexEntry // non-nil if the chunk is a duplicate
	BloomHit  bool        // true if the bloom filter indicated a possible match
	BloomMiss bool        // true if the bloom filter returned definite negative
}

// DedupIndex combines the bloom filter and hash index into a two-tier
// dedup lookup system.
type DedupIndex struct {
	bloom *BloomFilter
	index *HashIndex
	dir   string
	key   *crypto.MasterKey // nil for unencrypted repos

	// bloomMissing records that bloom.bin was absent at open and a fresh empty
	// bloom was created. See BloomSuspect.
	bloomMissing bool

	// delta is this session's index-delta journal when capture is armed
	// (#357 phase 2). See deltacapture.go.
	delta *deltaCapture
}

// ReadOpenExpectedChunks is what a READ-side open passes for expectedChunks
// (#356 item 10). It is a placeholder, not a tuning knob: expectedChunks only
// sizes a bloom filter that is BUILT, and an index being opened to verify,
// restore, prune, sweep or reconcile already has its bloom.bin on disk, which
// is loaded as it was written. The value is reached at all only if the
// directory turns out to be empty — a fresh, immediately-discarded filter —
// so raising or lowering it changes nothing an operator can observe.
//
// It used to be the bare literal 10000 at eleven call sites with no comment
// anywhere, which is eleven invitations to tune one of them.
//
// The WRITE path does not use this. pipeline.go derives a real estimate from
// the source size and the repo's chunk geometry, because there the filter it
// sizes is the one the backup fills.
const ReadOpenExpectedChunks uint64 = 10000

// NewDedupIndex creates or opens a dedup index in the given directory.
// cacheMB controls the page cache size for the hash index (0 disables caching).
// An optional MasterKey enables AES-256-GCM encryption of index files at rest.
//
// expectedChunks sizes a NEW bloom filter; an existing bloom.bin is loaded
// instead. Read-side callers pass ReadOpenExpectedChunks.
func NewDedupIndex(dir string, expectedChunks uint64, fpRate float64, cacheMB int, key ...*crypto.MasterKey) (*DedupIndex, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating index dir: %w", err)
	}

	var mk *crypto.MasterKey
	if len(key) > 0 && key[0] != nil {
		mk = key[0]
	}

	bloomPath := filepath.Join(dir, "bloom.bin")
	bloomEncPath := bloomPath + ".enc"
	indexPath := filepath.Join(dir, "hash-index.db")
	indexEncPath := indexPath + ".enc"

	// For encrypted repos, decrypt .enc files to plaintext working copies.
	if mk != nil {
		if _, err := os.Stat(bloomEncPath); err == nil {
			if err := crypto.DecryptFile(mk, bloomEncPath, bloomPath); err != nil {
				return nil, fmt.Errorf("decrypting bloom filter: %w", err)
			}
		}
		if _, err := os.Stat(indexEncPath); err == nil {
			if err := crypto.DecryptFile(mk, indexEncPath, indexPath); err != nil {
				return nil, fmt.Errorf("decrypting hash index: %w", err)
			}
		}
	} else if err := refuseEncryptedWithoutKey(dir); err != nil {
		return nil, err
	}

	// Try to load existing bloom filter, or create new
	var bloom *BloomFilter
	bloomMissing := false
	if _, err := os.Stat(bloomPath); err == nil {
		bloom, err = LoadBloomFilter(bloomPath)
		if err != nil {
			return nil, fmt.Errorf("loading bloom filter: %w", err)
		}
		// For encrypted repos, remove plaintext bloom after loading into memory.
		if mk != nil {
			os.Remove(bloomPath)
		}
	} else {
		bloom = NewBloomFilter(expectedChunks, fpRate)
		bloomMissing = true
	}

	hashIdx, err := NewHashIndex(indexPath, cacheMB, false)
	if err != nil {
		return nil, fmt.Errorf("opening hash index: %w", err)
	}

	d := &DedupIndex{
		bloom:        bloom,
		index:        hashIdx,
		dir:          dir,
		key:          mk,
		bloomMissing: bloomMissing,
	}
	// A cloud repository's index is the authoritative objects PLUS the deltas
	// not yet folded into them (#357 phase 2). Merging here is what makes
	// every reader — restore, verify, dedup, GC — see the same complete view
	// without a single call site having to remember to ask.
	if _, err := d.applyPendingDeltas(hashIdx.diskSize == 0); err != nil {
		hashIdx.CloseDiscard()
		return nil, err
	}
	return d, nil
}

// refuseEncryptedWithoutKey stops a KEYLESS open of an ENCRYPTED index (#370).
//
// hash-index.db.enc and bloom.bin.enc are only looked at when a key was
// supplied. With no key they were silently ignored, the plaintext names were
// not there either, and the open produced an EMPTY index — an index that has
// forgotten every chunk the repository holds, indistinguishable from a fresh
// one. Nothing downstream could tell: BloomSuspect is "bloom.bin missing AND
// Count() > 0", and the count is zero for exactly the same reason. A backup
// then publishes that amnesiac index over the real one and the EARLIER backup
// stops resolving ("chunk N not found in index") — silent, and unrecoverable
// without the operator noticing in time.
//
// So the presence of an .enc index object is treated as what it is: a statement
// that this index is encrypted, and that opening it needs a key nobody handed
// us. This is the same refusal the delta path already makes for a .delta.enc
// with no key (deltaapply.go, readDeltaObject) and it names the same input,
// store.IndexKeyFor — the single place that decides whether a repo's index has
// a key at all (managed: nil, passphrase: the real key).
//
// A plaintext hash-index.db lying beside the .enc objects does NOT excuse the
// open. That combination is a working copy some session left behind, not the
// repository's index, and preferring it would be the original bug wearing a
// different hat: a stale, partial view presented as the whole truth.
func refuseEncryptedWithoutKey(dir string) error {
	for _, name := range []string{"hash-index.db.enc", "bloom.bin.enc"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("index at %s is encrypted (%s is present) and was opened without a key — "+
				"reading the repository's index needs the repo key (store.IndexKeyFor); opening it anyway "+
				"would present an EMPTY index and any backup written against it would forget every chunk "+
				"the repository already holds", dir, name)
		}
	}
	return nil
}

// BloomSuspect reports that bloom.bin was absent beside a POPULATED hash index
// — corruption, not a fresh repo. Check() uses the bloom as a tier-1
// definite-negative (MayContain false ⇒ "new chunk", skip the index), so an
// empty bloom makes every chunk look new and a BACKUP would silently re-store
// the whole repo as duplicates. Only write paths must refuse this state:
// restore/verify/export use LookupDirect, which bypasses the bloom entirely
// and works fine — failing them at open would block recovery of intact data
// (permanently, for managed repos where rebuild needs a controller).
func (d *DedupIndex) BloomSuspect() bool {
	return d.bloomMissing && d.index.Count() > 0
}

// NewDedupIndexReadOnly opens a dedup index without building the .htab file.
// This saves significant I/O for callers that only need bulk reads (ReadAllEntries)
// and writes (Insert + Flush), not per-chunk Lookup or FlushDelta.
func NewDedupIndexReadOnly(dir string, expectedChunks uint64, fpRate float64, cacheMB int, key ...*crypto.MasterKey) (*DedupIndex, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("creating index dir: %w", err)
	}

	var mk *crypto.MasterKey
	if len(key) > 0 && key[0] != nil {
		mk = key[0]
	}

	bloomPath := filepath.Join(dir, "bloom.bin")
	bloomEncPath := bloomPath + ".enc"
	indexPath := filepath.Join(dir, "hash-index.db")
	indexEncPath := indexPath + ".enc"

	if mk != nil {
		if _, err := os.Stat(bloomEncPath); err == nil {
			if err := crypto.DecryptFile(mk, bloomEncPath, bloomPath); err != nil {
				return nil, fmt.Errorf("decrypting bloom filter: %w", err)
			}
		}
		if _, err := os.Stat(indexEncPath); err == nil {
			if err := crypto.DecryptFile(mk, indexEncPath, indexPath); err != nil {
				return nil, fmt.Errorf("decrypting hash index: %w", err)
			}
		}
	} else if err := refuseEncryptedWithoutKey(dir); err != nil {
		return nil, err
	}

	var bloom *BloomFilter
	if _, err := os.Stat(bloomPath); err == nil {
		bloom, err = LoadBloomFilter(bloomPath)
		if err != nil {
			return nil, fmt.Errorf("loading bloom filter: %w", err)
		}
		if mk != nil {
			os.Remove(bloomPath)
		}
	} else {
		bloom = NewBloomFilter(expectedChunks, fpRate)
	}

	hashIdx, err := NewHashIndex(indexPath, cacheMB, true)
	if err != nil {
		return nil, fmt.Errorf("opening hash index: %w", err)
	}

	d := &DedupIndex{
		bloom: bloom,
		index: hashIdx,
		dir:   dir,
		key:   mk,
	}
	if _, err := d.applyPendingDeltas(hashIdx.diskSize == 0); err != nil {
		hashIdx.CloseDiscard()
		return nil, err
	}
	return d, nil
}

// Check performs the two-tier dedup lookup.
// 1. Bloom filter: fast negative → definitely new
// 2. Hash index: confirm match or false positive
func (d *DedupIndex) Check(id hasher.ChunkID) (DedupResult, error) {
	// Tier 1: Bloom filter
	if !d.bloom.MayContain(id.WeakHash) {
		return DedupResult{
			IsNew:     true,
			BloomMiss: true,
		}, nil
	}

	// Tier 2: Hash index lookup
	entry, found, err := d.index.Lookup(id)
	if err != nil {
		return DedupResult{}, fmt.Errorf("index lookup: %w", err)
	}

	if found {
		return DedupResult{
			IsNew:    false,
			Entry:    entry,
			BloomHit: true,
		}, nil
	}

	// Bloom false positive — chunk is actually new
	return DedupResult{
		IsNew:    true,
		BloomHit: true,
	}, nil
}

// Insert adds a new chunk to both the bloom filter and hash index.
func (d *DedupIndex) Insert(id hasher.ChunkID, packNumber uint32, storeOffset uint64, chunkLength uint32) {
	d.insertNoCapture(id, packNumber, storeOffset, chunkLength)
	d.captureInsert(id, packNumber, storeOffset, chunkLength)
}

// insertNoCapture is Insert without the side effect that Insert will grow in
// #357 phase 2: delta capture. Merging somebody else's delta (Delta.ApplyTo)
// goes through here, so entries this run merely LEARNED never end up in the
// delta this run publishes.
func (d *DedupIndex) insertNoCapture(id hasher.ChunkID, packNumber uint32, storeOffset uint64, chunkLength uint32) {
	d.bloom.Add(id.WeakHash)
	d.index.Insert(id, packNumber, storeOffset, chunkLength)
}

// SaveBloom writes THIS index's bloom filter — base plus every delta merged at
// open — to path, without touching the hash index or this index's own files.
//
// It exists for #365: an index being REBUILT (GC dropping deleted packs) has to
// inherit the filter rather than build one, because entries carry no weak hash
// and the rebuild path never sees chunk plaintext. Compaction gets the same
// effect for free by opening the destination on the same directory; a rebuild
// writes somewhere else, so it needs to hand the filter over explicitly.
//
// The bloom is written in the clear. Callers that persist an encrypted repo's
// index encrypt it themselves, the way Flush does.
func (d *DedupIndex) SaveBloom(path string) error {
	return d.bloom.Save(path)
}

// Flush persists the bloom filter and hash index to disk.
// For encrypted repos, writes .enc files and removes plaintext.
func (d *DedupIndex) Flush() error {
	bloomPath := filepath.Join(d.dir, "bloom.bin")
	if err := d.bloom.Save(bloomPath); err != nil {
		return fmt.Errorf("saving bloom filter: %w", err)
	}
	if err := d.index.Flush(); err != nil {
		return fmt.Errorf("flushing hash index: %w", err)
	}

	if d.key != nil {
		indexPath := filepath.Join(d.dir, "hash-index.db")
		if err := crypto.EncryptFile(d.key, bloomPath, bloomPath+".enc"); err != nil {
			return fmt.Errorf("encrypting bloom filter: %w", err)
		}
		os.Remove(bloomPath)

		if err := crypto.EncryptFile(d.key, indexPath, indexPath+".enc"); err != nil {
			return fmt.Errorf("encrypting hash index: %w", err)
		}
	}
	// The delta object is made valid at the same moment the index files are:
	// a caller that has flushed has, by that act, a publishable delta.
	return d.WriteDeltaObject()
}

// CloseDiscard closes the dedup index WITHOUT flushing session state: no bloom
// save, no sorted-file rewrite. Buffered inserts are dropped. Used by analyze
// mode (must leave the repo untouched) and failed backups (must not durably
// reference chunks in packs that were never sealed). For encrypted repos the
// plaintext working copy is removed, leaving the pre-session .enc files as the
// authoritative state.
func (d *DedupIndex) CloseDiscard() error {
	// Discard the delta FIRST: a half-written journal must not outlive the
	// session that abandoned it even if the index close then fails.
	_ = d.closeDelta(true)
	err := d.index.CloseDiscard()
	if d.key != nil {
		os.Remove(filepath.Join(d.dir, "bloom.bin"))
		os.Remove(filepath.Join(d.dir, "hash-index.db"))
	}
	return err
}

// Close flushes and closes the dedup index.
// For encrypted repos, removes plaintext hash-index.db after close.
func (d *DedupIndex) Close() error {
	if err := d.Flush(); err != nil {
		d.closeDelta(true)
		d.index.Close()
		return err
	}
	if err := d.closeDelta(false); err != nil {
		d.index.Close()
		return err
	}
	err := d.index.Close()

	// Remove plaintext working copy for encrypted repos.
	if d.key != nil {
		indexPath := filepath.Join(d.dir, "hash-index.db")
		os.Remove(indexPath)
	}
	return err
}

// SetMemFlushed enables or disables the in-memory flushed hash set on the
// underlying hash index. See HashIndex.SetMemFlushed for details.
func (d *DedupIndex) SetMemFlushed(enabled bool) {
	d.index.SetMemFlushed(enabled)
}

// FlushHashIndex writes the current in-memory buffer to a small delta file
// without reading or modifying the main sorted index. O(k) cost only.
// Call periodically during long backups to bound memory usage.
// The delta files are merged into the main index on the final Flush.
func (d *DedupIndex) FlushHashIndex() error {
	return d.index.FlushDelta()
}

// LookupDirect bypasses the bloom filter and looks up a chunk by strong hash only.
// Used by the restore engine to find chunks by their SHA-256 hash.
func (d *DedupIndex) LookupDirect(strongHash [32]byte) (*IndexEntry, bool, error) {
	id := hasher.ChunkID{StrongHash: strongHash}
	return d.index.Lookup(id)
}

// ReadAllEntries flushes pending writes and returns all index entries.
func (d *DedupIndex) ReadAllEntries() ([]IndexEntry, error) {
	if err := d.index.Flush(); err != nil {
		return nil, fmt.Errorf("flushing before read-all: %w", err)
	}
	return d.index.ReadAll()
}

// Stats returns statistics about the dedup index.
type Stats struct {
	TotalChunks    uint64
	BloomSizeBytes uint64
	BloomItems     uint64
	IndexEntries   uint64
}

// Stats returns current dedup index statistics.
func (d *DedupIndex) Stats() Stats {
	return Stats{
		TotalChunks:    d.index.Count(),
		BloomSizeBytes: d.bloom.SizeBytes(),
		BloomItems:     d.bloom.Count(),
		IndexEntries:   d.index.Count(),
	}
}
