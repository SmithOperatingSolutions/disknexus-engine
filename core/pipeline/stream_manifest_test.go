// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
)

// TestStreamManifestBackup (lever 4 pipeline seam): a backup with a manifest
// streamer must leave NO local .entries/.dnm, hand every entry to the
// streamer, and the reassembled parts must be a readable .dnm whose entry
// count matches the run.
func TestStreamManifestBackup(t *testing.T) {
	cfg := config.Default()
	cfg.PackFileMaxSize = 64 * 1024
	repo := initResumeRepo(t, cfg)

	rng := rand.New(rand.NewSource(0x51DE))
	data := make([]byte, 300*1024)
	rng.Read(data)

	var parts [][]byte
	st := manifest.NewDNMStreamer(8*1024, func(p []byte) error {
		parts = append(parts, append([]byte(nil), p...))
		return nil
	})
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.BackupID = "streamed-run"
	p.StreamManifest = st
	p.FinishManifest = func(b *manifest.Backup) error { return st.Finish(b) }

	res, err := p.Backup(context.Background(), bytes.NewReader(data), "vol", int64(len(data)), repo)
	if err != nil {
		t.Fatalf("streamed backup: %v", err)
	}
	if _, err := os.Stat(manifest.EntriesPath(repo, res.BackupID)); !os.IsNotExist(err) {
		t.Fatal("local .entries sidecar exists despite streaming")
	}
	if _, err := os.Stat(manifest.DNMPath(repo, res.BackupID)); !os.IsNotExist(err) {
		t.Fatal("local .dnm exists despite streaming")
	}

	blob := []byte{}
	for _, pt := range parts {
		blob = append(blob, pt...)
	}
	path := filepath.Join(t.TempDir(), "composed.dnm")
	if err := os.WriteFile(path, blob, 0644); err != nil {
		t.Fatal(err)
	}
	r, err := manifest.OpenDNMReader(path)
	if err != nil {
		t.Fatalf("composed manifest unreadable: %v", err)
	}
	defer r.Close()
	if r.EntriesCount() != res.TotalChunks {
		t.Fatalf("entries %d != chunks %d", r.EntriesCount(), res.TotalChunks)
	}
}

// TestStreamManifestRefusesResumable: the two features are mutually exclusive
// (resume needs the on-disk sidecar for its segment cross-checks).
func TestStreamManifestRefusesResumable(t *testing.T) {
	cfg := config.Default()
	repo := initResumeRepo(t, cfg)
	st := manifest.NewDNMStreamer(8*1024, func([]byte) error { return nil })
	p := pipeline.New(cfg, resumeLogger(), noEnc())
	p.Resumable = true
	p.StreamManifest = st
	p.FinishManifest = func(b *manifest.Backup) error { return st.Finish(b) }
	if _, err := p.Backup(context.Background(), bytes.NewReader([]byte("x")), "vol", 1, repo); err == nil {
		t.Fatal("resumable + streamed accepted")
	}
}
