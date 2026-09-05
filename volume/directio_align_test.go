// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "testing"

// TestInitDirectIORoundsBufferSize guards issue #16: the O_DIRECT refill read
// used the raw user-supplied read_buffer_size. O_DIRECT devices reject any read
// whose length is not a sector multiple (EINVAL), so a non-multiple value
// failed every device read. initDirectIO must round bufferSize UP to a sector
// multiple — the alignment buffer was already rounded, but the refill length
// (r.bufferSize) was not.
func TestInitDirectIORoundsBufferSize(t *testing.T) {
	cases := []struct{ in, want int }{
		{1000, 1024},                     // non-multiple rounds up
		{512, 512},                       // exact multiple unchanged
		{1, 512},                         // tiny rounds to one sector
		{1024*1024 + 1, 1024*1024 + 512}, // large non-multiple
	}
	for _, c := range cases {
		r := &Reader{bufferSize: c.in}
		r.initDirectIO()
		if r.bufferSize != c.want {
			t.Errorf("initDirectIO(%d): bufferSize = %d, want %d (non-sector-multiple refills EINVAL on O_DIRECT)", c.in, r.bufferSize, c.want)
		}
		if r.bufferSize%readSectorSize != 0 {
			t.Errorf("initDirectIO(%d): bufferSize %d not a sector multiple", c.in, r.bufferSize)
		}
		// The refill slice alignedRead uses must fit in the allocation.
		if len(r.alignBuf) < r.alignOff+r.bufferSize {
			t.Errorf("initDirectIO(%d): alignBuf too small: len=%d need %d", c.in, len(r.alignBuf), r.alignOff+r.bufferSize)
		}
	}
}
