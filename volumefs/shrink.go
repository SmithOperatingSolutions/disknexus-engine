// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Shrinking a filesystem in place (#223 E4).
//
// The engine plans shrinks (disklayout.PlanFit against MinimumSize) and
// places shrunk bytes (bmr.RestoreDiskFit); it does not rewrite NTFS or
// ext4 metadata itself. That is ntfsresize's and resize2fs's job, and the
// recovery ISO carries them. This is the seam between the two: the engine
// says WHAT to shrink and to what size; the executor runs the tool, or says
// by name which tool is missing.

// Shrinker shrinks the filesystem on a partition device to newLength bytes.
type Shrinker interface {
	Shrink(ctx context.Context, device, filesystem string, newLength int64) error
}

// ExternalShrinker runs the platform tools. Empty tool names mean the
// conventional binary found on PATH; Run, when set, replaces exec (tests).
type ExternalShrinker struct {
	NTFSResize string // ntfsresize (ntfs-3g-progs)
	Resize2fs  string // resize2fs (e2fsprogs)
	E2fsck     string // e2fsck (e2fsprogs)
	Run        func(ctx context.Context, name string, args ...string) ([]byte, error)
}

func (s ExternalShrinker) tool(configured, conventional string) string {
	if configured != "" {
		return configured
	}
	return conventional
}

func (s ExternalShrinker) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.Run != nil {
		return s.Run(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Available reports whether the tools a filesystem's shrink needs are
// present, naming the missing one. A flow calls it BEFORE writing anything.
func (s ExternalShrinker) Available(filesystem string) error {
	var need []string
	switch filesystem {
	case "ntfs":
		need = []string{s.tool(s.NTFSResize, "ntfsresize")}
	case "ext4":
		need = []string{s.tool(s.E2fsck, "e2fsck"), s.tool(s.Resize2fs, "resize2fs")}
	default:
		return fmt.Errorf("%s filesystems cannot be shrunk; only NTFS and ext4 can", filesystem)
	}
	for _, n := range need {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%s is not available on this system, and shrinking a %s filesystem needs it", n, filesystem)
		}
	}
	return nil
}

// Shrink shrinks the filesystem on device to newLength bytes. NTFS:
// `ntfsresize -f -s <bytes> <device>` (the tool relocates what it must and
// refuses what it cannot — a page file or MFT fragment inside the cut —
// and its refusal is returned verbatim). ext4: a forced check first, as
// resize2fs requires, then `resize2fs <device> <KiB>K`.
func (s ExternalShrinker) Shrink(ctx context.Context, device, filesystem string, newLength int64) error {
	if newLength <= 0 {
		return fmt.Errorf("shrink to %d bytes is not a size", newLength)
	}
	if err := s.Available(filesystem); err != nil && s.Run == nil {
		return err
	}
	switch filesystem {
	case "ntfs":
		out, err := s.run(ctx, s.tool(s.NTFSResize, "ntfsresize"), "-f", "-s", fmt.Sprint(newLength), device)
		if err != nil {
			return fmt.Errorf("ntfsresize refused to shrink %s to %d bytes: %v\n%s", device, newLength, err, strings.TrimSpace(string(out)))
		}
		return nil
	case "ext4":
		if out, err := s.run(ctx, s.tool(s.E2fsck, "e2fsck"), "-f", "-y", device); err != nil {
			return fmt.Errorf("e2fsck on %s did not come back clean: %v\n%s", device, err, strings.TrimSpace(string(out)))
		}
		kib := (newLength + 1023) / 1024
		out, err := s.run(ctx, s.tool(s.Resize2fs, "resize2fs"), device, fmt.Sprintf("%dK", kib))
		if err != nil {
			return fmt.Errorf("resize2fs refused to shrink %s to %d KiB: %v\n%s", device, kib, err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("%s filesystems cannot be shrunk; only NTFS and ext4 can", filesystem)
	}
}
