// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import "fmt"

// chainEntryAccessor presents several accessors as one, in order — an
// incremental backup's own entries followed by its parents' (#506). The
// restore used to materialize the whole chain into one []Entry; at 1.26M
// entries that was ~100 MB of structs on the recovery ISO's 2 GB. A chain
// over DNM accessors keeps every entry on disk until it is read.
type chainEntryAccessor struct {
	parts  []EntryAccessor
	starts []int64 // cumulative start index of each part
	total  int64
}

// ChainEntryAccessor concatenates accessors. Indices run across the parts in
// order; an empty chain has Count 0.
func ChainEntryAccessor(parts ...EntryAccessor) EntryAccessor {
	c := &chainEntryAccessor{}
	for _, p := range parts {
		if p == nil {
			continue
		}
		c.parts = append(c.parts, p)
		c.starts = append(c.starts, c.total)
		c.total += p.Count()
	}
	return c
}

func (c *chainEntryAccessor) Count() int64 { return c.total }

// part finds the accessor holding global index i and i's index within it.
func (c *chainEntryAccessor) part(i int64) (int, int64) {
	lo, hi := 0, len(c.parts)-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if c.starts[mid] <= i {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, i - c.starts[lo]
}

func (c *chainEntryAccessor) At(i int64) (Entry, error) {
	if i < 0 || i >= c.total {
		return Entry{}, fmt.Errorf("entry %d out of range [0,%d)", i, c.total)
	}
	p, local := c.part(i)
	return c.parts[p].At(local)
}

// Range reads [start, end) across part boundaries with one Range per part
// touched — the sequential read a DNM accessor is fast at.
func (c *chainEntryAccessor) Range(start, end int64) ([]Entry, error) {
	if start < 0 || end > c.total || start > end {
		return nil, fmt.Errorf("entry range [%d,%d) out of range [0,%d)", start, end, c.total)
	}
	out := make([]Entry, 0, end-start)
	for start < end {
		p, local := c.part(start)
		partEnd := c.parts[p].Count()
		take := end - start
		if rem := partEnd - local; rem < take {
			take = rem
		}
		if take <= 0 {
			// A part whose Count disagrees with its indices: fail, never spin.
			return nil, fmt.Errorf("entry chain: no progress at index %d (part %d holds %d)", start, p, partEnd)
		}
		es, err := c.parts[p].Range(local, local+take)
		if err != nil {
			return nil, err
		}
		out = append(out, es...)
		start += take
	}
	return out, nil
}
