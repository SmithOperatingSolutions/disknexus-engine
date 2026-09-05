// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
)

// SampleEntryIndices deterministically selects a subset of a backup's
// verifiable (non-excluded) entry indices for --sample verification.
//
//   - percent in (0,100]; percent >= 100 selects the whole verifiable population.
//   - K = ceil(percent/100 * P), clamped to at least 1 (when there is anything
//     to sample) and at most P.
//   - Selection is seeded by the BackupID (optionally mixed with seed): the same
//     (backup, percent, seed) yields the same subset on every host and rerun, so
//     a flagged sample failure is reproducible.
//
// The returned indices are TRUE manifest indices, sorted ascending (stable
// report order + better range coalescing).
func SampleEntryIndices(backup *manifest.Backup, percent float64, seed uint64) ([]int, error) {
	// Reject non-finite first: NaN fails every comparison, so a naive
	// `percent <= 0 || percent > 100` would let NaN through and (via
	// int(Ceil(NaN))) silently sample ~1 chunk.
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent <= 0 || percent > 100 {
		return nil, fmt.Errorf("sample percent must be a finite value in (0,100], got %g", percent)
	}

	var pop []int
	for i := range backup.Entries {
		if !backup.Entries[i].IsExcluded {
			pop = append(pop, i)
		}
	}
	if len(pop) == 0 {
		return nil, nil
	}

	k := int(math.Ceil(percent / 100 * float64(len(pop))))
	if k < 1 {
		k = 1
	}
	if k > len(pop) {
		k = len(pop)
	}
	if k == len(pop) {
		return pop, nil // whole population; already ascending
	}

	// Order the population by a keyed hash, take the first K.
	idBytes := []byte(backup.BackupID)
	key := func(i int) uint64 {
		var buf [16]byte
		binary.BigEndian.PutUint64(buf[0:8], seed)
		binary.BigEndian.PutUint64(buf[8:16], uint64(i))
		h := sha256.Sum256(append(append([]byte(nil), idBytes...), buf[:]...))
		return binary.BigEndian.Uint64(h[0:8])
	}
	byKey := make([]int, len(pop))
	copy(byKey, pop)
	sort.Slice(byKey, func(a, b int) bool {
		ka, kb := key(byKey[a]), key(byKey[b])
		if ka != kb {
			return ka < kb
		}
		return byKey[a] < byKey[b] // tie-break for determinism
	})
	chosen := byKey[:k]
	sort.Ints(chosen)
	return chosen, nil
}
