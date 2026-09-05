// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// TestBackupIDOverrideHonored guards the #15 leftover: cloud backups are
// tracked by a controller-issued ID, but the pipeline generated its own
// manifest UUID, so the uploaded manifest was named by an ID the controller
// never lists — `restore --backup <listed-id>` could not find it. When
// Pipeline.BackupID is set, the backup (and its manifest file) must carry
// exactly that ID.
func TestBackupIDOverrideHonored(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	data := make([]byte, 64*1024)
	rand.Read(data)
	if err := os.WriteFile(sourcePath, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const want = "aaaabbbb-cccc-dddd-eeee-ffff00001111" // controller-issued ID
	p := pipeline.New(cfg, newLogger(), noEnc())
	p.BackupID = want

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer reader.Close()

	result, err := p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if result.BackupID != want {
		t.Fatalf("result.BackupID = %q, want the controller-issued %q", result.BackupID, want)
	}
	if _, err := manifest.Load(repoPath, want); err != nil {
		t.Fatalf("manifest not saved under the controller-issued ID: %v", err)
	}
}
