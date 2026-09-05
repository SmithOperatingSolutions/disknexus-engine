// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest_test

import (
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func mkEntry(off int64) manifest.Entry {
	var h [32]byte
	h[0] = byte(off)
	return manifest.Entry{VolumeOffset: off, ChunkHash: h, ChunkLength: int(off) + 1}
}

// TestEntryWriter_SyncAndLenReflectOnDisk guards issue #42 §8-C: Sync must make
// buffered entries durable and Len must report the authoritative on-disk length,
// not a buffered logical count. Pre-fix these methods don't exist; the 1 MiB
// bufio buffer holds entries with nothing on disk until Close.
func TestEntryWriter_SyncAndLenReflectOnDisk(t *testing.T) {
	dir := t.TempDir()
	w, err := manifest.OpenEntryWriter(dir, "b1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 5; i++ {
		if err := w.WriteEntry(mkEntry(int64(i * 8))); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	n, err := w.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if want := int64(5 * manifest.EntryRecordSize); n != want {
		t.Fatalf("Len = %d, want %d", n, want)
	}
	// The bytes are actually on disk (independent stat), not just buffered.
	info, err := os.Stat(manifest.EntriesPath(dir, "b1"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(5*manifest.EntryRecordSize) {
		t.Fatalf("on-disk size = %d, want %d (Sync did not flush)", info.Size(), 5*manifest.EntryRecordSize)
	}
}

// TestOpenEntryWriterResume_TruncatesAndAppends: reopening at wantLen drops the
// un-checkpointed tail and appends cleanly; the boundary record appears once and
// offsets stay strictly increasing.
func TestOpenEntryWriterResume_TruncatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	w, err := manifest.OpenEntryWriter(dir, "b2")
	if err != nil {
		t.Fatal(err)
	}
	// Run 1: 4 entries, but only the first 3 are "checkpointed".
	for i := 0; i < 4; i++ {
		if err := w.WriteEntry(mkEntry(int64(i * 8))); err != nil {
			t.Fatal(err)
		}
	}
	wantLen := int64(3 * manifest.EntryRecordSize)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Resume: truncate to 3 records, append 2 more.
	rw, err := manifest.OpenEntryWriterResume(dir, "b2", wantLen)
	if err != nil {
		t.Fatalf("OpenEntryWriterResume: %v", err)
	}
	if err := rw.WriteEntry(mkEntry(24)); err != nil { // the re-processed boundary + suffix
		t.Fatal(err)
	}
	if err := rw.WriteEntry(mkEntry(32)); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := manifest.ReadEntries(dir, "b2")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5 (3 kept + 2 appended)", len(entries))
	}
	var prev int64 = -1
	for i, e := range entries {
		if e.VolumeOffset <= prev {
			t.Fatalf("entry %d offset %d not strictly increasing (prev %d)", i, e.VolumeOffset, prev)
		}
		prev = e.VolumeOffset
	}
	// The dropped tail record (offset 24 from run 1) must not appear twice.
	if entries[3].VolumeOffset != 24 || entries[4].VolumeOffset != 32 {
		t.Fatalf("appended offsets = %d,%d, want 24,32", entries[3].VolumeOffset, entries[4].VolumeOffset)
	}
}

// TestOpenEntryWriterResume_RefusesShortFileNeverZeroExtends guards the core
// §8-C safety property: if the on-disk sidecar is SHORTER than the checkpoint's
// EntriesLen, resume must ERROR — never ftruncate-grow it with zero records
// (which restore would read as fabricated entries).
func TestOpenEntryWriterResume_RefusesShortFileNeverZeroExtends(t *testing.T) {
	dir := t.TempDir()
	if err := manifest.WriteEntries(dir, "b3", []manifest.Entry{mkEntry(0), mkEntry(8)}); err != nil {
		t.Fatal(err)
	}
	// Checkpoint claims 5 records but only 2 are on disk.
	wantLen := int64(5 * manifest.EntryRecordSize)
	rw, err := manifest.OpenEntryWriterResume(dir, "b3", wantLen)
	if err == nil {
		rw.Close()
		t.Fatal("expected refusal when sidecar is shorter than checkpoint EntriesLen")
	}
	// File must be untouched (still 2 records), definitely not zero-extended to 5.
	info, err := os.Stat(manifest.EntriesPath(dir, "b3"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(2*manifest.EntryRecordSize) {
		t.Fatalf("sidecar size changed to %d; must stay %d", info.Size(), 2*manifest.EntryRecordSize)
	}
}
