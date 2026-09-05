//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"os"
	"path/filepath"
	"syscall"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	govss "github.com/SmithOperatingSolutions/go-vss"
)

// AddRepoExclusionRanges enumerates all files under repoPath and adds their
// physical byte extents to m, so the backup stream skips those regions. Only
// applicable when the repo is on the same volume as the backup source.
//
// Extents are mapped through go-vss's live-volume FileExtents
// (FSCTL_GET_RETRIEVAL_POINTERS). Best-effort: a file that can't be opened or
// mapped is simply not excluded.
func AddRepoExclusionRanges(repoPath string, m *volume.ExclusionMap) {
	vol, err := govss.OpenVolume(repoPath)
	if err != nil {
		return
	}
	// FileExtents takes a path relative to the volume root ("C:\").
	root := filepath.VolumeName(repoPath) + `\`
	filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		exts, ferr := vol.FileExtents(filepath.ToSlash(rel))
		if ferr != nil {
			return nil
		}
		for _, e := range exts {
			if e.Length > 0 {
				m.AddRange(e.VolumeOffset, e.Length)
			}
		}
		return nil
	})
}

// Windows file attribute flags for cloud-sync placeholder (stub) files.
// Files with these attributes have not been downloaded to local disk;
// opening them triggers a download. We skip extent lookups for such files.
const (
	fileAttrOffline            = uint32(0x00001000) // data not immediately available (offline/HSM)
	fileAttrRecallOnOpen       = uint32(0x00040000) // recall triggered even for attribute-only opens
	fileAttrRecallOnDataAccess = uint32(0x00400000) // recall triggered on data access (typical cloud stub)
)

// isCloudStub returns true when info represents a cloud-sync placeholder file
// whose data has not been downloaded to the local volume. Opening such files
// (even for attributes only) can trigger a download from services like
// Synology Drive, OneDrive, or Dropbox.
func isCloudStub(info os.FileInfo) bool {
	raw, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	a := raw.FileAttributes
	return a&fileAttrOffline != 0 || a&fileAttrRecallOnOpen != 0 || a&fileAttrRecallOnDataAccess != 0
}
