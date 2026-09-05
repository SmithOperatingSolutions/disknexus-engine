// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// VerifyError describes a single verification failure.
type VerifyError struct {
	ChunkIndex int
	Offset     int64
	Message    string
}

func (e VerifyError) Error() string {
	return fmt.Sprintf("chunk %d (offset %d): %s", e.ChunkIndex, e.Offset, e.Message)
}

// Digest verdicts (#455): what a verify can honestly say about the
// manifest's whole-stream content digest. "" — no claim — is what a sampled
// or errored verify reports: a fold over part of the stream can neither
// match nor honestly mismatch the whole.
const (
	DigestMatch         = "match"
	DigestMismatch      = "mismatch"
	DigestNotVerifiable = "not-verifiable" // pre-#455 backup: no stored digest
)

// VerifyResult contains the outcome of a verify operation.
type VerifyResult struct {
	TotalChunks    int64
	VerifiedChunks int64
	ExcludedChunks int64
	Errors         []VerifyError
	Duration       time.Duration

	// DigestVerdict is the whole-stream check (#455): DigestMatch,
	// DigestMismatch, DigestNotVerifiable, or "" when this verify ran over
	// a sample (or hit chunk errors) and therefore makes no stream claim.
	// Per-chunk checks prove each chunk matches ITSELF; an entry list that
	// lost, duplicated or reordered a record passes every one of them —
	// only the fold over the reconstruction can object (#376, one level up).
	DigestVerdict string
	// DigestExpected/DigestActual are set on a mismatch, so the report
	// names both values instead of "they differ".
	DigestExpected string
	DigestActual   string
}

// OK returns true if the backup verified without errors.
func (r *VerifyResult) OK() bool {
	return len(r.Errors) == 0
}

// Verify checks that all chunks in a backup are retrievable and match their hashes.
// Unlike Restore, it reports ALL errors instead of stopping at the first one.
//
// Use VerifyWithNormalizer for repos created with --normalize; a nil
// normalizer (this function) verifies the stored bytes directly.
func Verify(ctx context.Context, backup *manifest.Backup, idx *index.DedupIndex, st *store.ChunkStore) (*VerifyResult, error) {
	return VerifyWithNormalizer(ctx, backup, idx, st, nil)
}

// VerifyWithNormalizer is Verify for repos that used a normalizer. Chunk
// identity is the hash of the normalized bytes while the stored bytes are
// the originals, so the retrieved bytes are re-normalized before comparing
// against the manifest's chunk hash. norm must match the repo config.
func VerifyWithNormalizer(ctx context.Context, backup *manifest.Backup, idx *index.DedupIndex, st *store.ChunkStore, norm preprocess.Normalizer) (*VerifyResult, error) {
	indices := make([]int, len(backup.Entries))
	for i := range backup.Entries {
		indices[i] = i
	}
	return VerifySelectedWithNormalizer(ctx, backup, idx, st, norm, indices)
}

// VerifySelectedWithNormalizer verifies only the entries at the given manifest
// indices (used by sampled cloud verify). Each VerifyError's ChunkIndex is the
// TRUE manifest index, so the report is unambiguous regardless of sampling.
// VerifyStreamed is the full verify over an EntryAccessor (#478): the walk
// restore-zip's memory lesson (#419) demands — windowed reads off the
// staged .dnm, never the whole entry list resident. It is by construction
// the complete in-order walk, so the digest fold applies exactly as in the
// slice path. backup supplies METADATA only (digest fields, the zero-entries
// guard); its Entries slice is ignored and may be nil.
// onProgress, when non-nil, is called every progressStride entries and at
// each window boundary with (verified so far, total) — the panel's percent
// for a run that takes minutes. Finer than the read window on purpose (the
// a00b2ce7 incident: one slow window froze progress for 11 minutes, and a
// stuck verify was indistinguishable from a slow one).
func VerifyStreamed(ctx context.Context, backup *manifest.Backup, entries manifest.EntryAccessor, idx *index.DedupIndex, st *store.ChunkStore, norm preprocess.Normalizer, onProgress func(done, total int64)) (*VerifyResult, error) {
	sv, err := NewStreamVerify(backup, entries.Count())
	if err != nil {
		return nil, err
	}
	if err := sv.Range(ctx, entries, 0, entries.Count(), idx, st, norm, onProgress); err != nil {
		return nil, err
	}
	return sv.Finish(), nil
}

// StreamVerify is VerifyStreamed opened up for span-wise walking (#522
// phase 3): the caller may verify the entry stream in consecutive ranges —
// staging each range's chunks under a disk budget between calls — and the
// digest fold carries across ranges, because SHA-256 state advanced in
// stream order is the SAME fold DigestCoversSourceStreamV1 names. Ranges
// MUST be walked consecutively from zero; Range enforces it, because a
// gap or overlap would silently fold a stream that is not the source.
type StreamVerify struct {
	total  int64
	next   int64
	start  time.Time
	result VerifyResult
	fold   *digestFold
}

// NewStreamVerify prepares a walk over total entries of backup (METADATA
// only — Entries may be nil, exactly as VerifyStreamed).
func NewStreamVerify(backup *manifest.Backup, total int64) (*StreamVerify, error) {
	if total == 0 && backup.TotalBytes > 0 {
		return nil, fmt.Errorf("backup %s has no chunk entries but claims %d bytes; its entries are missing or corrupt (try 'index --rebuild-all' or re-import)", backup.BackupID, backup.TotalBytes)
	}
	sv := &StreamVerify{total: total, start: time.Now(), fold: &digestFold{}}
	sv.result.TotalChunks = total
	if backup.ContentDigest != "" && backup.ContentDigestCovers == manifest.DigestCoversSourceStreamV1 {
		sv.fold.h = sha256.New()
		sv.fold.want = backup.ContentDigest
	} else {
		sv.fold.verdict = DigestNotVerifiable
	}
	return sv, nil
}

// Range verifies entries [lo, hi), folding them into the stream digest.
// onProgress reports GLOBAL positions against the full total.
func (sv *StreamVerify) Range(ctx context.Context, entries manifest.EntryAccessor, lo, hi int64, idx *index.DedupIndex, st *store.ChunkStore, norm preprocess.Normalizer, onProgress func(done, total int64)) error {
	if lo != sv.next {
		return fmt.Errorf("verify range starts at %d but the fold is at %d — the stream digest is a fold over CONSECUTIVE entries, and a gap or overlap would verify a stream that is not the source", lo, sv.next)
	}
	if hi > sv.total {
		return fmt.Errorf("verify range [%d,%d) exceeds the %d entries", lo, hi, sv.total)
	}
	n := sv.total
	const window = 4096
	// The callback's grain, deliberately decoupled from the read window: the
	// window bounds MEMORY, the stride bounds SILENCE. 64 entries is at most
	// a few pack-fetches of wall time; the callback itself is atomic stores.
	const progressStride = 64
	for wlo := lo; wlo < hi; wlo += window {
		whi := wlo + window
		if whi > hi {
			whi = hi
		}
		batch, err := entries.Range(wlo, whi)
		if err != nil {
			return fmt.Errorf("reading entries [%d,%d): %w", wlo, whi, err)
		}
		for j, entry := range batch {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			i := int(wlo) + j
			switch {
			case entry.IsExcluded:
				sv.result.ExcludedChunks++
				sv.fold.zeros(entry.ChunkLength)
			default:
				if data, verr := verifyEntry(i, entry, idx, st, norm); verr != nil {
					sv.result.Errors = append(sv.result.Errors, *verr)
					sv.fold.abort()
				} else {
					sv.fold.write(data)
					sv.result.VerifiedChunks++
				}
			}
			// Every OUTCOME ticks — excluded and errored entries advance the
			// walk exactly as verified ones do, and a manifest with a long
			// excluded span must not read as a hang.
			if onProgress != nil && (i+1)%progressStride == 0 {
				onProgress(int64(i)+1, n)
			}
		}
		if onProgress != nil {
			onProgress(whi, n)
		}
	}
	sv.next = hi
	return nil
}

// Checkpoint serializes the walk's position so a later process can continue
// it (#522: a multi-span verify that outlives one run). It carries the
// entry offset, the counters, and the SHA-256 fold's own marshaled state —
// crypto/sha256 implements encoding.BinaryMarshaler, and a fold restored
// from it produces the digest an uninterrupted fold would, so
// DigestCoversSourceStreamV1 is untouched: still one sequential fold over
// the whole stream, across process lifetimes.
//
// A walk that has seen a chunk error makes no stream claim and cannot be
// resumed: the fold is already aborted, and continuing would only spend
// hours to report a verdict Finish will refuse anyway.
func (sv *StreamVerify) Checkpoint() ([]byte, error) {
	if len(sv.result.Errors) > 0 {
		return nil, fmt.Errorf("verify saw %d chunk errors; a walk that makes no stream claim has nothing to resume", len(sv.result.Errors))
	}
	var foldState []byte
	if sv.fold.h != nil {
		m, ok := sv.fold.h.(encoding.BinaryMarshaler)
		if !ok {
			return nil, fmt.Errorf("digest fold does not marshal; cannot checkpoint")
		}
		var err error
		if foldState, err = m.MarshalBinary(); err != nil {
			return nil, fmt.Errorf("marshaling digest fold: %w", err)
		}
	}
	return json.Marshal(streamCheckpoint{
		Next: sv.next, Verified: sv.result.VerifiedChunks, Excluded: sv.result.ExcludedChunks,
		FoldState: foldState, Verdict: sv.fold.verdict,
	})
}

type streamCheckpoint struct {
	Next      int64  `json:"next"`
	Verified  int64  `json:"verified"`
	Excluded  int64  `json:"excluded"`
	FoldState []byte `json:"fold_state,omitempty"`
	Verdict   string `json:"verdict,omitempty"`
}

// ResumeStreamVerify is NewStreamVerify continued from a Checkpoint. The
// next Range must start at the checkpoint's offset; Range enforces it.
func ResumeStreamVerify(backup *manifest.Backup, total int64, checkpoint []byte) (*StreamVerify, error) {
	sv, err := NewStreamVerify(backup, total)
	if err != nil {
		return nil, err
	}
	var cp streamCheckpoint
	if err := json.Unmarshal(checkpoint, &cp); err != nil {
		return nil, fmt.Errorf("verify checkpoint unreadable: %w", err)
	}
	if cp.Next < 0 || cp.Next > total {
		return nil, fmt.Errorf("verify checkpoint at entry %d of %d: not this backup's walk", cp.Next, total)
	}
	if sv.fold.h != nil {
		if cp.FoldState == nil {
			return nil, fmt.Errorf("verify checkpoint carries no digest state for a digest-bearing backup")
		}
		u, ok := sv.fold.h.(encoding.BinaryUnmarshaler)
		if !ok {
			return nil, fmt.Errorf("digest fold does not unmarshal; cannot resume")
		}
		if err := u.UnmarshalBinary(cp.FoldState); err != nil {
			return nil, fmt.Errorf("restoring digest fold: %w", err)
		}
	}
	sv.next = cp.Next
	sv.result.VerifiedChunks = cp.Verified
	sv.result.ExcludedChunks = cp.Excluded
	return sv, nil
}

// Next is the entry offset the next Range must start at.
func (sv *StreamVerify) Next() int64 { return sv.next }

// Finish closes the fold and returns the result. The digest verdict is only
// meaningful when every entry was ranged; Finish refuses a partial walk the
// same way the range check refuses a gap.
func (sv *StreamVerify) Finish() *VerifyResult {
	if sv.next != sv.total {
		// A partial walk makes no stream claim (the sampled-verify rule).
		sv.fold.abort()
	}
	sv.fold.finish(&sv.result)
	sv.result.Duration = time.Since(sv.start)
	return &sv.result
}

func VerifySelectedWithNormalizer(ctx context.Context, backup *manifest.Backup, idx *index.DedupIndex, st *store.ChunkStore, norm preprocess.Normalizer, indices []int) (*VerifyResult, error) {
	// A backup that claims data but carries no entries (a lost .entries sidecar)
	// cannot be verified — zero entries would otherwise loop zero times and
	// report a false PASS. Parity with the restore guard.
	if len(backup.Entries) == 0 && backup.TotalBytes > 0 {
		return nil, fmt.Errorf("backup %s has no chunk entries but claims %d bytes; its entries are missing or corrupt (try 'index --rebuild-all' or re-import)", backup.BackupID, backup.TotalBytes)
	}

	start := time.Now()

	var result VerifyResult
	result.TotalChunks = int64(len(indices))

	// The stream fold (#455) runs only when this verify walks the WHOLE
	// entry list in order — the only walk whose reconstruction is the
	// stream. Excluded entries contribute ChunkLength zeros, because that
	// is what the capture folded: exclusion zeroes blocks BEFORE the
	// chunker (#94), so zeros are what the stream held there.
	fold := newDigestFold(backup, indices)

	for _, i := range indices {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		entry := backup.Entries[i]

		if entry.IsExcluded {
			result.ExcludedChunks++
			fold.zeros(entry.ChunkLength)
			continue
		}
		data, verr := verifyEntry(i, entry, idx, st, norm)
		if verr != nil {
			result.Errors = append(result.Errors, *verr)
			fold.abort() // an unverified chunk means the fold is not the stream
			continue
		}
		fold.write(data)
		result.VerifiedChunks++
	}
	fold.finish(&result)

	result.Duration = time.Since(start)
	return &result, nil
}

// digestFold accumulates the reconstruction for the whole-stream check.
type digestFold struct {
	h       hash.Hash
	want    string
	verdict string // pre-decided verdict; "" = fold and compare
}

func newDigestFold(backup *manifest.Backup, indices []int) *digestFold {
	f := &digestFold{}
	// A sample makes no claim, whatever the manifest carries.
	if len(indices) != len(backup.Entries) {
		return f
	}
	for j, i := range indices {
		if i != j {
			return f // out of order or duplicated: not the stream
		}
	}
	if backup.ContentDigest == "" || backup.ContentDigestCovers != manifest.DigestCoversSourceStreamV1 {
		// No digest (pre-#455), or a definition this build does not fold:
		// NOT VERIFIABLE — distinct from failed, never reported as passed.
		f.verdict = DigestNotVerifiable
		return f
	}
	f.h = sha256.New()
	f.want = backup.ContentDigest
	return f
}

func (f *digestFold) write(p []byte) {
	if f.h != nil {
		f.h.Write(p)
	}
}

var foldZeros [64 << 10]byte

func (f *digestFold) zeros(n int) {
	if f.h == nil {
		return
	}
	for n > 0 {
		c := n
		if c > len(foldZeros) {
			c = len(foldZeros)
		}
		f.h.Write(foldZeros[:c])
		n -= c
	}
}

func (f *digestFold) abort() { f.h = nil; f.verdict = "" }

func (f *digestFold) finish(res *VerifyResult) {
	if f.h == nil {
		res.DigestVerdict = f.verdict
		return
	}
	got := hex.EncodeToString(f.h.Sum(nil))
	if got == f.want {
		res.DigestVerdict = DigestMatch
		return
	}
	res.DigestVerdict = DigestMismatch
	res.DigestExpected = f.want
	res.DigestActual = got
}

// verifyEntry runs the full per-chunk check (lookup → retrieve → re-normalize →
// hash → length) for one entry, returning a VerifyError on the first problem or
// nil on success. Retrieve internally decrypts (AADChunk) and decompresses, so
// this is byte-identical to the restore path.
// verifyEntry also returns the retrieved (stored-original) bytes on
// success, so the caller's stream fold reads what a restore would write
// without a second retrieval.
func verifyEntry(i int, entry manifest.Entry, idx *index.DedupIndex, st *store.ChunkStore, norm preprocess.Normalizer) ([]byte, *VerifyError) {
	idxEntry, found, err := idx.LookupDirect(entry.ChunkHash)
	if err != nil {
		return nil, &VerifyError{ChunkIndex: i, Offset: entry.VolumeOffset, Message: fmt.Sprintf("index lookup error: %v", err)}
	}
	if !found {
		return nil, &VerifyError{ChunkIndex: i, Offset: entry.VolumeOffset, Message: fmt.Sprintf("chunk not found in index (hash %x)", entry.ChunkHash[:8])}
	}

	data, err := st.Retrieve(idxEntry.PackNumber, int64(idxEntry.StoreOffset))
	if err != nil {
		return nil, &VerifyError{ChunkIndex: i, Offset: entry.VolumeOffset, Message: fmt.Sprintf("retrieval error: %v", err)}
	}

	actualHash := sha256.Sum256(preprocess.IdentityHashInput(norm, data))
	if actualHash != entry.ChunkHash {
		return nil, &VerifyError{ChunkIndex: i, Offset: entry.VolumeOffset, Message: fmt.Sprintf("SHA-256 mismatch: expected %x, got %x", entry.ChunkHash[:8], actualHash[:8])}
	}
	if len(data) != entry.ChunkLength {
		return nil, &VerifyError{ChunkIndex: i, Offset: entry.VolumeOffset, Message: fmt.Sprintf("size mismatch: expected %d, got %d", entry.ChunkLength, len(data))}
	}
	return data, nil
}
