// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/klauspost/compress/zstd"
)

// writeTestPack creates a minimal pack file with n synthetic chunks.
// Returns the frame offset of each chunk, in the order written — the ground
// truth a rebuilt index has to agree with.
func writeTestPack(t *testing.T, path string, chunks [][]byte) []int64 {
	t.Helper()

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	defer enc.Close()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating pack: %v", err)
	}
	defer f.Close()

	var offsets []int64
	var at int64
	var header [8]byte
	for _, raw := range chunks {
		compressed := enc.EncodeAll(raw, nil)
		binary.LittleEndian.PutUint32(header[0:4], uint32(len(compressed)))
		binary.LittleEndian.PutUint32(header[4:8], uint32(len(raw)))
		if _, err := f.Write(header[:]); err != nil {
			t.Fatalf("writing header: %v", err)
		}
		if _, err := f.Write(compressed); err != nil {
			t.Fatalf("writing payload: %v", err)
		}
		offsets = append(offsets, at)
		at += 8 + int64(len(compressed))
	}
	return offsets
}

func makeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := store.RepoConfig{
		ChunkMinSize:     65536,
		ChunkAvgSize:     131072,
		ChunkMaxSize:     262144,
		PackFileMaxSize:  1 << 30,
		CompressionLevel: 3,
	}
	if err := store.InitRepo(dir, cfg); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	return dir
}

func TestRebuild_BloomAndHash(t *testing.T) {
	repoPath := makeTestRepo(t)
	chunksDir := filepath.Join(repoPath, "chunks")
	if err := os.MkdirAll(chunksDir, 0755); err != nil {
		t.Fatalf("mkdir chunks: %v", err)
	}

	// Write two pack files with 3 chunks each.
	chunks0 := [][]byte{
		[]byte("chunk-alpha-0001"),
		[]byte("chunk-beta-0002"),
		[]byte("chunk-gamma-0003"),
	}
	chunks1 := [][]byte{
		[]byte("chunk-delta-0004"),
		[]byte("chunk-epsilon-0005"),
		[]byte("chunk-zeta-0006"),
	}
	offsets0 := writeTestPack(t, filepath.Join(chunksDir, "0000.pack"), chunks0)
	offsets1 := writeTestPack(t, filepath.Join(chunksDir, "0001.pack"), chunks1)

	// The ground truth: every chunk, and the pack and offset that really hold
	// it. This is what a rebuilt index has to reproduce from the packs alone.
	type placed struct {
		raw    []byte
		pack   uint32
		offset int64
	}
	var placedChunks []placed
	for i, raw := range chunks0 {
		placedChunks = append(placedChunks, placed{raw, 0, offsets0[i]})
	}
	for i, raw := range chunks1 {
		placedChunks = append(placedChunks, placed{raw, 1, offsets1[i]})
	}

	ctx := context.Background()
	result, err := index.Rebuild(ctx, index.RebuildOptions{
		RepoPath:         repoPath,
		RebuildBloom:     true,
		RebuildHashIndex: true,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if result.PacksScanned != 2 {
		t.Errorf("PacksScanned = %d, want 2", result.PacksScanned)
	}
	if result.ChunksScanned != 6 {
		t.Errorf("ChunksScanned = %d, want 6", result.ChunksScanned)
	}

	// Bloom and hash-index.db should exist.
	indexDir := filepath.Join(repoPath, "index")
	for _, name := range []string{"bloom.bin", "hash-index.db"} {
		if _, err := os.Stat(filepath.Join(indexDir, name)); err != nil {
			t.Errorf("expected %s to exist after rebuild: %v", name, err)
		}
	}

	// Reload the index and check every chunk resolves — and that it resolves
	// to the location that ACTUALLY holds it.
	//
	// "Found" on its own is not enough. A Rebuild that writes both files but
	// records the wrong pack number or offset answers every lookup and names
	// packs that exist, and still cannot restore a byte: that is #376, which
	// shipped completed, unrestorable backups while 43 packages stayed green.
	// The only oracle that separates the two is reading the frame back through
	// the store at the location the index reports — the same read restore
	// performs — and comparing the bytes. internal/captureflow's index
	// invariant does this for the cloud path; this is the same check for
	// `disknexus index --rebuild-all`, which is the documented recovery path
	// when a repository's index is lost (docs/index_file_recovery.md).
	idx, err := index.NewDedupIndex(indexDir, 100, 0.001, 0)
	if err != nil {
		t.Fatalf("NewDedupIndex: %v", err)
	}
	defer idx.Close()

	// Chunk identity is the hash of NORMALIZED bytes. makeTestRepo declares no
	// normalizer, so identity is the hash of the chunk itself — assert that
	// rather than assume it, because a repo that declared one would make every
	// lookup below miss for a reason that has nothing to do with Rebuild.
	cfg, err := store.LoadRepoConfig(repoPath)
	if err != nil {
		t.Fatalf("LoadRepoConfig: %v", err)
	}
	if n := cfg.Effective().Normalizers; len(n) != 0 {
		t.Fatalf("this repo declares normalizers %v, and the identities below are hashed with none — "+
			"every lookup would miss for a reason unrelated to the rebuild", n)
	}

	// A store over the repo's own pack files. ChunkStore maps a pack NUMBER to
	// the file that number names, so RetrieveRaw resolves the index's answer
	// through exactly the mapping restore uses. It opens a pack for writing
	// only on the first Store call, which never happens here.
	cs, err := store.NewChunkStore(repoPath, 1<<30, 3)
	if err != nil {
		t.Fatalf("opening a chunk store over the rebuilt repo: %v", err)
	}
	defer cs.Close()

	checked := 0
	for _, want := range placedChunks {
		id := hasher.Sum(preprocess.IdentityHashInput(nil, want.raw))
		res, err := idx.Check(id)
		if err != nil {
			t.Fatalf("index lookup for chunk %q: %v", want.raw, err)
		}
		if res.IsNew || res.Entry == nil {
			t.Errorf("the rebuilt index cannot resolve chunk %q (%x), which really is in pack %d at offset %d. "+
				"Restore resolves every manifest entry through the index alone and hard-fails on a miss, so a "+
				"backup referencing this chunk is unrestorable after `index --rebuild-all`.",
				want.raw, id.StrongHash[:8], want.pack, want.offset)
			continue
		}
		e := res.Entry
		// Two independent oracles, both reported: the bytes really stored at
		// the location the index names, and the location itself. Neither
		// short-circuits the other — a wrong pack number shows up as both
		// "that location holds a different chunk" (what the operator's restore
		// hits) and "it is really in pack N" (which one it should have been).
		ok := true

		// Oracle 1 — the authority. Read the frame back out of the repository
		// at the location the index reported and confirm it is this chunk.
		switch frame, rawSize, err := cs.RetrieveRaw(e.PackNumber, int64(e.StoreOffset)); {
		case err != nil:
			ok = false
			t.Errorf("the rebuilt index puts chunk %q at pack %d offset %d, and reading that frame back out of "+
				"the repository fails: %v — restore reads exactly this location, so this chunk is unrecoverable.",
				want.raw, e.PackNumber, e.StoreOffset, err)
		case rawSize != e.ChunkLength:
			ok = false
			t.Errorf("the frame at pack %d offset %d declares %d raw bytes; the rebuilt index recorded %d for "+
				"chunk %q — restore sizes its read from the index and would truncate or overrun.",
				e.PackNumber, e.StoreOffset, rawSize, e.ChunkLength, want.raw)
		default:
			got, err := cs.RetrieveFromFrame(frame)
			if err != nil {
				ok = false
				t.Errorf("decoding the frame the rebuilt index names for chunk %q (pack %d offset %d) fails: %v",
					want.raw, e.PackNumber, e.StoreOffset, err)
			} else if !bytes.Equal(got, want.raw) {
				ok = false
				t.Errorf("the rebuilt index sends chunk %q to pack %d offset %d, and that location holds a "+
					"DIFFERENT chunk, %q. The lookup answers and the pack exists, so nothing upstream notices; "+
					"the restore either fails an integrity check or writes the wrong bytes to the volume (#376).",
					want.raw, e.PackNumber, e.StoreOffset, got)
			}
		}

		// Oracle 2 — exactness. Oracle 1 is blind to a chunk whose bytes also
		// happen to sit somewhere else; this test wrote the packs, so it knows
		// the one location that is correct.
		if e.PackNumber != want.pack || int64(e.StoreOffset) != want.offset || e.ChunkLength != uint32(len(want.raw)) {
			ok = false
			t.Errorf("the rebuilt index sends chunk %q to pack %d offset %d length %d; it is really in pack %d "+
				"at offset %d length %d. Restore reads exactly the location the index names.",
				want.raw, e.PackNumber, e.StoreOffset, e.ChunkLength, want.pack, want.offset, len(want.raw))
		}
		if ok {
			checked++
		}
	}
	// A loop that iterates zero times must not pass (docs/TESTING.md §8). This
	// repo holds exactly six chunks, stated as a literal so a fixture that
	// quietly stopped producing them fails here rather than reporting success
	// over an empty list.
	const wantChecked = 6
	if checked != wantChecked || len(placedChunks) != wantChecked {
		t.Fatalf("read back and byte-compared %d chunks out of the %d this fixture placed, want %d of %d — "+
			"any shortfall is reported above; a rebuild that cannot place every chunk has not recovered "+
			"the index, and a fixture that placed none proves nothing",
			checked, len(placedChunks), wantChecked, wantChecked)
	}

	// Discrimination: a chunk that was never written to any pack must NOT
	// resolve. Without this, an index that answered "found, pack 0, offset 0"
	// to everything would satisfy the loop above for the one chunk that really
	// does live there and tell us nothing about the other five.
	unseen := hasher.Sum([]byte("chunk-never-written-to-any-pack"))
	res, err := idx.Check(unseen)
	if err != nil {
		t.Fatalf("index lookup for a chunk that was never stored: %v", err)
	}
	if !res.IsNew {
		t.Errorf("the rebuilt index resolves a chunk that was never written to any pack (it answers pack %d "+
			"offset %d) — it answers lookups it cannot answer, so the placements verified above prove nothing",
			res.Entry.PackNumber, res.Entry.StoreOffset)
	}
}

func TestRebuild_BloomOnly(t *testing.T) {
	repoPath := makeTestRepo(t)
	chunksDir := filepath.Join(repoPath, "chunks")
	os.MkdirAll(chunksDir, 0755)

	writeTestPack(t, filepath.Join(chunksDir, "0000.pack"), [][]byte{
		[]byte("hello-world-chunk"),
	})

	result, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:     repoPath,
		RebuildBloom: true,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if result.ChunksScanned != 1 {
		t.Errorf("ChunksScanned = %d, want 1", result.ChunksScanned)
	}

	indexDir := filepath.Join(repoPath, "index")
	if _, err := os.Stat(filepath.Join(indexDir, "bloom.bin")); err != nil {
		t.Errorf("bloom.bin should exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(indexDir, "hash-index.db")); err == nil {
		t.Error("hash-index.db should NOT exist when only rebuilding bloom")
	}
}

func TestRebuild_HashOnly(t *testing.T) {
	repoPath := makeTestRepo(t)
	chunksDir := filepath.Join(repoPath, "chunks")
	os.MkdirAll(chunksDir, 0755)

	writeTestPack(t, filepath.Join(chunksDir, "0000.pack"), [][]byte{
		[]byte("hash-only-chunk"),
	})

	result, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:         repoPath,
		RebuildHashIndex: true,
	})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if result.ChunksScanned != 1 {
		t.Errorf("ChunksScanned = %d, want 1", result.ChunksScanned)
	}

	indexDir := filepath.Join(repoPath, "index")
	if _, err := os.Stat(filepath.Join(indexDir, "hash-index.db")); err != nil {
		t.Errorf("hash-index.db should exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(indexDir, "bloom.bin")); err == nil {
		t.Error("bloom.bin should NOT exist when only rebuilding hash index")
	}
}

func TestRebuild_NoPacks(t *testing.T) {
	repoPath := makeTestRepo(t)
	// chunks dir exists but is empty.
	os.MkdirAll(filepath.Join(repoPath, "chunks"), 0755)

	_, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:     repoPath,
		RebuildBloom: true,
	})
	if err == nil {
		t.Error("expected error for empty chunks dir")
	}
}

func TestRebuild_NoRepo(t *testing.T) {
	_, err := index.Rebuild(context.Background(), index.RebuildOptions{
		RepoPath:     t.TempDir() + "/nonexistent",
		RebuildBloom: true,
	})
	if err == nil {
		t.Error("expected error for non-existent repo")
	}
}
