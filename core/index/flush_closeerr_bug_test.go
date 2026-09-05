// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
)

// TestFlushToleratesStaleFileHandleClose proves that Flush does not abort when
// closing the current index file descriptor fails. On some Windows
// configurations (GitHub Actions) idx.file sits idle for the whole backup
// (reads go through the hash table) and its handle returns ERROR_INVALID_HANDLE
// from Close — this previously failed the entire backup with "closing index
// before rename". The close only exists to release the OS lock before the
// rename, so a close error must not be fatal.
//
// The stale handle is simulated portably by closing idx.file before Flush, so
// Flush's own Close returns an error (os.ErrClosed on Linux/macOS).
func TestFlushToleratesStaleFileHandleClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hash-index.db")
	idx, err := NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("NewHashIndex: %v", err)
	}

	ids := []hasher.ChunkID{chunkIDFromByte(1), chunkIDFromByte(2), chunkIDFromByte(3)}
	for i, id := range ids {
		idx.Insert(id, uint32(i), uint64(i)*100, 100)
	}
	if err := idx.FlushDelta(); err != nil {
		t.Fatalf("FlushDelta: %v", err)
	}
	// Add one more so Flush has work to do (dirty).
	idx.Insert(chunkIDFromByte(9), 9, 900, 100)

	// Simulate the stale/invalid handle: Flush's idx.file.Close() will now
	// return an error.
	if idx.file != nil {
		_ = idx.file.Close()
	}

	if err := idx.Flush(); err != nil {
		t.Fatalf("Flush aborted on a stale-handle close error: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// All entries must have survived the flush.
	verify, err := NewHashIndex(path, 0, false)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer verify.Close()
	for _, id := range append(ids, chunkIDFromByte(9)) {
		if _, ok, err := verify.Lookup(id); err != nil || !ok {
			t.Errorf("entry %x missing after flush (ok=%v err=%v)", id.StrongHash[0], ok, err)
		}
	}
}
