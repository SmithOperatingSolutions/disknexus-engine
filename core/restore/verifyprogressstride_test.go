// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// sliceAccessor serves entries from memory — granularity is about the
// CALLBACK, not the read path.
type sliceAccessor struct{ entries []manifest.Entry }

func (a sliceAccessor) Count() int64                       { return int64(len(a.entries)) }
func (a sliceAccessor) At(i int64) (manifest.Entry, error) { return a.entries[i], nil }
func (a sliceAccessor) Range(lo, hi int64) ([]manifest.Entry, error) {
	return a.entries[lo:hi], nil
}

// Owner ask (2026-08-29): progress finer than once per 4096-entry window.
// On the fleet's slow verifies a window can take minutes (the a00b2ce7
// incident: 11 minutes to the FIRST progress line), during which the panel's
// percent and the log are frozen — indistinguishable from a hang. The read
// window stays (it bounds memory); the CALLBACK must not be chained to it.
func TestVerifyProgressIsFinerThanTheReadWindow(t *testing.T) {
	_, dedupIdx, chunkStore, chunkData, chunkHash := setupTestRepo(t)
	defer dedupIdx.Close()
	defer chunkStore.Close()

	const n = 1000 // well inside ONE 4096 window
	entries := make([]manifest.Entry, n)
	off := int64(0)
	for i := range entries {
		entries[i] = manifest.Entry{VolumeOffset: off, ChunkHash: chunkHash, ChunkLength: len(chunkData)}
		off += int64(len(chunkData))
	}
	b := &manifest.Backup{BackupID: "stride", TotalBytes: off}

	var calls int
	var lastDone int64
	res, err := VerifyStreamed(context.Background(), b, sliceAccessor{entries}, dedupIdx, chunkStore, nil,
		func(done, total int64) {
			calls++
			if done < lastDone {
				t.Fatalf("progress went backwards: %d after %d", done, lastDone)
			}
			lastDone = done
			if total != n {
				t.Fatalf("total = %d, want %d", total, n)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() {
		t.Fatalf("fixture defect: verify failed (%v), the call count below measures a broken run", res.Errors)
	}
	if lastDone != n {
		t.Fatalf("final progress %d never reached the total %d", lastDone, n)
	}
	// 1000 entries inside one window used to yield exactly ONE callback (at
	// the window's end). Finer-than-window means many: at a 64-entry stride,
	// ~16. The floor of 8 tolerates a stride up to 128 without chaining the
	// assertion to one constant.
	if calls < 8 {
		t.Fatalf("onProgress fired %d time(s) for %d entries — progress is chained to the 4096 read "+
			"window, so a slow window freezes the panel and the log for its whole duration and a "+
			"stuck verify is indistinguishable from a slow one", calls, n)
	}
}
