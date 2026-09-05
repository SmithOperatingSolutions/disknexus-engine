// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"testing"
	"unsafe"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// synthEntries generates N excluded entries on demand — nothing is held
// between calls, so the only Entry structs alive during a restore are the
// ones the restorer itself is holding.
type synthEntries struct{ n int64 }

func (s synthEntries) Count() int64 { return s.n }
func (s synthEntries) entry(i int64) manifest.Entry {
	return manifest.Entry{VolumeOffset: i * 4096, ChunkLength: 4096, IsExcluded: true}
}
func (s synthEntries) At(i int64) (manifest.Entry, error) { return s.entry(i), nil }
func (s synthEntries) Range(start, end int64) ([]manifest.Entry, error) {
	out := make([]manifest.Entry, end-start)
	for i := range out {
		out[i] = s.entry(start + int64(i))
	}
	return out, nil
}

// lastRangeHook fires inside the Range call that reaches the end of the
// entries — the one moment pass 1 has read everything and still holds
// whatever it holds.
type lastRangeHook struct {
	manifest.EntryAccessor
	onLast func()
}

func (h lastRangeHook) Range(start, end int64) ([]manifest.Entry, error) {
	out, err := h.EntryAccessor.Range(start, end)
	if end == h.Count() && h.onLast != nil {
		h.onLast()
	}
	return out, err
}

type discardTarget struct{ written int64 }

func (d *discardTarget) WriteAt(p []byte, off int64) (int, error) {
	d.written += int64(len(p))
	return len(p), nil
}
func (d *discardTarget) Truncate(int64) error { return nil }
func (d *discardTarget) Sync() error          { return nil }

func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestRestoreEntries_NeverHoldsTheEntriesWhole is the #506 claim: a block
// restore of N entries holds O(batch) Entry structs, not O(N). The heap is
// read at the end of pass 1 (after a forced GC, on the test's own goroutine —
// no sampling race), against a bound of half the size of a materialized
// []Entry. The positive control restores the same N through a slice accessor
// whose slice is alive at that moment, and must exceed the full slice size —
// proving the detector sees the ~48 MB it is guarding against.
func TestRestoreEntries_NeverHoldsTheEntriesWhole(t *testing.T) {
	const n = 1 << 20
	entrySize := int64(unsafe.Sizeof(manifest.Entry{}))
	sliceBytes := n * entrySize
	r := NewRestorer(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	backup := &manifest.Backup{BackupID: "synthetic", TotalBytes: n * 4096}

	measure := func(ea manifest.EntryAccessor, baseline uint64) (delta int64, written int64) {
		var atEnd uint64
		hooked := lastRangeHook{EntryAccessor: ea, onLast: func() { atEnd = liveHeap() }}
		tgt := &discardTarget{}
		res, err := r.RestoreEntries(context.Background(), backup, hooked, tgt)
		if err != nil {
			t.Fatalf("RestoreEntries: %v", err)
		}
		if atEnd == 0 {
			t.Fatal("the end-of-pass-1 hook never fired: the detector measured nothing")
		}
		if res.ExcludedChunks != n || res.TotalChunks != n {
			t.Fatalf("result = %+v, want %d excluded of %d", res, n, n)
		}
		return int64(atEnd) - int64(baseline), tgt.written
	}

	// Streamed: entries generated on demand.
	base := liveHeap()
	delta, written := measure(synthEntries{n: n}, base)
	if written != n*4096 {
		t.Fatalf("streamed restore wrote %d bytes, want %d", written, n*4096)
	}
	t.Logf("streamed: heap at end of pass 1 grew %d MB (materialized slice would be %d MB)", delta>>20, sliceBytes>>20)
	if delta > sliceBytes/2 {
		t.Fatalf("streamed restore held %d MB at the end of pass 1 — more than half a materialized []Entry (%d MB); the entries are being held whole", delta>>20, sliceBytes>>20)
	}

	// Positive control: the same restore over a live []Entry must be seen.
	base = liveHeap()
	all, _ := synthEntries{n: n}.Range(0, n)
	ctrl, _ := measure(manifest.NewSliceEntryAccessor(all), base)
	runtime.KeepAlive(all)
	t.Logf("slice control: heap at end of pass 1 grew %d MB", ctrl>>20)
	if ctrl < sliceBytes {
		t.Fatalf("positive control: a live []Entry of %d MB registered only %d MB — the heap detector cannot see what it guards", sliceBytes>>20, ctrl>>20)
	}
}
