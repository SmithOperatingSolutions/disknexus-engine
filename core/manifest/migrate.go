// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Migrate converts all legacy .manifest backups in repoPath that do not yet
// have a .dnm file. Returns counts of converted and skipped backups.
// If dryRun is true, no files are written or renamed.
//
// The function is idempotent — safe to run multiple times. Individual backup
// errors are non-fatal: a warning is printed and failed is incremented.
func Migrate(repoPath string, dryRun bool) (converted, skipped, failed int, err error) {
	dir := filepath.Join(repoPath, "manifests")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, fmt.Errorf("reading manifests dir: %w", err)
	}

	for _, e := range dirEntries {
		name := e.Name()
		// Only process .manifest files (skip .manifest.bak and other files).
		if filepath.Ext(name) != ".manifest" || strings.HasSuffix(name, ".manifest.bak") {
			continue
		}
		id := strings.TrimSuffix(name, ".manifest")

		// Skip if .dnm already exists.
		if _, statErr := os.Stat(DNMPath(repoPath, id)); statErr == nil {
			skipped++
			continue
		}

		// Load via the legacy path (.manifest JSON + .entries sidecar).
		b, loadErr := Load(repoPath, id)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load backup %s: %v\n", id, loadErr)
			failed++
			continue
		}

		if dryRun {
			fmt.Printf("would convert: %s\n", id)
			converted++
			continue
		}

		// Write the .dnm file.
		if writeErr := saveDNM(repoPath, b); writeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to write .dnm for %s: %v\n", id, writeErr)
			failed++
			continue
		}

		// Verify: open the .dnm and confirm BackupID matches.
		r, openErr := OpenDNMReader(DNMPath(repoPath, id))
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to verify .dnm for %s: %v\n", id, openErr)
			os.Remove(DNMPath(repoPath, id))
			failed++
			continue
		}
		meta, readErr := r.readMetadata()
		r.Close()
		if readErr != nil || meta.BackupID != id {
			fmt.Fprintf(os.Stderr, "warning: .dnm verification failed for %s\n", id)
			os.Remove(DNMPath(repoPath, id))
			failed++
			continue
		}

		// Rename .manifest → .manifest.bak and .entries → .entries.bak.
		manifestPath := filepath.Join(dir, id+".manifest")
		os.Rename(manifestPath, manifestPath+".bak")
		entriesPath := EntriesPath(repoPath, id)
		os.Rename(entriesPath, entriesPath+".bak") // ignore error if sidecar absent

		converted++
	}

	return converted, skipped, failed, nil
}
