// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
)

func ntfsFixture(t *testing.T) string {
	t.Helper()
	// Module-relative: the same path in the workspace and in the engine repo
	// after the split (a "../../engine/..." spelling only resolves when the
	// checkout happens to be named engine — a skip, i.e. a deleted test).
	p := filepath.Join("..", "volumefs", "testdata", "ntfs.img")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("ntfs.img fixture not available: %v", err)
	}
	return p
}

// TestBuildCaptureExclusions_Disabled: config off means no map at all.
func TestBuildCaptureExclusions_Disabled(t *testing.T) {
	cfg := config.Default()
	cfg.ExcludeVolatileFiles = false
	if m := BuildCaptureExclusions(cfg, ntfsFixture(t), "", 8<<20); m != nil {
		t.Fatal("exclusions built while disabled")
	}
}

// TestBuildCaptureExclusions_VolatileFromImage: an NTFS source always yields
// $LogFile ranges, so the map must be non-nil for NTFS captures.
func TestBuildCaptureExclusions_VolatileFromImage(t *testing.T) {
	cfg := config.Default()
	m := BuildCaptureExclusions(cfg, ntfsFixture(t), "", 8<<20)
	if m == nil || m.Len() == 0 {
		t.Fatal("no volatile exclusions for NTFS image ($LogFile expected)")
	}
}

// TestBuildCaptureExclusions_ForeignPathGuard is the cross-volume safety
// guarantee: a repo/temp path that is NOT on the captured volume must add
// nothing to the map — its extents are offsets on a different device, and
// excluding them would zero unrelated ranges of the captured stream.
func TestBuildCaptureExclusions_ForeignPathGuard(t *testing.T) {
	cfg := config.Default()
	img := ntfsFixture(t)

	base := BuildCaptureExclusions(cfg, img, "", 8<<20)
	if base == nil {
		t.Fatal("no baseline volatile map")
	}
	// A real local directory that is definitely not "on" the captured image.
	foreign := t.TempDir()
	withForeign := BuildCaptureExclusions(cfg, img, "", 8<<20, foreign)
	if withForeign == nil {
		t.Fatal("map vanished when a foreign path was passed")
	}
	if withForeign.Len() != base.Len() {
		t.Fatalf("foreign path changed the exclusion map: %d != %d ranges — cross-volume guard is broken",
			withForeign.Len(), base.Len())
	}
}

// TestStaleCloudTempDirsIgnoresTheEnvironment (#542): the scan covers
// exactly the bases it is handed. It used to read DISKNEXUS_TEMP and the
// ambient temp itself — product knowledge inside the engine — and the
// product now passes every base (captureflow.scratchBases, which is where
// the DISKNEXUS_TEMP test moved). A stale dir under a base the caller did
// NOT name must not be returned, environment or no environment: an engine
// consumer that never set that variable must not have its scan steered by
// it.
func TestStaleCloudTempDirsIgnoresTheEnvironment(t *testing.T) {
	envBase := t.TempDir()
	t.Setenv("DISKNEXUS_TEMP", envBase)
	envStale := filepath.Join(envBase, "disknexus-s3-12345")
	if err := os.MkdirAll(envStale, 0700); err != nil {
		t.Fatal(err)
	}
	// Positive control (§4): named as a base, it IS found.
	if got := StaleCloudTempDirs(envBase); len(got) != 1 || got[0] != envStale {
		t.Fatalf("control: the stale dir under a NAMED base was not found: %v", got)
	}
	// Not named: not found, whatever the environment says.
	for _, got := range [][]string{StaleCloudTempDirs(), StaleCloudTempDirs(t.TempDir())} {
		for _, d := range got {
			if d == envStale {
				t.Fatalf("stale dir %s was returned from a scan that did not name its base — the engine is reading DISKNEXUS_TEMP", envStale)
			}
		}
	}
}

// TestStaleCloudTempDirsScansExtraBases (#315): crashed-run work dirs live
// wherever scratch NOW goes — for the agent that is <stateDir>/tmp — so the
// scan takes explicit extra bases. Empty and duplicate bases are harmless.
func TestStaleCloudTempDirsScansExtraBases(t *testing.T) {
	t.Setenv("DISKNEXUS_TEMP", "")
	base := filepath.Join(t.TempDir(), "state", "tmp")
	stale := filepath.Join(base, "disknexus-s3-98765")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	got := StaleCloudTempDirs("", base, base)
	found, count := false, 0
	for _, d := range got {
		if d == stale {
			found = true
			count++
		}
	}
	if !found {
		t.Fatalf("stale dir %s under the extra base not found in %v — the #297 exclusion no longer "+
			"matches the agent's own crashed runs after the #315 relocation", stale, got)
	}
	if count != 1 {
		t.Fatalf("stale dir %s reported %d times (duplicate bases must dedupe)", stale, count)
	}
}
