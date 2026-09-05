// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config file is merged OVER the defaults: a key the file omits keeps its
// default, a key it sets wins, and what Save writes Load reads back. An
// operator's partial config must not silently zero the chunk geometry.
func TestLoadMergesTheFileOverDefaultsAndSaveRoundTrips(t *testing.T) {
	def := Default()
	if def.ChunkAvgSize <= def.ChunkMinSize || def.ChunkMaxSize <= def.ChunkAvgSize {
		t.Fatalf("default geometry is not min < avg < max: %d/%d/%d", def.ChunkMinSize, def.ChunkAvgSize, def.ChunkMaxSize)
	}
	if def.HashWorkers < 1 || !def.ExcludeVolatileFiles || def.PackFileMaxSize <= 0 || def.BloomFPRate <= 0 || def.BloomFPRate >= 1 {
		t.Fatalf("defaults are not usable as-is: %+v", def)
	}

	dir := t.TempDir()
	partial := filepath.Join(dir, "partial.json")
	if err := os.WriteFile(partial, []byte(`{"compression_level": 9, "exclude_volatile_files": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(partial)
	if err != nil {
		t.Fatal(err)
	}
	if got.CompressionLevel != 9 || got.ExcludeVolatileFiles {
		t.Fatalf("the file's keys did not win: %+v", got)
	}
	if got.ChunkAvgSize != def.ChunkAvgSize || got.PackFileMaxSize != def.PackFileMaxSize || got.HashWorkers != def.HashWorkers {
		t.Fatalf("keys the file omits lost their defaults: %+v", got)
	}

	got.ChunkAvgSize = 12345
	saved := filepath.Join(dir, "saved.json")
	if err := got.Save(saved); err != nil {
		t.Fatal(err)
	}
	back, err := Load(saved)
	if err != nil {
		t.Fatal(err)
	}
	if back != got {
		t.Fatalf("Save/Load round trip differs:\n got  %+v\n want %+v", back, got)
	}

	// A missing or malformed file is an error — and the defaults still come
	// back, so a caller that chooses to continue does so with a sane config.
	if cfg, err := Load(filepath.Join(dir, "missing.json")); err == nil || cfg.ChunkAvgSize != def.ChunkAvgSize {
		t.Fatalf("missing file: err=%v cfg=%+v", err, cfg)
	}
	if err := os.WriteFile(partial, []byte(`{"compression_level": `), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(partial); err == nil {
		t.Fatal("a truncated config file loaded without error")
	}
}
