// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package e2e

// Ported from core/pipeline/resume_test.go: the CLI-equivalent resume
// orchestration (checkpoint writer, pack reconciliation) and the reader
// that makes the interrupt land deterministically mid-stream.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// gatedReader delivers data[:gateAt] normally, then blocks until ctx is
// cancelled and returns ctx.Err(). This keeps the chunker running until the
// consumer-side checkpoint deterministically cancels the context, so the
// interrupt lands mid-stream instead of racing against EOF on a small input.
type gatedReader struct {
	data   []byte
	pos    int
	gateAt int
	ctx    context.Context
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.pos >= r.gateAt {
		<-r.ctx.Done()
		return 0, r.ctx.Err()
	}
	end := r.pos + len(p)
	if end > r.gateAt {
		end = r.gateAt
	}
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}

func mkCheckpointFn(repo, backupID string, total int64, onWrite func(*checkpoint.Checkpoint)) func(checkpoint.Progress, checkpoint.Delta) error {
	return func(prog checkpoint.Progress, delta checkpoint.Delta) error {
		// Mirror the CLI: segment first, then the checkpoint record.
		if err := checkpoint.AppendSegmentLocal(repo, backupID, &checkpoint.Segment{
			Seq:             prog.CheckpointSeq,
			EntriesLenAfter: prog.EntriesLen,
			SidecarBytes:    delta.SidecarBytes,
			Inserts:         delta.Inserts,
		}); err != nil {
			return err
		}
		c := &checkpoint.Checkpoint{
			Version:         checkpoint.Version,
			Mode:            "volume",
			BackupID:        backupID,
			Progress:        prog,
			SourceKind:      "input",
			SourcePath:      "mem",
			TotalSize:       total,
			PacksGeneration: store.PacksGeneration(repo),
		}
		if err := checkpoint.Write(repo, c); err != nil {
			return err
		}
		if onWrite != nil {
			onWrite(c)
		}
		return nil
	}
}

// deletePacksAbove removes chunks/NNNN.pack with NNNN > n (the resumed run's
// un-checkpointed tail), matching the CLI's resume reconciliation.
func deletePacksAbove(t *testing.T, repo string, n uint32) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(repo, "chunks", "*.pack"))
	for _, m := range matches {
		base := strings.TrimSuffix(filepath.Base(m), ".pack")
		num, err := strconv.Atoi(base)
		if err != nil {
			continue
		}
		if uint32(num) > n {
			if err := os.Remove(m); err != nil {
				t.Fatal(err)
			}
		}
	}
}
