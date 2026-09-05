// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline

// White-box unit tests for seqHeap, which is the min-heap used by the
// sequencer goroutine to re-order PreparedChunks into their original input
// sequence. These tests live in package pipeline (not pipeline_test) so they
// can access the unexported seq field.

import (
	"container/heap"
	"testing"
)

// TestSeqHeapOrdering verifies that the heap emits PreparedChunks in
// ascending seq order regardless of insertion order.
func TestSeqHeapOrdering(t *testing.T) {
	insertOrder := []uint64{5, 2, 8, 0, 3, 1, 7, 4, 6}

	h := &seqHeap{}
	heap.Init(h)
	for _, s := range insertOrder {
		heap.Push(h, PreparedChunk{seq: s})
	}

	for want := uint64(0); want < uint64(len(insertOrder)); want++ {
		if h.Len() == 0 {
			t.Fatalf("heap empty at seq %d", want)
		}
		got := heap.Pop(h).(PreparedChunk).seq
		if got != want {
			t.Errorf("Pop() seq = %d, want %d", got, want)
		}
	}
	if h.Len() != 0 {
		t.Errorf("heap should be empty after draining, Len() = %d", h.Len())
	}
}

// TestSeqHeapReverse is the worst case: chunks inserted in reverse order.
func TestSeqHeapReverse(t *testing.T) {
	const n = 100
	h := &seqHeap{}
	heap.Init(h)
	for i := n - 1; i >= 0; i-- {
		heap.Push(h, PreparedChunk{seq: uint64(i)})
	}
	for want := uint64(0); want < n; want++ {
		got := heap.Pop(h).(PreparedChunk).seq
		if got != want {
			t.Fatalf("Pop() seq = %d, want %d", got, want)
		}
	}
}

// TestSeqHeapSingle verifies a single-element heap works correctly.
func TestSeqHeapSingle(t *testing.T) {
	h := &seqHeap{}
	heap.Init(h)
	heap.Push(h, PreparedChunk{seq: 42})
	if h.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", h.Len())
	}
	got := heap.Pop(h).(PreparedChunk).seq
	if got != 42 {
		t.Errorf("Pop() seq = %d, want 42", got)
	}
	if h.Len() != 0 {
		t.Errorf("Len() after Pop() = %d, want 0", h.Len())
	}
}

// TestSeqHeapDuplicates verifies the heap handles duplicate seq values
// without panicking (shouldn't occur in practice, but must not corrupt state).
func TestSeqHeapDuplicates(t *testing.T) {
	h := &seqHeap{}
	heap.Init(h)
	for range 3 {
		heap.Push(h, PreparedChunk{seq: 0})
	}
	if h.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", h.Len())
	}
	for i := range 3 {
		got := heap.Pop(h).(PreparedChunk).seq
		if got != 0 {
			t.Errorf("Pop()[%d] seq = %d, want 0", i, got)
		}
	}
}
