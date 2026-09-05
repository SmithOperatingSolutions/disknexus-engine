// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package store

import (
	"testing"

	"github.com/klauspost/compress/zstd"
)

// The repo config carries an integer compression_level, but the encoder only
// has four presets: newChunkStore collapses the integer onto them. Offering
// 0-9 in a UI would pretend to ten settings where four exist, so the panel
// offers four named choices — and each one has to send a level that really
// selects the preset it claims. These tests derive the mapping from the
// engine so a change to the switch fails here rather than shipping a menu
// whose labels quietly stop matching what the encoder does.

// distinctPresets walks a wide range of config levels and returns the presets
// the engine can actually produce, in level order. Derived, never listed: if
// newChunkStore's boundaries move, so does this.
func distinctPresets(t *testing.T) []zstd.EncoderLevel {
	t.Helper()
	var out []zstd.EncoderLevel
	for level := -5; level <= 25; level++ {
		p := CompressionPreset(level)
		if len(out) == 0 || out[len(out)-1] != p {
			for _, seen := range out {
				if seen == p {
					t.Fatalf("preset %v reappears at level %d after a different preset — the mapping is not monotonic", p, level)
				}
			}
			out = append(out, p)
		}
	}
	return out
}

// TestCompressionPresetMatchesEncoder pins the levels whose meaning the API
// and CLI have always had, so the refactor that gave the switch a name cannot
// change behaviour. Level 0 is NOT "no compression": it is zstd's fastest
// preset, and every chunk is encoded regardless.
func TestCompressionPresetMatchesEncoder(t *testing.T) {
	cases := []struct {
		level int
		want  zstd.EncoderLevel
	}{
		{-1, zstd.SpeedFastest}, {0, zstd.SpeedFastest}, {1, zstd.SpeedFastest},
		{2, zstd.SpeedDefault}, {3, zstd.SpeedDefault},
		{4, zstd.SpeedBetterCompression}, {6, zstd.SpeedBetterCompression},
		{7, zstd.SpeedBestCompression}, {9, zstd.SpeedBestCompression}, {22, zstd.SpeedBestCompression},
	}
	for _, tc := range cases {
		if got := CompressionPreset(tc.level); got != tc.want {
			t.Errorf("CompressionPreset(%d) = %v, want %v", tc.level, got, tc.want)
		}
	}
}

// TestCompressionChoicesCoverEveryPreset: the offered menu must be exactly
// the set of settings that actually differ — every preset reachable, none
// twice. A fifth choice that duplicates a preset is a menu entry that does
// nothing; a missing one hides a capability.
func TestCompressionChoicesCoverEveryPreset(t *testing.T) {
	presets := distinctPresets(t)
	choices := CompressionChoices()
	if len(choices) != len(presets) {
		t.Fatalf("%d choices offered for %d distinct presets (%v)", len(choices), len(presets), presets)
	}
	for i, c := range choices {
		// The level a choice sends must select the preset it claims.
		if got := CompressionPreset(c.Level); got != c.Preset {
			t.Errorf("choice %q sends level %d, which selects %v, not the %v it claims", c.Name, c.Level, got, c.Preset)
		}
		// Offered weakest-to-strongest, matching the presets' own order.
		if c.Preset != presets[i] {
			t.Errorf("choice %d (%q) is %v, want %v — choices must run weakest to strongest", i, c.Name, c.Preset, presets[i])
		}
		if c.Name == "" {
			t.Errorf("choice %d has no name", i)
		}
	}
}

// TestCompressionChoiceLevelsSurviveTheUnsetConvention: a stored level of 0 is
// indistinguishable from "field absent", and the CLI's applyRepoStorageConfig
// reads it as unset (`if rc.CompressionLevel > 0`) while the cloud path
// applies it literally. A choice that sent 0 would therefore mean SpeedFastest
// on one path and SpeedDefault on the other, for the same repo. Every offered
// level must be non-zero so the menu means one thing everywhere.
func TestCompressionChoiceLevelsSurviveTheUnsetConvention(t *testing.T) {
	for _, c := range CompressionChoices() {
		if c.Level <= 0 {
			t.Errorf("choice %q sends level %d: zero/negative is read as \"unset\" by applyRepoStorageConfig, so this choice would not survive a CLI backup", c.Name, c.Level)
		}
	}
}

// TestChoiceLevelsAreBucketBoundaries makes an implicit coupling explicit.
// The repo detail page has to name the preset a STORED level selects — a
// level that need not be one of the four offered (an older API caller could
// have sent 2). It does that from the catalog alone, by taking the first
// choice whose level is >= the stored one, which is correct only because each
// choice's level is the top of its bucket. Assert that rather than trust it:
// otherwise a choice re-pointed at, say, level 2 would silently mislabel
// existing repos in the UI.
func TestChoiceLevelsAreBucketBoundaries(t *testing.T) {
	choices := CompressionChoices()
	for level := -5; level <= 25; level++ {
		want := CompressionPreset(level)
		got := choices[len(choices)-1].Preset // nothing matched => strongest
		for _, c := range choices {
			if level <= c.Level {
				got = c.Preset
				break
			}
		}
		if got != want {
			t.Fatalf("level %d: first-choice-at-or-above lookup gives %v, engine gives %v — choice levels are no longer bucket boundaries, so the panel would mislabel stored levels",
				level, got, want)
		}
	}
}

// TestApplyProfileNeverTouchesCompression: geometry and compression are
// orthogonal knobs (owner decision). A profile that also set a compression
// level would silently overwrite the operator's separate choice.
func TestApplyProfileNeverTouchesCompression(t *testing.T) {
	for _, name := range ProfileNames() {
		for _, level := range []int{0, 1, 3, 9} {
			cfg := RepoConfig{CompressionLevel: level}
			if err := ApplyProfile(&cfg, name); err != nil {
				t.Fatal(err)
			}
			if cfg.CompressionLevel != level {
				t.Errorf("profile %q changed compression %d -> %d; geometry and compression are independent",
					name, level, cfg.CompressionLevel)
			}
		}
	}
}
