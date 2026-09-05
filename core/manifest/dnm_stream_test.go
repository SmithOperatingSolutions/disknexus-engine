// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func streamedEntries(n int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		var h [32]byte
		h[0], h[1] = byte(i), byte(i>>8)
		out[i] = Entry{VolumeOffset: int64(i) * 4096, ChunkHash: h, ChunkLength: 4096}
	}
	out[3].IsExcluded = true
	return out
}

// TestStreamedDNMEquivalence is lever 4's core guarantee: a manifest assembled
// from streamed parts (bounded buffer, entries never on local disk) must be a
// valid .dnm the existing reader opens, with identical metadata and entries.
func TestStreamedDNMEquivalence(t *testing.T) {
	entries := streamedEntries(500) // 22,500 entry bytes
	b := &Backup{
		BackupID:     "streamed-eq",
		Timestamp:    time.Now().UTC().Truncate(time.Millisecond),
		SourceVolume: "C:",
		BackupType:   "full",
		TotalBytes:   500 * 4096,
		TotalChunks:  500,
		UniqueChunks: 499,
		StoredBytes:  1 << 20,
	}

	const window = 4096
	var parts [][]byte
	st := NewDNMStreamer(window, func(p []byte) error {
		parts = append(parts, append([]byte(nil), p...))
		return nil
	})
	for _, e := range entries {
		if err := st.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
		if buffered := st.Buffered(); buffered > window+EntryRecordSize {
			t.Fatalf("peak buffer %d exceeds window bound", buffered)
		}
	}
	if err := st.Finish(b); err != nil {
		t.Fatal(err)
	}
	if len(parts) < 3 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	// S3 multipart rule: every part except the last must be >= the window
	// (the real window is 8 MB >= the 5 MB minimum).
	for i, p := range parts[:len(parts)-1] {
		if len(p) < window {
			t.Fatalf("part %d is %d bytes, below window", i, len(p))
		}
	}

	// Reassemble (what the controller's compose does) and read back.
	path := filepath.Join(t.TempDir(), "s.dnm")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		f.Write(p)
	}
	f.Close()

	r, err := OpenDNMReader(path)
	if err != nil {
		t.Fatalf("reader rejected streamed dnm: %v", err)
	}
	defer r.Close()
	if r.EntriesCount() != int64(len(entries)) {
		t.Fatalf("EntriesCount = %d, want %d", r.EntriesCount(), len(entries))
	}
	got, err := r.EntriesRange(0, uint64(len(entries)))
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if got[i] != entries[i] {
			t.Fatalf("entry %d mismatch: %+v != %+v", i, got[i], entries[i])
		}
	}
	meta, err := r.readMetadata()
	if err != nil {
		t.Fatal(err)
	}
	if meta.BackupID != b.BackupID || meta.TotalChunks != b.TotalChunks {
		t.Fatalf("metadata mismatch: %+v", meta)
	}
}

// TestStreamedDNMUsesTrailer: the streamed header carries the zero sentinel
// (the section-index offset is unknowable up front on an immutable store) and
// the reader must find the index via the 8-byte trailer.
func TestStreamedDNMUsesTrailer(t *testing.T) {
	var parts [][]byte
	st := NewDNMStreamer(1<<20, func(p []byte) error {
		parts = append(parts, append([]byte(nil), p...))
		return nil
	})
	for _, e := range streamedEntries(10) {
		if err := st.WriteEntry(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish(&Backup{BackupID: "trailer", BackupType: "full"}); err != nil {
		t.Fatal(err)
	}
	blob := []byte{}
	for _, p := range parts {
		blob = append(blob, p...)
	}
	if off := binary.LittleEndian.Uint64(blob[12:20]); off != 0 {
		t.Fatalf("streamed header SectionIndexOffset = %d, want 0 sentinel", off)
	}
	path := filepath.Join(t.TempDir(), "t.dnm")
	if err := os.WriteFile(path, blob, 0644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDNMReader(path)
	if err != nil {
		t.Fatalf("trailer path failed: %v", err)
	}
	r.Close()

	// A corrupted trailer must be a clean error, not a panic or bogus read.
	blob[len(blob)-3] ^= 0xFF
	if err := os.WriteFile(path, blob, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDNMReader(path); err == nil {
		t.Fatal("corrupted trailer accepted")
	}
}
