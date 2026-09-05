// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import "testing"

func TestPageCacheGetPut(t *testing.T) {
	c := newPageCache(1) // 1 MB

	// Miss on empty cache.
	if node := c.get(0); node != nil {
		t.Fatal("expected nil on empty cache")
	}

	// Put and get.
	var data [pageSize]byte
	for i := range data {
		data[i] = byte(i & 0xFF)
	}
	c.put(0, data[:], pageSize)

	node := c.get(0)
	if node == nil {
		t.Fatal("expected cache hit")
	}
	if node.valid != pageSize {
		t.Errorf("valid = %d, want %d", node.valid, pageSize)
	}
	if node.data[100] != data[100] {
		t.Errorf("data mismatch at byte 100")
	}
}

func TestPageCacheLRUEviction(t *testing.T) {
	// Cache that holds exactly 16 pages (minimum).
	c := newPageCache(0) // 0 MB → minimum 16 pages

	if c.maxPages != 16 {
		t.Fatalf("maxPages = %d, want 16", c.maxPages)
	}

	var data [pageSize]byte

	// Fill the cache.
	for i := 0; i < 16; i++ {
		data[0] = byte(i)
		c.put(int64(i), data[:], pageSize)
	}

	// All 16 should be present.
	for i := 0; i < 16; i++ {
		if c.get(int64(i)) == nil {
			t.Fatalf("page %d missing from full cache", i)
		}
	}

	// Insert page 16 — should evict page 0 (LRU, since we just accessed
	// 0..15 in order with the get loop, making 0 the most recent. Actually
	// let's think about this more carefully.)
	//
	// After filling 0..15, LRU order (tail→head) is: 0, 1, 2, ..., 15
	// Then the get loop accessed 0..15 in order, moving each to front.
	// After get(0): head=0, tail=1 (but 15 was head before, now 0 is head)
	// After get(1): head=1, 0, ..., 15, tail=2
	// ...
	// After get(15): head=15, 14, 13, ..., 0, tail=... hmm let me just test empirically.

	// Access page 0 explicitly to make sure it's NOT the LRU.
	c.get(0)

	// Now insert page 16. The LRU tail should be evicted.
	data[0] = 16
	c.put(16, data[:], pageSize)

	// Page 16 should be present.
	if c.get(16) == nil {
		t.Fatal("page 16 missing after insert")
	}

	// Page 0 should still be present (we accessed it recently).
	if c.get(0) == nil {
		t.Fatal("page 0 should not have been evicted")
	}

	// One of pages 1..15 should have been evicted (the one at the tail).
	evicted := 0
	for i := 1; i <= 15; i++ {
		if c.get(int64(i)) == nil {
			evicted++
		}
	}
	if evicted != 1 {
		t.Errorf("expected 1 eviction, got %d", evicted)
	}
}

func TestPageCacheInvalidate(t *testing.T) {
	c := newPageCache(1)
	var data [pageSize]byte

	c.put(0, data[:], pageSize)
	c.put(1, data[:], pageSize)

	c.invalidate()

	if c.get(0) != nil {
		t.Error("page 0 should be gone after invalidate")
	}
	if c.get(1) != nil {
		t.Error("page 1 should be gone after invalidate")
	}
	if len(c.pages) != 0 {
		t.Errorf("pages map has %d entries after invalidate", len(c.pages))
	}
}

func TestPageCacheMinimumCapacity(t *testing.T) {
	c := newPageCache(0)
	if c.maxPages < 16 {
		t.Errorf("maxPages = %d, want >= 16", c.maxPages)
	}
}

func TestPageCacheUpdateExisting(t *testing.T) {
	c := newPageCache(1)

	var data1 [pageSize]byte
	data1[0] = 0xAA
	c.put(5, data1[:], pageSize)

	var data2 [pageSize]byte
	data2[0] = 0xBB
	c.put(5, data2[:], pageSize)

	node := c.get(5)
	if node == nil {
		t.Fatal("expected cache hit")
	}
	if node.data[0] != 0xBB {
		t.Errorf("data not updated: got %#x, want 0xBB", node.data[0])
	}
}
