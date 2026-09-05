// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestBackupReportsFinalPackSealFailure proves that run() swallows the
// error from the deferred chunkStore.Close(). For any backup smaller than
// PackFileMaxSize the final pack is sealed only inside Close(), and for
// S3-backed repos OnPackSealed is what uploads the pack — so a failed
// upload of ALL chunk data still returns a nil error from Backup, the
// manifest is saved, and the backup is reported as successful while being
// unrestorable. ChunkStore.Close() itself returns the error correctly
// (see store's TestOnPackSealedError); the pipeline discards it.
func TestBackupReportsFinalPackSealFailure(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	sourceData := make([]byte, 128*1024) // far below PackFileMaxSize: no rotation
	rand.Read(sourceData)
	if err := os.WriteFile(sourcePath, sourceData, 0644); err != nil {
		t.Fatal(err)
	}

	p := pipeline.New(cfg, newLogger(), noEnc())

	sealErr := errors.New("simulated pack upload failure")
	p.OnPackSealed = func(packPath string, packNum uint32, size int64) error {
		return sealErr
	}

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer reader.Close()

	_, err = p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	if !errors.Is(err, sealErr) {
		t.Fatalf("Backup returned %v; want the final pack seal failure %v — a backup whose chunk data was never persisted must not report success", err, sealErr)
	}
}
