// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import "fmt"

// LoadMetadata reads a backup's header — identity, size, parent, digest —
// and NOT its entries (#506). A block restore walks a parent chain to learn
// each manifest's parent, and a whole-disk restore validates each member's
// size before touching it; both used LoadForBlockRestore, which reads every
// entry into memory to answer a question about the header. Entries are
// read through NewEntryAccessor when a caller actually needs them.
//
// Legacy JSON manifests carry their entries inline; those are loaded and
// dropped, which is the only choice that format offers.
func LoadMetadata(repoPath, backupID string) (*Backup, error) {
	dnmPath := DNMPath(repoPath, backupID)
	r, err := OpenDNMReader(dnmPath)
	if err == nil {
		defer r.Close()
		b, err := r.Metadata()
		if err != nil {
			return nil, fmt.Errorf("reading dnm metadata: %w", err)
		}
		return &b, nil
	}
	b, err := Load(repoPath, backupID)
	if err != nil {
		return nil, err
	}
	b.Entries = nil
	return b, nil
}
