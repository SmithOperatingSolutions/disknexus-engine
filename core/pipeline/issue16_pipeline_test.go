// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestAnalyzeLeavesRepoUntouched guards issue #16: Analyze's deferred index
// Close flushed a fresh bloom filter even with zero inserts, leaving an
// undersized, unresizable bloom.bin on a fresh repo that cripples a later real
// backup. Analyze must not persist any index state.
func TestAnalyzeLeavesRepoUntouched(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	data := make([]byte, 256*1024)
	rand.Read(data)
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())
	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := p.Analyze(context.Background(), reader, sourcePath, reader.Size(), repoPath); err != nil {
		reader.Close()
		t.Fatalf("Analyze: %v", err)
	}
	reader.Close()

	if _, err := os.Stat(filepath.Join(repoPath, "index", "bloom.bin")); err == nil {
		t.Fatal("Analyze persisted bloom.bin; an analyze run must leave the repo untouched (the fresh bloom is undersized and cripples the next real backup)")
	}
	if info, err := os.Stat(filepath.Join(repoPath, "index", "hash-index.db")); err == nil && info.Size() > 0 {
		t.Fatalf("Analyze persisted %d bytes of hash-index.db; must be empty", info.Size())
	}
}

// errAfterReader yields n bytes then a non-EOF error, simulating a source that
// dies mid-backup.
type errAfterReader struct {
	remaining int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, errors.New("simulated device failure")
	}
	n := len(p)
	if n > r.remaining {
		n = r.remaining
	}
	for i := range n {
		p[i] = byte(i)
	}
	r.remaining -= n
	return n, nil
}

// TestFailedBackupDiscardsSessionState guards two issue #16 items: a FAILED
// backup used to (a) durably flush its session index inserts via the deferred
// Close — referencing chunks in a pack that was never sealed/uploaded — and
// (b) leak the half-written .entries sidecar. Both must be discarded.
func TestFailedBackupDiscardsSessionState(t *testing.T) {
	repoPath, _, cfg := setupRepo(t)

	p := pipeline.New(cfg, newLogger(), noEnc())
	reader := &errAfterReader{remaining: 512 * 1024} // enough for several chunks
	_, err := p.Backup(context.Background(), reader, "failing-source", 1024*1024, repoPath)
	if err == nil {
		t.Fatal("expected the backup to fail on the erroring reader")
	}
	if !strings.Contains(err.Error(), "simulated device failure") {
		t.Fatalf("unexpected failure: %v", err)
	}

	// (a) The index must hold no session inserts.
	idx, err := index.NewDedupIndex(filepath.Join(repoPath, "index"), 1000, cfg.BloomFPRate, 0)
	if err != nil {
		t.Fatalf("reopening index: %v", err)
	}
	defer idx.Close()
	if n := idx.Stats().IndexEntries; n != 0 {
		t.Fatalf("failed backup persisted %d index entries referencing never-sealed packs", n)
	}

	// (b) No .entries sidecar may be left behind.
	matches, _ := filepath.Glob(filepath.Join(repoPath, "manifests", "*.entries"))
	if len(matches) != 0 {
		t.Fatalf("failed backup leaked entries sidecar(s): %v", matches)
	}
}
