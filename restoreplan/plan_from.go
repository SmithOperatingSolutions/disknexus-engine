// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restoreplan

import "github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"

// planBatch is how many entries BuildFrom reads per Range call.
const planBatch = 1 << 16

// BuildFrom is Build over an EntryAccessor (#506): the same per-pack
// counting, reading entries in batches so a 1.26M-entry chain never has
// to exist as one slice. Build delegates here through a slice accessor.
func BuildFrom(ea manifest.EntryAccessor, packOf func([32]byte) (uint32, bool), packMax int64) *Plan {
	p := &Plan{neededChunks: map[uint32]int{}, dense: map[uint32]bool{}}
	var totalLen, n int64
	count := ea.Count()
	for lo := int64(0); lo < count; lo += planBatch {
		hi := lo + planBatch
		if hi > count {
			hi = count
		}
		batch, err := ea.Range(lo, hi)
		if err != nil {
			// A plan is an optimization: an unreadable range degrades to
			// "nothing is dense" for that range, and the restore's own read
			// of the same entries reports the error where it matters.
			continue
		}
		for _, e := range batch {
			if e.IsExcluded {
				continue
			}
			if pk, ok := packOf(e.ChunkHash); ok {
				p.neededChunks[pk]++
				totalLen += int64(e.ChunkLength)
				n++
			}
		}
	}
	if n == 0 {
		return p
	}
	avgChunk := totalLen / n
	if avgChunk <= 0 {
		avgChunk = 64 << 10
	}
	packCapacity := packMax / avgChunk
	threshold := packCapacity / 64
	if threshold < 4 {
		threshold = 4
	}
	for pk, cnt := range p.neededChunks {
		if int64(cnt) >= threshold {
			p.dense[pk] = true
		}
	}
	return p
}
