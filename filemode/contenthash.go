// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package filemode

// The derivation lives in engine/core/manifest since #465 (restore
// recomputes it at extract; core cannot import this package). These
// delegates keep every existing caller and the #353 call sites unchanged.

import (
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

func ComputeContentHashes(backup *manifest.Backup) { manifest.ComputeContentHashes(backup) }

func ComputeContentHashesForVolumeFile(f *manifest.FileEntry, entries manifest.EntryAccessor) {
	manifest.ComputeContentHashesForVolumeFile(f, entries)
}

func ComputeContentHashesForFile(f *manifest.FileEntry, entries manifest.EntryAccessor) {
	manifest.ComputeContentHashesForFile(f, entries)
}
