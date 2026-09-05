// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store_test

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

func TestChunkStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	// Store some chunks
	chunks := make([][]byte, 10)
	type loc struct {
		pack   uint32
		offset int64
	}
	locs := make([]loc, 10)

	for i := range chunks {
		chunks[i] = make([]byte, 8192)
		rand.Read(chunks[i])

		packNum, offset, _, err := cs.Store(chunks[i])
		if err != nil {
			t.Fatalf("Store chunk %d: %v", i, err)
		}
		locs[i] = loc{packNum, offset}
	}

	// Retrieve and verify
	for i := range chunks {
		data, err := cs.Retrieve(locs[i].pack, locs[i].offset)
		if err != nil {
			t.Fatalf("Retrieve chunk %d: %v", i, err)
		}
		if !bytes.Equal(data, chunks[i]) {
			t.Errorf("chunk %d data mismatch", i)
		}
	}
}

func TestChunkStorePackRotation(t *testing.T) {
	dir := t.TempDir()

	// Small max pack size to force rotation
	cs, err := store.NewChunkStore(dir, 1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	// Store enough data to trigger rotation
	data := make([]byte, 512)
	rand.Read(data)

	var packNums []uint32
	for i := 0; i < 10; i++ {
		packNum, _, _, err := cs.Store(data)
		if err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
		packNums = append(packNums, packNum)
	}

	// Should have used multiple pack files
	stats := cs.Stats()
	if stats.PackFiles < 2 {
		t.Errorf("expected multiple pack files, got %d", stats.PackFiles)
	}
	t.Logf("pack files: %d, total bytes: %d", stats.PackFiles, stats.TotalBytes)
}

func TestChunkStoreCompression(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	// Store compressible data (all zeros)
	data := make([]byte, 65536)
	_, _, compressedSize, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if compressedSize >= len(data) {
		t.Errorf("compression ineffective: %d compressed vs %d raw", compressedSize, len(data))
	}

	t.Logf("compression ratio: %.1fx (%d -> %d bytes)",
		float64(len(data))/float64(compressedSize), len(data), compressedSize)
}

func TestInitRepo(t *testing.T) {
	dir := t.TempDir()

	cfg := store.RepoConfig{
		ChunkMinSize:     4096,
		ChunkAvgSize:     8192,
		ChunkMaxSize:     65536,
		BuzhashMask:      0x1FFF,
		PackFileMaxSize:  512 * 1024 * 1024,
		CompressionLevel: 3,
	}

	if err := store.InitRepo(dir, cfg); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	loaded, err := store.LoadRepoConfig(dir)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}

	if loaded.Version != 1 {
		t.Errorf("expected version 1, got %d", loaded.Version)
	}
	if loaded.ChunkAvgSize != 8192 {
		t.Errorf("expected avg size 8192, got %d", loaded.ChunkAvgSize)
	}
}

func TestChunkStoreEncryptedRoundTrip(t *testing.T) {
	dir := t.TempDir()

	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3, mk)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	chunks := make([][]byte, 10)
	type loc struct {
		pack   uint32
		offset int64
	}
	locs := make([]loc, 10)

	for i := range chunks {
		chunks[i] = make([]byte, 8192)
		rand.Read(chunks[i])

		packNum, offset, _, err := cs.Store(chunks[i])
		if err != nil {
			t.Fatalf("Store chunk %d: %v", i, err)
		}
		locs[i] = loc{packNum, offset}
	}

	for i := range chunks {
		data, err := cs.Retrieve(locs[i].pack, locs[i].offset)
		if err != nil {
			t.Fatalf("Retrieve chunk %d: %v", i, err)
		}
		if !bytes.Equal(data, chunks[i]) {
			t.Errorf("chunk %d data mismatch", i)
		}
	}
}

func TestChunkStoreRawPassThrough(t *testing.T) {
	dir := t.TempDir()

	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3, mk)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	// Store a chunk normally
	data := make([]byte, 4096)
	rand.Read(data)
	packNum, offset, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Retrieve raw frame
	frame, _, err := cs.RetrieveRaw(packNum, offset)
	if err != nil {
		t.Fatalf("RetrieveRaw: %v", err)
	}

	// Store raw frame into a new store (also encrypted)
	dir2 := t.TempDir()
	cs2, err := store.NewChunkStore(dir2, 512*1024*1024, 3, mk)
	if err != nil {
		t.Fatalf("NewChunkStore2: %v", err)
	}
	defer cs2.Close()

	newPack, newOffset, _, err := cs2.StoreRaw(frame)
	if err != nil {
		t.Fatalf("StoreRaw: %v", err)
	}

	// Retrieve from new store should give back original data
	retrieved, err := cs2.Retrieve(newPack, newOffset)
	if err != nil {
		t.Fatalf("Retrieve from raw: %v", err)
	}

	if !bytes.Equal(data, retrieved) {
		t.Error("raw pass-through data mismatch")
	}
}

func TestOnPackSealedRotation(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	type sealedInfo struct {
		path    string
		packNum uint32
		size    int64
	}
	var mu sync.Mutex
	var sealed []sealedInfo

	cs.OnPackSealed = func(packPath string, packNum uint32, size int64) error {
		mu.Lock()
		defer mu.Unlock()
		sealed = append(sealed, sealedInfo{packPath, packNum, size})
		return nil
	}

	data := make([]byte, 512)
	rand.Read(data)
	for i := 0; i < 10; i++ {
		if _, _, _, err := cs.Store(data); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	// Close triggers callback for the final pack
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(sealed) < 2 {
		t.Fatalf("expected at least 2 callback calls (rotations + close), got %d", len(sealed))
	}

	// Pack numbers should be sequential starting at 0
	for i, s := range sealed {
		if s.packNum != uint32(i) {
			t.Errorf("sealed[%d].packNum = %d, want %d", i, s.packNum, i)
		}
		if s.size <= 0 {
			t.Errorf("sealed[%d].size = %d, want > 0", i, s.size)
		}
		expected := cs.PackPath(s.packNum)
		if s.path != expected {
			t.Errorf("sealed[%d].path = %q, want %q", i, s.path, expected)
		}
	}
}

func TestOnPackSealedClose(t *testing.T) {
	dir := t.TempDir()

	// Large maxPackSize so no rotation occurs
	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	type sealedInfo struct {
		path    string
		packNum uint32
		size    int64
	}
	var sealed []sealedInfo

	cs.OnPackSealed = func(packPath string, packNum uint32, size int64) error {
		sealed = append(sealed, sealedInfo{packPath, packNum, size})
		return nil
	}

	data := make([]byte, 4096)
	rand.Read(data)
	for i := 0; i < 3; i++ {
		if _, _, _, err := cs.Store(data); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	if len(sealed) != 0 {
		t.Fatalf("expected 0 callbacks before Close, got %d", len(sealed))
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if len(sealed) != 1 {
		t.Fatalf("expected exactly 1 callback after Close, got %d", len(sealed))
	}
	if sealed[0].packNum != 0 {
		t.Errorf("packNum = %d, want 0", sealed[0].packNum)
	}
	if sealed[0].size <= 0 {
		t.Errorf("size = %d, want > 0", sealed[0].size)
	}
}

func TestOnPackSealedNil(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	// No callback set — should not panic
	data := make([]byte, 512)
	rand.Read(data)
	for i := 0; i < 10; i++ {
		if _, _, _, err := cs.Store(data); err != nil {
			t.Fatalf("Store %d: %v", i, err)
		}
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestOnPackSealedError(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	cbErr := fmt.Errorf("upload failed")
	cs.OnPackSealed = func(packPath string, packNum uint32, size int64) error {
		return cbErr
	}

	data := make([]byte, 512)
	rand.Read(data)

	var storeErr error
	for i := 0; i < 10; i++ {
		_, _, _, err := cs.Store(data)
		if err != nil {
			storeErr = err
			break
		}
	}

	if storeErr == nil {
		t.Fatal("expected Store to return an error from callback")
	}
	if !errors.Is(storeErr, cbErr) {
		t.Errorf("error = %v, want wrapping %v", storeErr, cbErr)
	}
}

func TestOnPackMissing(t *testing.T) {
	dir := t.TempDir()

	// Store a chunk.
	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}

	data := make([]byte, 4096)
	rand.Read(data)
	packNum, offset, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Copy the pack file aside, then remove it from the directory.
	packPath := cs.PackPath(packNum)
	packData, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatalf("reading pack: %v", err)
	}
	os.Remove(packPath)

	// Reopen the store starting at pack 1 so it doesn't overwrite the missing pack 0.
	cs2, err := store.NewChunkStoreAt(dir, 512*1024*1024, 3, 1)
	if err != nil {
		t.Fatalf("NewChunkStoreAt: %v", err)
	}
	defer cs2.Close()

	// Without OnPackMissing, Retrieve should fail.
	_, err = cs2.Retrieve(packNum, offset)
	if err == nil {
		t.Fatal("expected error for missing pack, got nil")
	}

	// Set OnPackMissing to copy the pack file back.
	cs2.OnPackMissing = func(pn uint32) error {
		return os.WriteFile(cs2.PackPath(pn), packData, 0644)
	}

	// Now Retrieve should succeed.
	retrieved, err := cs2.Retrieve(packNum, offset)
	if err != nil {
		t.Fatalf("Retrieve with OnPackMissing: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("data mismatch after OnPackMissing")
	}
}

func TestChunkStoreAt(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStoreAt(dir, 512*1024*1024, 3, 5)
	if err != nil {
		t.Fatalf("NewChunkStoreAt: %v", err)
	}

	data := make([]byte, 4096)
	rand.Read(data)

	packNum, _, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if packNum != 5 {
		t.Errorf("packNum = %d, want 5", packNum)
	}

	// Verify the file is named 0005.pack
	expected := cs.PackPath(5)
	info, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("Stat %s: %v", expected, err)
	}
	if info.Size() == 0 {
		t.Error("pack file is empty")
	}

	// Retrieve and verify round-trip
	retrieved, err := cs.Retrieve(5, 0)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("data mismatch")
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestRetrieveFromFrame(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	data := make([]byte, 4096)
	rand.Read(data)

	packNum, offset, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Get raw frame.
	frame, _, err := cs.RetrieveRaw(packNum, offset)
	if err != nil {
		t.Fatalf("RetrieveRaw: %v", err)
	}

	// RetrieveFromFrame should give back original data.
	retrieved, err := cs.RetrieveFromFrame(frame)
	if err != nil {
		t.Fatalf("RetrieveFromFrame: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("RetrieveFromFrame data mismatch")
	}
}

func TestRetrieveFromFrameEncrypted(t *testing.T) {
	dir := t.TempDir()

	mk, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	defer mk.Destroy()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3, mk)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	data := make([]byte, 8192)
	rand.Read(data)

	packNum, offset, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	frame, _, err := cs.RetrieveRaw(packNum, offset)
	if err != nil {
		t.Fatalf("RetrieveRaw: %v", err)
	}

	retrieved, err := cs.RetrieveFromFrame(frame)
	if err != nil {
		t.Fatalf("RetrieveFromFrame: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("RetrieveFromFrame encrypted data mismatch")
	}
}

func TestRetrieveFromFrameTooShort(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	_, err = cs.RetrieveFromFrame([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for short frame, got nil")
	}
}

func TestCacheFrame(t *testing.T) {
	dir := t.TempDir()

	cs, err := store.NewChunkStore(dir, 512*1024*1024, 3)
	if err != nil {
		t.Fatalf("NewChunkStore: %v", err)
	}
	defer cs.Close()

	data := make([]byte, 4096)
	rand.Read(data)

	// Store and get raw frame.
	packNum, offset, _, err := cs.Store(data)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	frame, _, err := cs.RetrieveRaw(packNum, offset)
	if err != nil {
		t.Fatalf("RetrieveRaw: %v", err)
	}

	// Store into a new ChunkStore that doesn't have the pack file.
	dir2 := t.TempDir()
	cs2, err := store.NewChunkStoreAt(dir2, 512*1024*1024, 3, 0)
	if err != nil {
		t.Fatalf("NewChunkStoreAt: %v", err)
	}
	defer cs2.Close()

	// Without cache, Retrieve should fail.
	_, err = cs2.Retrieve(packNum, offset)
	if err == nil {
		t.Fatal("expected error for missing pack, got nil")
	}

	// Cache the frame — now Retrieve should succeed.
	cs2.CacheFrame(packNum, offset, frame)
	retrieved, err := cs2.Retrieve(packNum, offset)
	if err != nil {
		t.Fatalf("Retrieve with cache: %v", err)
	}
	if !bytes.Equal(data, retrieved) {
		t.Error("cached frame data mismatch")
	}

	// Second call should still hit cache (same chunk may be referenced
	// by multiple manifest entries due to dedup).
	retrieved2, err := cs2.Retrieve(packNum, offset)
	if err != nil {
		t.Fatalf("second Retrieve with cache: %v", err)
	}
	if !bytes.Equal(data, retrieved2) {
		t.Error("second cached frame data mismatch")
	}
}
