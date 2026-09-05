// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"fmt"
	"strings"
)

// Low-footprint profile: bounds the peak local disk a (cloud) backup run
// needs. The transient per-run artifacts scale with chunk count — 48 B/chunk
// dedup index + 45 B/chunk manifest sidecar — so 64 KB average chunks cut
// them 8× vs the 8 KB local default (~1.7 GB instead of ~13 GB per TB
// captured), and 128 MB packs cap the one in-flight staged pack. The cost is
// coarser dedup granularity: a small in-place change re-stores up to 512 KB.
// Not the default; chosen per repo at creation ("an option, not the mode").
const (
	LowFootprintChunkMin = 16 << 10
	LowFootprintChunkAvg = 64 << 10
	LowFootprintChunkMax = 512 << 10
	LowFootprintPackMax  = 128 << 20
)

// ProfileNames lists every profile ApplyProfile accepts, in the order they
// should be offered (finest dedup first). It is the single source of truth
// for the catalog: the CLI's error text and the controller's
// GET /api/repo-profiles both derive from it, so a profile is never
// implemented in one surface and invisible in another.
// TestProfileNamesMatchesApplyProfileCases enforces the correspondence.
// The catalog is a doubling ladder — 8 KB, 32 KB, 64 KB, 128 KB, 1 MB — so
// the trade-off is one sentence: each step up halves per-chunk index and
// manifest pressure and doubles what a small in-place write re-stores.
func ProfileNames() []string {
	return []string{"fine-grained", "mixed", "low-footprint", "coarse", "archive"}
}

// ApplyProfile applies a named creation profile to cfg. An empty name is a
// no-op. Profiles only touch the chunker/pack geometry — encryption,
// compression and normalizers stay whatever the caller chose.
func ApplyProfile(cfg *RepoConfig, name string) error {
	switch name {
	case "":
		return nil
	case "low-footprint":
		// Alias of the default geometry since the 64 KB default (#83);
		// kept because docs, scripts, and existing controller repo
		// profiles reference it.
		cfg.ChunkMinSize = LowFootprintChunkMin
		cfg.ChunkAvgSize = LowFootprintChunkAvg
		cfg.ChunkMaxSize = LowFootprintChunkMax
		cfg.BuzhashMask = uint64(LowFootprintChunkAvg - 1)
		cfg.PackFileMaxSize = LowFootprintPackMax
		return nil
	case "fine-grained":
		// The pre-v0.7.5 default: maximum dedup granularity for
		// small-file file-mode repos; poor fit for volume-scale streams.
		cfg.ChunkMinSize = 4 << 10
		cfg.ChunkAvgSize = 8 << 10
		cfg.ChunkMaxSize = 64 << 10
		cfg.BuzhashMask = uint64(8<<10) - 1
		cfg.PackFileMaxSize = 512 << 20
		return nil
	case "mixed":
		// Databases and mixed fileservers rewrite records IN PLACE, so what
		// costs them is write amplification, not dedup granularity: a small
		// update dirties whatever chunk contains it, and the default's 64 KB
		// chunk re-stores twice the bytes a 32 KB one does. Going the other
		// way to 8 KB doubles index pressure to buy granularity these
		// workloads never exploit (records rarely repeat across the fleet).
		// Packs stay at the default 128 MB — the halving is about chunk
		// geometry, and nothing here changes how much is in flight per run.
		cfg.ChunkMinSize = 8 << 10
		cfg.ChunkAvgSize = 32 << 10
		cfg.ChunkMaxSize = 256 << 10
		cfg.BuzhashMask = uint64(32<<10) - 1
		cfg.PackFileMaxSize = 128 << 20
		return nil
	case "coarse":
		// Multi-TB image fleets: half the metadata and index pressure of
		// the default, at the cost of larger incrementals (a changed block
		// dirties a ~128 KB chunk) and coarser cross-machine dedup.
		cfg.ChunkMinSize = 32 << 10
		cfg.ChunkAvgSize = 128 << 10
		cfg.ChunkMaxSize = 1 << 20
		cfg.BuzhashMask = uint64(128<<10) - 1
		cfg.PackFileMaxSize = 256 << 20
		return nil
	case "archive":
		// Already-compressed bulk data — video libraries, media archives,
		// encrypted blobs, archive tiers. It does not dedup: identical chunks
		// essentially never recur, so every byte spent on fine chunking is
		// index and manifest overhead bought for nothing (~128× fewer chunks
		// than fine-grained here). Incrementals are whole new objects
		// regardless, which is what makes the coarse geometry free rather
		// than a trade. 1 GiB packs keep a multi-TB tier from becoming
		// millions of small objects to list, fetch and account for.
		cfg.ChunkMinSize = 256 << 10
		cfg.ChunkAvgSize = 1 << 20
		cfg.ChunkMaxSize = 8 << 20
		cfg.BuzhashMask = uint64(1<<20) - 1
		cfg.PackFileMaxSize = 1 << 30
		return nil
	default:
		return fmt.Errorf("unknown profile %q (valid: %s)", name, strings.Join(ProfileNames(), ", "))
	}
}
