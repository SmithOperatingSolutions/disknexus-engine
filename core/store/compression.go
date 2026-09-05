// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import "github.com/klauspost/compress/zstd"

// Compression settings for a repo.
//
// RepoConfig.CompressionLevel is an integer, but the encoder has only four
// presets and newChunkStore collapses the integer onto them. That collapse
// lives here so there is ONE definition of what a level means: the engine
// uses it to build the encoder, and the controller derives the choices it
// offers the panel from it. A menu of 0-9 would advertise ten settings where
// four exist.
//
// Note for anyone reading a config: level 0 is not "uncompressed". It selects
// zstd's FASTEST preset, and ChunkStore.Store encodes every chunk
// unconditionally. There is no way to store a chunk uncompressed.

// CompressionPreset returns the zstd encoder preset a config compression level
// selects. This is the mapping the write path actually uses.
func CompressionPreset(level int) zstd.EncoderLevel {
	switch {
	case level <= 1:
		return zstd.SpeedFastest
	case level <= 3:
		return zstd.SpeedDefault
	case level <= 6:
		return zstd.SpeedBetterCompression
	default:
		return zstd.SpeedBestCompression
	}
}

// CompressionChoice is one meaningfully-distinct compression setting: the
// level to persist and the preset that level selects.
type CompressionChoice struct {
	Name   string
	Level  int
	Preset zstd.EncoderLevel
}

// CompressionChoices lists the settings that actually differ, weakest to
// strongest — one per preset CompressionPreset can return.
//
// Every level here is deliberately NON-ZERO. A stored 0 is indistinguishable
// from an absent field, and the two read paths disagree about it: the CLI's
// applyRepoStorageConfig ignores it (`if rc.CompressionLevel > 0`, keeping
// config.Default()) while the cloud path assigns it literally. "Fastest"
// therefore sends 1, not 0, so the choice means the same thing whichever
// path writes the repo.
func CompressionChoices() []CompressionChoice {
	return []CompressionChoice{
		{Name: "fastest", Level: 1, Preset: zstd.SpeedFastest},
		{Name: "balanced", Level: 3, Preset: zstd.SpeedDefault},
		{Name: "better", Level: 6, Preset: zstd.SpeedBetterCompression},
		{Name: "best", Level: 9, Preset: zstd.SpeedBestCompression},
	}
}
