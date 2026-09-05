// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A pack's frame header states the payload length, and a CORRUPTED pack
// states garbage — up to 4GB from four flipped bytes. Retrieve/RetrieveRaw
// allocated that length before any validation, so restoring over a corrupt
// pack ballooned to multi-GB allocations: the #348 CI runners died of OOM
// running the XOR-corruption pins, and a real machine restoring a damaged
// repo would do the same. A frame length must be bounds-checked BEFORE the
// allocation, and the refusal must name the corrupt figure.
func TestCorruptFrameLengthRefusedBeforeAllocation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A pack containing one "frame" whose header claims ~4GB of payload.
	header := make([]byte, 16)
	binary.LittleEndian.PutUint32(header[0:4], 0xFFFFFFF0) // payload length: garbage
	binary.LittleEndian.PutUint32(header[4:8], 1024)       // raw size: irrelevant
	packPath := filepath.Join(dir, "chunks", "0000.pack")
	if err := os.WriteFile(packPath, header, 0o644); err != nil {
		t.Fatal(err)
	}

	cs, err := NewChunkStore(dir, 128<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	for name, call := range map[string]func() error{
		"Retrieve":    func() error { _, err := cs.Retrieve(0, 0); return err },
		"RetrieveRaw": func() error { _, _, err := cs.RetrieveRaw(0, 0); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s of a frame claiming 4GB succeeded, want a bounds refusal", name)
		}
		if !strings.Contains(err.Error(), "4294967280") || !strings.Contains(err.Error(), "exceeds") {
			t.Errorf("%s error = %q — must refuse BEFORE allocating, naming the corrupt length and the bound (corrupt pack, not an I/O error)", name, err)
		}
	}
}
