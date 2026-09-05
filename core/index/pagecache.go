// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import "sync"

const pageSize = 4096

// pageNode is a doubly-linked list node holding one cached page.
type pageNode struct {
	pageNum int64
	data    [pageSize]byte
	valid   int // number of valid bytes in data (may be < pageSize for last page)
	prev    *pageNode
	next    *pageNode
}

// pageCache is a page-level LRU cache for the on-disk hash index.
// It caches aligned 4096-byte pages to exploit spatial locality in binary
// search and sequential chunk hash lookups.
type pageCache struct {
	mu       sync.Mutex
	maxPages int
	pages    map[int64]*pageNode
	head     *pageNode // most recently used
	tail     *pageNode // least recently used
}

// newPageCache creates a page cache with the given size budget in megabytes.
// Minimum 16 pages regardless of budget.
func newPageCache(maxMB int) *pageCache {
	maxPages := (maxMB * 1024 * 1024) / pageSize
	if maxPages < 16 {
		maxPages = 16
	}
	return &pageCache{
		maxPages: maxPages,
		pages:    make(map[int64]*pageNode),
	}
}

// get returns the cached page for the given page number, or nil on miss.
// On hit, moves the page to the front of the LRU list.
func (c *pageCache) get(pageNum int64) *pageNode {
	c.mu.Lock()
	defer c.mu.Unlock()

	node, ok := c.pages[pageNum]
	if !ok {
		return nil
	}
	c.moveToFront(node)
	return node
}

// put inserts a page into the cache. If the cache is full, the LRU page
// is evicted.
func (c *pageCache) put(pageNum int64, data []byte, valid int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already cached, update in place.
	if node, ok := c.pages[pageNum]; ok {
		copy(node.data[:], data)
		node.valid = valid
		c.moveToFront(node)
		return
	}

	// Evict if full.
	for len(c.pages) >= c.maxPages {
		c.evictTail()
	}

	node := &pageNode{pageNum: pageNum, valid: valid}
	copy(node.data[:], data)
	c.pages[pageNum] = node
	c.pushFront(node)
}

// invalidate clears all cached pages.
func (c *pageCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.pages = make(map[int64]*pageNode)
	c.head = nil
	c.tail = nil
}

// moveToFront moves an existing node to the head of the list.
func (c *pageCache) moveToFront(node *pageNode) {
	if c.head == node {
		return
	}
	// Unlink
	if node.prev != nil {
		node.prev.next = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	}
	if c.tail == node {
		c.tail = node.prev
	}
	// Push to front
	node.prev = nil
	node.next = c.head
	if c.head != nil {
		c.head.prev = node
	}
	c.head = node
	if c.tail == nil {
		c.tail = node
	}
}

// pushFront inserts a new node at the head.
func (c *pageCache) pushFront(node *pageNode) {
	node.prev = nil
	node.next = c.head
	if c.head != nil {
		c.head.prev = node
	}
	c.head = node
	if c.tail == nil {
		c.tail = node
	}
}

// evictTail removes the least recently used page.
func (c *pageCache) evictTail() {
	if c.tail == nil {
		return
	}
	node := c.tail
	delete(c.pages, node.pageNum)
	if node.prev != nil {
		node.prev.next = nil
	}
	c.tail = node.prev
	if c.tail == nil {
		c.head = nil
	}
}
