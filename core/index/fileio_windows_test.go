//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// TestIdleHandleReadAt reproduces the exact scenario that caused
// ERROR_INVALID_HANDLE on GitHub Actions: open a file, write a header via
// standard Write (so the handle is not brand-new), then let it sit idle
// (no I/O) and read back via fileReadAt. Go's os.File.ReadAt calls
// SetFilePointerEx before ReadFile, which fails on some Windows
// configurations for idle handles. Our fileReadAt bypasses SetFilePointerEx.
func TestIdleHandleReadAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idle.htab")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Write an htab-style header: magic (8B) + numSlots (8B) + count (8B) = 24B.
	hdr := make([]byte, htabHeaderSize)
	copy(hdr[0:8], htabMagic[:])
	binary.LittleEndian.PutUint64(hdr[8:16], 16) // numSlots = 16
	binary.LittleEndian.PutUint64(hdr[16:24], 0) // count = 0
	if _, err := f.Write(hdr); err != nil {
		t.Fatal(err)
	}

	// Extend file to full htab size (header + 16 slots × EntrySize).
	totalSize := int64(htabHeaderSize) + 16*EntrySize
	if err := f.Truncate(totalSize); err != nil {
		t.Fatal(err)
	}

	// Close and reopen in O_RDWR — simulates how openDiskHashTable works.
	f.Close()
	f, err = os.OpenFile(path, os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// The handle is now idle (no I/O since open). Read the header back.
	got := make([]byte, htabHeaderSize)
	n, err := fileReadAt(f, got, 0)
	if err != nil {
		t.Fatalf("fileReadAt on idle handle: %v", err)
	}
	if n != htabHeaderSize {
		t.Fatalf("fileReadAt: read %d bytes, want %d", n, htabHeaderSize)
	}
	if !bytes.Equal(got, hdr) {
		t.Errorf("header mismatch: got %x, want %x", got, hdr)
	}

	// Read slot 0 (all zeros — empty slot).
	slot := make([]byte, EntrySize)
	n, err = fileReadAt(f, slot, int64(htabHeaderSize))
	if err != nil {
		t.Fatalf("fileReadAt slot 0 on idle handle: %v", err)
	}
	if n != int(EntrySize) {
		t.Fatalf("slot 0: read %d bytes, want %d", n, EntrySize)
	}
	for i, b := range slot {
		if b != 0 {
			t.Fatalf("slot 0 byte %d = 0x%02x, want 0x00", i, b)
		}
	}
}

// TestBufioWriteThenFileReadAt mimics the buildHashTable bucket workflow:
// data is written through a bufio.Writer, flushed, then read back with
// fileReadAt. This verifies that our OVERLAPPED-based read works after
// buffered writes that may leave the kernel file position in an
// unpredictable state.
func TestBufioWriteThenFileReadAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bucket.tmp")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Write several entries through bufio.
	bw := bufio.NewWriter(f)
	var entries [][]byte
	for i := 0; i < 10; i++ {
		entry := make([]byte, EntrySize)
		entry[0] = byte(i + 1) // non-zero first byte to distinguish entries
		entry[EntrySize-1] = byte(i + 1)
		entries = append(entries, entry)
		if _, err := bw.Write(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := bw.Flush(); err != nil {
		t.Fatal(err)
	}

	// Read every entry back with fileReadAt at computed offsets.
	for i, want := range entries {
		off := int64(i) * EntrySize
		got := make([]byte, EntrySize)
		n, err := fileReadAt(f, got, off)
		if err != nil {
			t.Fatalf("entry %d (off %d): fileReadAt: %v", i, off, err)
		}
		if n != int(EntrySize) {
			t.Fatalf("entry %d: read %d bytes, want %d", i, n, EntrySize)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("entry %d: got[0]=0x%02x got[%d]=0x%02x, want[0]=0x%02x want[%d]=0x%02x",
				i, got[0], EntrySize-1, got[EntrySize-1], want[0], EntrySize-1, want[EntrySize-1])
		}
	}
}

// TestHeaderPatchViaFileWriteAt mimics the htab Close() flow: the count field
// at offset 16 is patched with fileWriteAt after the file was created and
// populated. Verifies that a targeted write at a specific offset doesn't
// corrupt surrounding header fields.
func TestHeaderPatchViaFileWriteAt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch.htab")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Write full header.
	hdr := make([]byte, htabHeaderSize)
	copy(hdr[0:8], htabMagic[:])
	binary.LittleEndian.PutUint64(hdr[8:16], 32) // numSlots = 32
	binary.LittleEndian.PutUint64(hdr[16:24], 0) // count = 0
	if _, err := f.Write(hdr); err != nil {
		t.Fatal(err)
	}
	// Extend to at least header size.
	if err := f.Truncate(int64(htabHeaderSize) + 32*EntrySize); err != nil {
		t.Fatal(err)
	}

	// Patch count field to 17 via fileWriteAt (same pattern as Close()).
	var countBuf [8]byte
	binary.LittleEndian.PutUint64(countBuf[:], 17)
	n, err := fileWriteAt(f, countBuf[:], 16)
	if err != nil {
		t.Fatalf("fileWriteAt count patch: %v", err)
	}
	if n != 8 {
		t.Fatalf("wrote %d bytes, want 8", n)
	}

	// Read entire header back and verify all fields.
	got := make([]byte, htabHeaderSize)
	if _, err := fileReadAt(f, got, 0); err != nil {
		t.Fatalf("fileReadAt header: %v", err)
	}

	// Magic should be unchanged.
	var magic [8]byte
	copy(magic[:], got[0:8])
	if magic != htabMagic {
		t.Errorf("magic corrupted: got %x, want %x", magic, htabMagic)
	}
	// numSlots should be unchanged.
	numSlots := binary.LittleEndian.Uint64(got[8:16])
	if numSlots != 32 {
		t.Errorf("numSlots corrupted: got %d, want 32", numSlots)
	}
	// count should be patched.
	count := binary.LittleEndian.Uint64(got[16:24])
	if count != 17 {
		t.Errorf("count: got %d, want 17", count)
	}
}

// TestOverlappedHighOffset verifies that fileReadAt/fileWriteAt correctly
// encode offsets > 4 GB using the OffsetHigh field of the OVERLAPPED struct.
// This guards against truncation bugs in the 32-bit offset split.
func TestOverlappedHighOffset(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-offset test in short mode")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "bigoff.dat")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Write at an offset just past the 4 GB boundary.
	// We use sparse file support — only the written region allocates disk.
	const highOff = int64(1)<<32 + 4096 // 4 GiB + 4 KiB
	payload := []byte("above-4gb-boundary")
	n, err := fileWriteAt(f, payload, highOff)
	if err != nil {
		t.Fatalf("fileWriteAt at high offset: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("wrote %d, want %d", n, len(payload))
	}

	// Read it back.
	got := make([]byte, len(payload))
	n, err = fileReadAt(f, got, highOff)
	if err != nil {
		t.Fatalf("fileReadAt at high offset: %v", err)
	}
	if n != len(payload) || !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got[:n], payload)
	}
}

// TestInterleavedWriteReadAt exercises rapid alternation between fileWriteAt
// and fileReadAt on the same handle — stresses the OVERLAPPED-based I/O path
// to catch any state leakage between operations.
func TestInterleavedWriteReadAt(t *testing.T) {
	f := createTempIndexFile(t)

	for i := 0; i < 100; i++ {
		off := int64(i) * 64
		data := bytes.Repeat([]byte{byte(i)}, 64)
		if _, err := fileWriteAt(f, data, off); err != nil {
			t.Fatalf("iter %d write: %v", i, err)
		}
		// Immediately read back.
		got := make([]byte, 64)
		if _, err := fileReadAt(f, got, off); err != nil {
			t.Fatalf("iter %d read: %v", i, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("iter %d: data mismatch at offset %d", i, off)
		}
	}

	// Verify earlier writes weren't corrupted by later ones.
	for i := 0; i < 100; i++ {
		off := int64(i) * 64
		want := bytes.Repeat([]byte{byte(i)}, 64)
		got := make([]byte, 64)
		if _, err := fileReadAt(f, got, off); err != nil {
			t.Fatalf("verify iter %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("verify iter %d: data corrupted", i)
		}
	}
}
