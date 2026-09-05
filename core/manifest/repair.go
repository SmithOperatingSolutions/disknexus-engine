// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"fmt"
	"os"
	"sort"
)

// RepairEntries loads each specified backup, sorts its entries by VolumeOffset,
// and re-saves the .dnm file. This repairs backups created before the pipeline
// sequencer was added (where parallel hash workers could deliver entries in
// non-deterministic order, breaking binary-search-based restore-files).
//
// If backupIDs is empty, all backups in the repo are repaired.
// If dryRun is true, no files are written; only the diagnosis is reported.
//
// Returns counts of (repaired, skipped, failed) backups.
func RepairEntries(repoPath string, backupIDs []string, dryRun bool) (repaired, skipped, failed int, err error) {
	if len(backupIDs) == 0 {
		backups, listErr := List(repoPath)
		if listErr != nil {
			return 0, 0, 0, fmt.Errorf("listing backups: %w", listErr)
		}
		for _, b := range backups {
			backupIDs = append(backupIDs, b.BackupID)
		}
	}

	for _, id := range backupIDs {
		b, loadErr := Load(repoPath, id)
		if loadErr != nil {
			fmt.Printf("  FAIL  %s: load error: %v\n", id, loadErr)
			failed++
			continue
		}

		if isSorted(b.Entries) {
			// Even if entries are sorted, remove any orphaned .entries sidecar
			// that predates the Save() cleanup logic in Phase 6 — but ONLY when
			// a .dnm exists, which is what makes the sidecar redundant. For an
			// unmigrated legacy backup (JSON manifest, no .dnm) the sidecar is
			// the only copy of the entries; deleting it would silently zero the
			// backup and let a later prune drop all of its chunks as orphans.
			entriesFile := EntriesPath(repoPath, id)
			_, dnmErr := os.Stat(DNMPath(repoPath, id))
			dnmExists := dnmErr == nil
			if _, statErr := os.Stat(entriesFile); statErr == nil && dnmExists {
				if !dryRun {
					os.Remove(entriesFile)
				}
				fmt.Printf("  OK    %s (%d entries sorted, removed orphaned .entries)\n", id, len(b.Entries))
			} else {
				fmt.Printf("  OK    %s (%d entries already sorted)\n", id, len(b.Entries))
			}
			skipped++
			continue
		}

		fmt.Printf("  REPAIR %s (%d entries out of order)\n", id, len(b.Entries))
		if dryRun {
			repaired++
			continue
		}

		sort.Slice(b.Entries, func(i, j int) bool {
			return b.Entries[i].VolumeOffset < b.Entries[j].VolumeOffset
		})

		if saveErr := b.Save(repoPath); saveErr != nil {
			fmt.Printf("  FAIL  %s: save error: %v\n", id, saveErr)
			failed++
			continue
		}

		repaired++
	}

	return repaired, skipped, failed, nil
}

// isSorted reports whether entries are in non-decreasing VolumeOffset order.
func isSorted(entries []Entry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i].VolumeOffset < entries[i-1].VolumeOffset {
			return false
		}
	}
	return true
}
