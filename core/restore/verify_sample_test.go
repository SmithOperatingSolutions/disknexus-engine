// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore_test

import (
	"context"
	"crypto/sha256"
	"math"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// TestVerifyWithNormalizer_ZeroEntriesGuard guards issue #40 (parity with the
// restore zero-entries guard): a backup that claims bytes but has no entries
// must FAIL verify rather than loop zero times and report a false PASS.
func TestVerifyWithNormalizer_ZeroEntriesGuard(t *testing.T) {
	backup := &manifest.Backup{BackupID: "zzzz", TotalBytes: 1024} // Entries nil
	_, err := restore.VerifyWithNormalizer(context.Background(), backup, nil, nil, nil)
	if err == nil {
		t.Fatal("zero-entries backup claiming 1024 bytes must fail verify, not report OK")
	}
}

// TestSampleEntryIndices covers the pure sampler: count/clamp, no excluded
// index, reproducibility, 100% = all, out-of-range rejected, empty population.
func TestSampleEntryIndices(t *testing.T) {
	mk := func(n int, excluded map[int]bool) *manifest.Backup {
		b := &manifest.Backup{BackupID: "seed-backup"}
		for i := 0; i < n; i++ {
			b.Entries = append(b.Entries, manifest.Entry{VolumeOffset: int64(i), IsExcluded: excluded[i]})
		}
		return b
	}
	backup := mk(100, map[int]bool{3: true, 7: true, 50: true}) // 97 verifiable

	got, err := restore.SampleEntryIndices(backup, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 10 { // ceil(0.10*97)=10
		t.Fatalf("len = %d, want 10", len(got))
	}
	for _, i := range got {
		if backup.Entries[i].IsExcluded {
			t.Fatalf("selected an excluded index %d", i)
		}
	}
	// Reproducible.
	got2, _ := restore.SampleEntryIndices(backup, 10, 0)
	if len(got) != len(got2) {
		t.Fatal("nondeterministic count")
	}
	for i := range got {
		if got[i] != got2[i] {
			t.Fatal("sample not reproducible for same (backup,percent,seed)")
		}
	}
	// Different seed → (very likely) different subset.
	got3, _ := restore.SampleEntryIndices(backup, 10, 42)
	same := len(got) == len(got3)
	if same {
		for i := range got {
			if got[i] != got3[i] {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("different seed produced an identical subset (seed not mixed in)")
	}
	// 100% → all verifiable.
	all, _ := restore.SampleEntryIndices(backup, 100, 0)
	if len(all) != 97 {
		t.Fatalf("100%% sample = %d, want 97", len(all))
	}
	// min-1 clamp.
	one, _ := restore.SampleEntryIndices(backup, 0.0001, 0)
	if len(one) != 1 {
		t.Fatalf("tiny sample = %d, want 1", len(one))
	}
	// Out of range.
	if _, err := restore.SampleEntryIndices(backup, 0, 0); err == nil {
		t.Fatal("percent=0 must error")
	}
	if _, err := restore.SampleEntryIndices(backup, 101, 0); err == nil {
		t.Fatal("percent=101 must error")
	}
	// Non-finite: NaN slips past a naive `p<=0 || p>100` guard (all NaN
	// comparisons are false) and would silently sample ~1 chunk.
	if _, err := restore.SampleEntryIndices(backup, math.NaN(), 0); err == nil {
		t.Fatal("percent=NaN must error")
	}
	if _, err := restore.SampleEntryIndices(backup, math.Inf(1), 0); err == nil {
		t.Fatal("percent=+Inf must error")
	}
	// Empty verifiable population.
	empty, err := restore.SampleEntryIndices(mk(3, map[int]bool{0: true, 1: true, 2: true}), 50, 0)
	if err != nil || len(empty) != 0 {
		t.Fatalf("all-excluded → empty, got %v err=%v", empty, err)
	}
}

// TestVerifySelectedWithNormalizer_TrueIndex proves selected verification uses
// the TRUE manifest index in errors, and that excluding the bad chunk passes.
func TestVerifySelectedWithNormalizer_TrueIndex(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cs, err := store.NewChunkStore(dir, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	idx, err := index.NewDedupIndex(filepath.Join(dir, "index"), 1000, cfg.BloomFPRate, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Three good chunks at manifest indices 0,1,2.
	var entries []manifest.Entry
	for i := 0; i < 3; i++ {
		data := []byte{byte(i), byte(i + 1), 9, 9, 9, 9, 9, 9}
		p, off, _, err := cs.Store(data)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data)
		idx.Insert(makeChunkID(h), p, uint64(off), uint32(len(data)))
		entries = append(entries, manifest.Entry{VolumeOffset: int64(i) * 8, ChunkHash: h, ChunkLength: len(data)})
	}
	// Chunk at index 1 is "corrupt": store a hash that won't match its data.
	badData := []byte{5, 5, 5, 5, 5, 5, 5, 5}
	p, off, _, _ := cs.Store(badData)
	var wrong [32]byte
	wrong[0] = 0xEE
	idx.Insert(makeChunkID(wrong), p, uint64(off), uint32(len(badData)))
	entries[1] = manifest.Entry{VolumeOffset: 8, ChunkHash: wrong, ChunkLength: len(badData)}

	backup := &manifest.Backup{BackupID: "b", TotalBytes: 24, Entries: entries}

	// (a) selecting the corrupt entry → error at true index 1.
	r, err := restore.VerifySelectedWithNormalizer(context.Background(), backup, idx, cs, nil, []int{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() {
		t.Fatal("expected verification failure for the corrupt chunk")
	}
	if len(r.Errors) != 1 || r.Errors[0].ChunkIndex != 1 {
		t.Fatalf("error ChunkIndex = %v, want the true manifest index 1", r.Errors)
	}

	// (b) selecting only good entries → OK.
	r2, err := restore.VerifySelectedWithNormalizer(context.Background(), backup, idx, cs, nil, []int{0, 2})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.OK() || r2.VerifiedChunks != 2 {
		t.Fatalf("good-only selection should pass, got %+v", r2)
	}
}
