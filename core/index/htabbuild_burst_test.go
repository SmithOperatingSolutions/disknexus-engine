// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// #498: opening a repository index rebuilds the whole .htab, and the build's
// peak allocation grew with REPO size — htabBuildK was a fixed 8, sized so a
// 22M-entry index peaks at ~440 MB. On the 512Mi controller
// (GOMEMLIMIT=410MB) that is an instant OOM: an 8 KB backup's verify killed
// the pod in 4 seconds, before #496's staging plan could refuse anything,
// because the 264.9 GB repo's ~16M-entry index put the build right at the
// design target. The number of segments must scale with the index so the
// per-segment residency (slot array + bucket data + sort arrays, ~80 B per
// slot) stays CONSTANT, trading temp-file count for a bounded burst.

// writeSortedIndexFile fabricates an n-entry hash-index.db whose entries are
// sorted by their 8-byte hash prefix (sequential big-endian prefixes — the
// order the real writer's sort produces).
func writeSortedIndexFile(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const batch = 8192
	buf := make([]byte, batch*EntrySize)
	written := 0
	for written < n {
		m := batch
		if n-written < m {
			m = n - written
		}
		for i := 0; i < m; i++ {
			e := buf[i*EntrySize : (i+1)*EntrySize]
			for j := range e {
				e[j] = 0
			}
			binary.BigEndian.PutUint64(e[0:8], uint64(written+i)<<8|1)
			binary.LittleEndian.PutUint32(e[32:36], uint32(i%977))
			binary.LittleEndian.PutUint64(e[36:44], uint64(i)*4096)
			binary.LittleEndian.PutUint32(e[44:48], 4096)
		}
		if _, err := f.Write(buf[:m*EntrySize]); err != nil {
			t.Fatal(err)
		}
		written += m
	}
}

func TestHashTableBuildPeakIsBoundedRegardlessOfIndexSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash-index.db")
	const n = 6_000_000 // ~288 MB on disk; fixed K=8 peaks well past 100 MB here
	writeSortedIndexFile(t, path, n)

	// Peak LIVE heap during the build, watched from outside: the burst is a
	// single allocation sequence hundreds of MB tall and hundreds of
	// milliseconds wide, far above the sampler's 1ms grain and the cap's
	// slack (#476 discipline: the cap sits between GC noise and the defect).
	runtime.GC()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak atomic.Int64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Forced collection: HeapAlloc without it is live PLUS
			// platform-dependent float (the #511 lesson) — the cap must
			// bound LIVE.
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if d := int64(m.HeapAlloc) - int64(base.HeapAlloc); d > peak.Load() {
				peak.Store(d)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()

	idx, err := NewHashIndex(path, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-sampled

	// Positive control (§4): the table the bounded build produced still
	// answers — first, last, and a middle entry.
	for _, i := range []uint64{0, n / 2, n - 1} {
		var h [32]byte
		binary.BigEndian.PutUint64(h[0:8], i<<8|1)
		if _, found, lerr := idx.htab.Lookup(h); lerr != nil || !found {
			idx.CloseDiscard()
			t.Fatalf("entry %d missing from the built table (err %v) — the bound below is measured on a "+
				"build that lost data", i, lerr)
		}
	}
	idx.CloseDiscard()

	const cap = 96 << 20
	if got := peak.Load(); got > cap {
		t.Fatalf("building the hash table for a %d-entry index held %d MB of heap at peak (cap %d MB) — "+
			"the burst scales with REPO size, and at the fleet's 16M-entry index it is ~440 MB: an 8 KB "+
			"backup's verify OOM-kills the 512Mi controller in 4 seconds, before any budget gate can "+
			"refuse (#498)", n, got>>20, cap>>20)
	}
	t.Logf("build peak: %d MB over %d entries", peak.Load()>>20, n)
}
