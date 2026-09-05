// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"hash"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// Delta capture: the write half of #357 phase 2.
//
// A run's delta is written INCREMENTALLY, as a journal, rather than gathered
// at the end. The reason is memory: the entries are known only at Insert time
// (the weak hash the bloom needs exists nowhere else), and a first backup of
// a terabyte is ~16 M of them — a gigabyte of live map if we held it. A
// 56-byte buffered append beside a 64 KB chunk upload is not a cost anyone
// can measure.
//
// The journal file IS the delta object: a placeholder header followed by the
// entry array. WriteDeltaObject patches the header (count + payload digest)
// in place and syncs, which makes the object valid on disk at that instant
// without rewriting a byte of payload. It is safe to call repeatedly — ship.go
// uploads once per backup in a chain with the index still open, and each
// upload publishes a strictly larger, strictly valid delta.
//
// Capture is OPT-IN (CaptureDelta) because it only means something for a repo
// whose index is published as deltas. A local repo's index is the file on
// disk; there is nobody to send a change to.

type deltaCapture struct {
	path  string
	f     *os.File
	bw    *bufio.Writer
	sum   hash.Hash // running SHA-256 over the entry array
	count uint64
	buf   [DeltaEntrySize]byte
	err   error // first write error; surfaced by WriteDeltaObject
}

// CaptureDelta arms delta capture for this session: every Insert from now on
// is appended to a delta object at path, which WriteDeltaObject (and Flush,
// and Close) makes valid on disk.
//
// Entries merged from another writer's delta (Delta.ApplyTo) are deliberately
// NOT captured — see insertNoCapture.
func (d *DedupIndex) CaptureDelta(path string) error {
	if d.delta != nil {
		return fmt.Errorf("delta capture already armed at %s", d.delta.path)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating index delta journal: %w", err)
	}
	// Placeholder header, patched by WriteDeltaObject. Written now so the
	// entry array starts at DeltaHeaderSize from the first append.
	if _, err := f.Write(make([]byte, DeltaHeaderSize)); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("writing index delta header: %w", err)
	}
	d.delta = &deltaCapture{
		path: path,
		f:    f,
		bw:   bufio.NewWriterSize(f, 1<<16),
		sum:  sha256.New(),
	}
	return nil
}

// DeltaEntryCount reports how many entries this run has captured. Zero when
// capture is not armed.
func (d *DedupIndex) DeltaEntryCount() uint64 {
	if d.delta == nil {
		return 0
	}
	return d.delta.count
}

// DeltaPath returns the captured delta's path, or "" when capture is not armed.
func (d *DedupIndex) DeltaPath() string {
	if d.delta == nil {
		return ""
	}
	return d.delta.path
}

// captureInsert appends one entry to the delta journal.
//
// Errors are recorded rather than returned: Insert has no error channel and
// giving it one would touch every caller in the tree. WriteDeltaObject is the
// gate — it refuses to declare a delta valid when any append failed, so a
// journal with a hole can never be published.
func (d *DedupIndex) captureInsert(id hasher.ChunkID, packNumber uint32, storeOffset uint64, chunkLength uint32) {
	c := d.delta
	if c == nil || c.err != nil {
		return
	}
	e := DeltaEntry{
		StrongHash:  id.StrongHash,
		WeakHash:    id.WeakHash,
		PackNumber:  packNumber,
		StoreOffset: storeOffset,
		ChunkLength: chunkLength,
	}
	EncodeDeltaEntry(&e, c.buf[:])
	if _, err := c.bw.Write(c.buf[:]); err != nil {
		c.err = err
		return
	}
	c.sum.Write(c.buf[:])
	c.count++
}

// WriteDeltaObject makes the captured delta valid on disk: buffered entries
// are flushed, the header is patched with the entry count and the payload
// digest, and the file is synced.
//
// Called by Flush and Close, so ordinary callers never need it. It is a no-op
// when capture is not armed.
func (d *DedupIndex) WriteDeltaObject() error {
	c := d.delta
	if c == nil {
		return nil
	}
	if c.err != nil {
		return fmt.Errorf("index delta journal is incomplete: %w", c.err)
	}
	if err := c.bw.Flush(); err != nil {
		c.err = err
		return fmt.Errorf("flushing index delta journal: %w", err)
	}
	var sum [32]byte
	copy(sum[:], c.sum.Sum(nil))
	if _, err := c.f.WriteAt(DeltaHeader(c.count, sum), 0); err != nil {
		c.err = err
		return fmt.Errorf("stamping index delta header: %w", err)
	}
	if err := c.f.Sync(); err != nil {
		c.err = err
		return fmt.Errorf("syncing index delta: %w", err)
	}
	if c.count == 0 {
		return nil // nothing to describe; see closeDelta
	}
	return d.encryptCapturedDelta(c.path)
}

// closeDelta finalizes and closes the journal. discard removes the object —
// what a FAILED run must do, since its entries name chunks in packs that were
// never sealed.
func (d *DedupIndex) closeDelta(discard bool) error {
	c := d.delta
	if c == nil {
		return nil
	}
	d.delta = nil
	if discard {
		c.f.Close()
		os.Remove(c.path)
		os.Remove(c.path + ".enc")
		return nil
	}
	err := d.finalizeAndCloseDelta(c)
	if err == nil && c.count == 0 {
		// A run that stored no new chunk has no change to describe. Publishing
		// an empty object anyway would leave one more thing for every reader to
		// LIST and GET until the next compaction — and a fully-deduped backup
		// is the common case on a fleet sharing a repo, which is the case this
		// issue exists to make good.
		os.Remove(c.path)
		return nil
	}
	// An encrypted repo's index does not rest in the clear. The journal has
	// to stay plaintext WHILE the run appends to it (same as hash-index.db's
	// working copy); it goes away the moment the run is finished with it.
	if err == nil && d.key != nil {
		os.Remove(c.path)
	}
	return err
}

func (d *DedupIndex) finalizeAndCloseDelta(c *deltaCapture) error {
	d.delta = c
	err := d.WriteDeltaObject()
	d.delta = nil
	if cerr := c.f.Close(); err == nil {
		err = cerr
	}
	return err
}
