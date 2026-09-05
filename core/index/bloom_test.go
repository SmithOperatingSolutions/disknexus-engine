// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
)

func TestBloomFilterBasic(t *testing.T) {
	bf := index.NewBloomFilter(1000, 0.001)

	// Should not contain anything initially
	if bf.MayContain(42) {
		t.Error("empty bloom filter should not contain 42")
	}

	// Add and check
	bf.Add(42)
	if !bf.MayContain(42) {
		t.Error("bloom filter should contain 42 after Add")
	}

	if bf.Count() != 1 {
		t.Errorf("expected count 1, got %d", bf.Count())
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	n := uint64(10000)
	bf := index.NewBloomFilter(n, 0.01)

	// Add n items
	for i := uint64(0); i < n; i++ {
		bf.Add(i)
	}

	// Check items we added
	for i := uint64(0); i < n; i++ {
		if !bf.MayContain(i) {
			t.Fatalf("false negative for %d", i)
		}
	}

	// Count false positives for items NOT added
	falsePositives := 0
	testN := uint64(100000)
	for i := n; i < n+testN; i++ {
		if bf.MayContain(i) {
			falsePositives++
		}
	}

	fpRate := float64(falsePositives) / float64(testN)
	t.Logf("false positive rate: %.4f%% (%d/%d)", fpRate*100, falsePositives, testN)

	// Allow 5x the target rate as margin
	if fpRate > 0.05 {
		t.Errorf("false positive rate too high: %.4f%% (target 1%%)", fpRate*100)
	}
}

func TestBloomFilterSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bloom.bin")

	bf := index.NewBloomFilter(1000, 0.001)
	for i := uint64(0); i < 100; i++ {
		bf.Add(i * 7)
	}

	if err := bf.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := index.LoadBloomFilter(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Count() != bf.Count() {
		t.Errorf("count mismatch: %d vs %d", loaded.Count(), bf.Count())
	}

	// Verify all added items are still present
	for i := uint64(0); i < 100; i++ {
		if !loaded.MayContain(i * 7) {
			t.Errorf("loaded bloom missing %d", i*7)
		}
	}
}

func TestBloomFilterLoadInvalidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.bin")

	os.WriteFile(path, []byte("too short"), 0644)

	_, err := index.LoadBloomFilter(path)
	if err == nil {
		t.Fatal("expected error for invalid bloom file")
	}
}
