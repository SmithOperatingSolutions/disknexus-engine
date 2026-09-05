// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// Index deltas (#357 phase 2).
//
// A cloud backup used to publish its index by OVERWRITING the whole thing:
// bloom.bin and hash-index.db went up as complete objects at fixed keys on
// every run. That is ~96 bytes of index per stored chunk — about 1.6 GB per
// TB of unique data — moved in both directions by every backup, however
// little changed. It is also last-writer-wins, which is why exactly one
// writer per repo is allowed to exist at a time.
//
// A delta is the same information expressed as a CHANGE: the entries this run
// added, in a writer-unique object nobody else overwrites. Readers merge the
// pending deltas over the authoritative index when they open the repo
// (see cloudsync.DownloadIndex), and the retention sweep folds them back in
// (cloudsync.CompactIndexDeltas).
//
// Three properties make that safe, and each is pinned by a test:
//
//   - IDEMPOTENT. An entry is a hash → coordinates INSERT. Applying the same
//     delta twice inserts the same key twice, which is one key. This is what
//     lets a crashed compaction simply be re-run, and what makes a duplicated
//     delta object harmless.
//   - ORDER-INDEPENDENT. Two writers' deltas commute: distinct chunk hashes
//     do not interact, and the same chunk hash from two writers resolves to
//     two REAL copies of that chunk in two real packs — either mapping
//     restores correctly, and the loser's bytes are unreferenced data GC
//     reclaims.
//   - SELF-CHECKING. The payload carries a SHA-256 of itself and a version
//     word. Every entry in a delta becomes a chunk location that restore
//     trusts absolutely (restore.go LookupDirect hard-fails on a miss, and
//     hard-fails on a WRONG location too, later, at the hash check), so a
//     delta that arrives damaged must be refused rather than merged.
//
// The weak hash is in the format for a reason that is easy to miss: the hash
// index does not store it and cannot recompute it (it is xxhash of the chunk
// PLAINTEXT, see hasher.Sum), but the bloom filter — dedup's tier-1 negative
// — is keyed on it. A delta carrying only hash-index fields would leave every
// chunk it describes invisible to dedup until the next compaction, which is
// precisely the cross-device dedup this issue exists to enable.

const (
	deltaMagic = "DNIDLT"
	// DeltaVersion is the on-the-wire format version. Bump it for any layout
	// change; older readers refuse rather than misread (ParseDelta).
	DeltaVersion = uint16(1)
	// DeltaHeaderSize is the fixed header preceding the entry array.
	DeltaHeaderSize = 64
	// DeltaEntrySize is one entry: hash(32) + weak(8) + pack(4) + offset(8) +
	// length(4). The 8 bytes of slack are the price of natural alignment and
	// a format that can grow a field without a version bump costing a rewrite.
	DeltaEntrySize = 56
)

// header layout:
//
//	 0   6  magic "DNIDLT"
//	 6   2  version      uint16 LE
//	 8   8  entryCount   uint64 LE
//	16  32  sha256 of the entry array
//	48  16  reserved (zero)
const (
	deltaOffVersion = 6
	deltaOffCount   = 8
	deltaOffSum     = 16
)

// DeltaEntry is one chunk's location plus the weak hash its bloom bit needs.
type DeltaEntry struct {
	StrongHash  [32]byte
	WeakHash    uint64
	PackNumber  uint32
	StoreOffset uint64
	ChunkLength uint32
}

// Delta is a decoded index delta.
type Delta struct {
	Entries []DeltaEntry
}

// Marshal encodes the delta, including the payload checksum.
func (d *Delta) Marshal() []byte {
	buf := make([]byte, DeltaHeaderSize+len(d.Entries)*DeltaEntrySize)
	copy(buf[0:6], deltaMagic)
	binary.LittleEndian.PutUint16(buf[deltaOffVersion:], DeltaVersion)
	binary.LittleEndian.PutUint64(buf[deltaOffCount:], uint64(len(d.Entries)))
	for i := range d.Entries {
		EncodeDeltaEntry(&d.Entries[i], buf[DeltaHeaderSize+i*DeltaEntrySize:])
	}
	sum := sha256.Sum256(buf[DeltaHeaderSize:])
	copy(buf[deltaOffSum:deltaOffSum+32], sum[:])
	return buf
}

// EncodeDeltaEntry writes one entry into the first DeltaEntrySize bytes of dst.
// Exported because the write path streams entries into a journal file as they
// are inserted rather than buffering a whole run's worth in memory.
func EncodeDeltaEntry(e *DeltaEntry, dst []byte) {
	copy(dst[0:32], e.StrongHash[:])
	binary.LittleEndian.PutUint64(dst[32:40], e.WeakHash)
	binary.LittleEndian.PutUint32(dst[40:44], e.PackNumber)
	binary.LittleEndian.PutUint64(dst[44:52], e.StoreOffset)
	binary.LittleEndian.PutUint32(dst[52:56], e.ChunkLength)
}

func decodeDeltaEntry(src []byte, e *DeltaEntry) {
	copy(e.StrongHash[:], src[0:32])
	e.WeakHash = binary.LittleEndian.Uint64(src[32:40])
	e.PackNumber = binary.LittleEndian.Uint32(src[40:44])
	e.StoreOffset = binary.LittleEndian.Uint64(src[44:52])
	e.ChunkLength = binary.LittleEndian.Uint32(src[52:56])
}

// DeltaHeader builds the fixed header for an entry array of the given length
// and checksum. The journal writer patches this over its placeholder once the
// run's entry count is known.
func DeltaHeader(entryCount uint64, sum [32]byte) []byte {
	hdr := make([]byte, DeltaHeaderSize)
	copy(hdr[0:6], deltaMagic)
	binary.LittleEndian.PutUint16(hdr[deltaOffVersion:], DeltaVersion)
	binary.LittleEndian.PutUint64(hdr[deltaOffCount:], entryCount)
	copy(hdr[deltaOffSum:deltaOffSum+32], sum[:])
	return hdr
}

// ParseDelta decodes and VERIFIES a delta object.
//
// Every refusal here is deliberate. A delta contributes chunk coordinates to
// an index that restore trusts without a second source, so "parse what you
// can" is the wrong posture: an object that is not exactly a delta of a
// version we understand, whose payload hashes to what its header claims, is
// not merged at all.
// DeltaEntryCount reads a staged delta FILE's entry count from its fixed
// header, cross-checked against the file's size — without materializing a
// single entry (#504: staging counted entries by full parse, holding raw
// bytes and parsed structs simultaneously for a number). The full parse —
// checksum included — still happens at the merge, which is where a corrupt
// delta must stop things.
func DeltaEntryCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, DeltaHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return 0, fmt.Errorf("not an index delta (short header): %w", err)
	}
	if string(hdr[0:6]) != deltaMagic {
		return 0, fmt.Errorf("not an index delta (bad magic)")
	}
	if v := binary.LittleEndian.Uint16(hdr[deltaOffVersion:]); v != DeltaVersion {
		return 0, fmt.Errorf("index delta version %d is not version %d", v, DeltaVersion)
	}
	count := binary.LittleEndian.Uint64(hdr[deltaOffCount:])
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if uint64(fi.Size()) != uint64(DeltaHeaderSize)+count*DeltaEntrySize {
		return 0, fmt.Errorf("index delta declares %d entries but carries %d bytes — truncated", count, fi.Size())
	}
	return int(count), nil
}

// ValidateDeltaFile streams a staged delta file through every ParseDelta
// check — magic, version, declared count vs size, payload checksum —
// holding only a read buffer (#507 round 2: whole-file validation buffers
// were half the monster-delta burst). A delta that will not validate is a
// HARD failure; its entries are locations completed backups depend on.
func ValidateDeltaFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, DeltaHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fmt.Errorf("not an index delta (short header): %w", err)
	}
	if string(hdr[0:6]) != deltaMagic {
		return fmt.Errorf("not an index delta (bad magic)")
	}
	if v := binary.LittleEndian.Uint16(hdr[deltaOffVersion:]); v != DeltaVersion {
		return fmt.Errorf("index delta version %d is not version %d — this build cannot merge it, "+
			"and merging it wrong would put false chunk locations in front of restore", v, DeltaVersion)
	}
	count := binary.LittleEndian.Uint64(hdr[deltaOffCount:])
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if uint64(fi.Size()) != uint64(DeltaHeaderSize)+count*DeltaEntrySize {
		return fmt.Errorf("index delta declares %d entries (%d bytes) but carries %d bytes — truncated",
			count, count*DeltaEntrySize, fi.Size()-int64(DeltaHeaderSize))
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	var want, got [32]byte
	copy(want[:], hdr[deltaOffSum:deltaOffSum+32])
	h.Sum(got[:0])
	if got != want {
		return fmt.Errorf("index delta payload checksum mismatch: the object is damaged")
	}
	return nil
}

// streamDeltaRecords yields a VALIDATED delta file's entries one at a time
// off a buffered reader — the fold's record source, no whole-file buffer.
// Callers run ValidateDeltaFile first; this re-checks only the header
// shape.
func streamDeltaRecords(path string, fn func(*DeltaEntry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, DeltaHeaderSize)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return fmt.Errorf("not an index delta (short header): %w", err)
	}
	count := binary.LittleEndian.Uint64(hdr[deltaOffCount:])
	br := bufio.NewReaderSize(f, 1<<20)
	buf := make([]byte, DeltaEntrySize)
	var e DeltaEntry
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(br, buf); err != nil {
			return fmt.Errorf("index delta truncated mid-entry: %w", err)
		}
		decodeDeltaEntry(buf, &e)
		if err := fn(&e); err != nil {
			return err
		}
	}
	return nil
}

// ForEachDeltaEntry validates a delta blob (magic, version, size, checksum
// — everything ParseDelta checks) and yields each entry to fn WITHOUT
// materializing the []DeltaEntry slice (#504: the fold's per-delta parse
// garbage was twice the slab it was filling).
func ForEachDeltaEntry(data []byte, fn func(*DeltaEntry)) error {
	if len(data) < DeltaHeaderSize || string(data[0:6]) != deltaMagic {
		return fmt.Errorf("not an index delta (bad magic)")
	}
	if v := binary.LittleEndian.Uint16(data[deltaOffVersion:]); v != DeltaVersion {
		return fmt.Errorf("index delta version %d is not version %d — this build cannot merge it, "+
			"and merging it wrong would put false chunk locations in front of restore", v, DeltaVersion)
	}
	count := binary.LittleEndian.Uint64(data[deltaOffCount:])
	payload := data[DeltaHeaderSize:]
	if uint64(len(payload)) != count*DeltaEntrySize {
		return fmt.Errorf("index delta declares %d entries (%d bytes) but carries %d bytes — truncated",
			count, count*DeltaEntrySize, len(payload))
	}
	var want [32]byte
	copy(want[:], data[deltaOffSum:deltaOffSum+32])
	if got := sha256.Sum256(payload); got != want {
		return fmt.Errorf("index delta payload checksum mismatch: the object is damaged")
	}
	var e DeltaEntry
	for i := uint64(0); i < count; i++ {
		decodeDeltaEntry(payload[i*DeltaEntrySize:], &e)
		fn(&e)
	}
	return nil
}

func ParseDelta(data []byte) (*Delta, error) {
	if len(data) < DeltaHeaderSize || string(data[0:6]) != deltaMagic {
		return nil, fmt.Errorf("not an index delta (bad magic)")
	}
	if v := binary.LittleEndian.Uint16(data[deltaOffVersion:]); v != DeltaVersion {
		return nil, fmt.Errorf("index delta version %d is not version %d — this build cannot merge it, "+
			"and merging it wrong would put false chunk locations in front of restore", v, DeltaVersion)
	}
	count := binary.LittleEndian.Uint64(data[deltaOffCount:])
	payload := data[DeltaHeaderSize:]
	if uint64(len(payload)) != count*DeltaEntrySize {
		return nil, fmt.Errorf("index delta declares %d entries (%d bytes) but carries %d bytes — truncated",
			count, count*DeltaEntrySize, len(payload))
	}
	var want [32]byte
	copy(want[:], data[deltaOffSum:deltaOffSum+32])
	if got := sha256.Sum256(payload); got != want {
		return nil, fmt.Errorf("index delta payload checksum mismatch: the object is damaged")
	}
	d := &Delta{Entries: make([]DeltaEntry, count)}
	for i := range d.Entries {
		decodeDeltaEntry(payload[i*DeltaEntrySize:], &d.Entries[i])
	}
	return d, nil
}

// ApplyTo merges the delta into an open dedup index: hash-index inserts plus
// the bloom bits dedup needs to find them again.
//
// Deliberately NOT journalled back out (see CaptureDelta): entries that
// arrived in somebody else's delta are already durable in the repo, and
// re-publishing them would make every writer's delta grow with the repo
// instead of with its own work — the exact cost this format removes.
func (d *Delta) ApplyTo(idx *DedupIndex) {
	for i := range d.Entries {
		e := &d.Entries[i]
		idx.insertNoCapture(hasher.ChunkID{WeakHash: e.WeakHash, StrongHash: e.StrongHash},
			e.PackNumber, e.StoreOffset, e.ChunkLength)
	}
}

// PackNumbers returns the distinct pack numbers the delta's entries live in,
// ascending. A delta's entries only ever point into packs the run that wrote
// it created (a backup inserts an index entry only for a chunk it just
// stored), so this is that run's pack-name additions — derived from the
// entries rather than stored beside them, which makes the two impossible to
// disagree.
func (d *Delta) PackNumbers() []uint32 {
	seen := map[uint32]bool{}
	var out []uint32
	for i := range d.Entries {
		n := d.Entries[i].PackNumber
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sortU32Asc(out)
	return out
}

func sortU32Asc(s []uint32) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
