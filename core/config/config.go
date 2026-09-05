// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

const (
	// 64 KB geometry is the default as of v0.7.5 (#83 field decision):
	// volume/disk capture is the flagship path, and 8 KB chunks degrade
	// badly at that scale (~1.2% metadata/full vs ~0.15%, 8x the per-chunk
	// overhead). The old geometry survives as the "fine-grained" profile.
	DefaultChunkMinSize     = 16 * 1024         // 16 KB
	DefaultChunkAvgSize     = 64 * 1024         // 64 KB
	DefaultChunkMaxSize     = 512 * 1024        // 512 KB
	DefaultBuzhashMask      = 0xFFFF            // 16 bits → ~64 KB average
	DefaultPackFileMaxSize  = 128 * 1024 * 1024 // 128 MB
	DefaultCompressionLevel = 3                 // zstd level
	DefaultReadBufferSize   = 1024 * 1024       // 1 MB
	DefaultBloomFPRate      = 0.001             // 0.1%
	DefaultIndexCacheMB     = 256
	DefaultMemoryBudgetMB   = 512
)

// Config holds all configuration for the backup engine.
type Config struct {
	// Chunking
	ChunkMinSize int    `json:"chunk_min_size"`
	ChunkAvgSize int    `json:"chunk_avg_size"`
	ChunkMaxSize int    `json:"chunk_max_size"`
	BuzhashMask  uint64 `json:"buzhash_mask"`

	// Storage
	PackFileMaxSize  int64  `json:"pack_file_max_size"`
	CompressionLevel int    `json:"compression_level"`
	RepoPath         string `json:"repo_path"`

	// Performance
	HashWorkers    int `json:"hash_workers"`
	MemoryBudgetMB int `json:"memory_budget_mb"`
	ReadBufferSize int `json:"read_buffer_size"`

	// Index
	BloomFPRate     float64 `json:"bloom_fp_rate"`
	IndexCacheMB    int     `json:"index_cache_mb"`
	MemFlushedIndex bool    `json:"mem_flushed_index"` // keep flushed set in RAM (O(n) memory, O(1) lookup)

	// Preprocessing
	ExcludeVolatileFiles bool `json:"exclude_volatile_files"`
	NormalizeNTFS        bool `json:"normalize_ntfs"`
}

func defaultWorkers() int {
	n := runtime.NumCPU()
	if n > 1 {
		return n - 1
	}
	return 1
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		ChunkMinSize:         DefaultChunkMinSize,
		ChunkAvgSize:         DefaultChunkAvgSize,
		ChunkMaxSize:         DefaultChunkMaxSize,
		BuzhashMask:          DefaultBuzhashMask,
		PackFileMaxSize:      DefaultPackFileMaxSize,
		CompressionLevel:     DefaultCompressionLevel,
		RepoPath:             "",
		HashWorkers:          defaultWorkers(),
		MemoryBudgetMB:       DefaultMemoryBudgetMB,
		ReadBufferSize:       DefaultReadBufferSize,
		BloomFPRate:          DefaultBloomFPRate,
		IndexCacheMB:         DefaultIndexCacheMB,
		ExcludeVolatileFiles: true,
	}
}

// Load reads a config file and returns the merged configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing config %s: %w", path, err)
	}
	return cfg, nil
}

// Save writes the configuration to a JSON file.
func (c Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}
