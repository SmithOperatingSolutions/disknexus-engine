// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}

// The executor's contract is its argv: NTFS shrinks with ntfsresize forced
// to the exact byte size; ext4 runs a forced e2fsck first (resize2fs
// refuses without one) and then resize2fs in KiB; a tool's refusal is
// returned verbatim; a filesystem the tools cannot shrink is refused by
// name before any tool runs.
func TestExternalShrinkerInvokesTheRightToolWithTheRightArguments(t *testing.T) {
	var calls []call
	rec := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{name, args})
		return []byte("ok"), nil
	}
	s := ExternalShrinker{Run: rec}
	if err := s.Shrink(context.Background(), "/dev/sdb2", "ntfs", 123456789); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].name != "ntfsresize" || strings.Join(calls[0].args, " ") != "-f -s 123456789 /dev/sdb2" {
		t.Fatalf("ntfs shrink ran %+v, want ntfsresize -f -s 123456789 /dev/sdb2", calls)
	}
	calls = nil
	if err := s.Shrink(context.Background(), "/dev/sdb3", "ext4", 10*1024*1024+1); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].name != "e2fsck" || strings.Join(calls[0].args, " ") != "-f -y /dev/sdb3" ||
		calls[1].name != "resize2fs" || strings.Join(calls[1].args, " ") != "/dev/sdb3 10241K" {
		t.Fatalf("ext4 shrink ran %+v, want e2fsck -f -y then resize2fs /dev/sdb3 10241K (rounded up)", calls)
	}
	// Configured tool paths are used verbatim.
	calls = nil
	custom := ExternalShrinker{NTFSResize: "/opt/ntfs/ntfsresize", E2fsck: "/opt/e2/e2fsck", Resize2fs: "/opt/e2/resize2fs", Run: rec}
	custom.Shrink(context.Background(), "/dev/x", "ntfs", 4096)
	custom.Shrink(context.Background(), "/dev/y", "ext4", 4096)
	if calls[0].name != "/opt/ntfs/ntfsresize" || calls[1].name != "/opt/e2/e2fsck" || calls[2].name != "/opt/e2/resize2fs" {
		t.Fatalf("configured tool paths not used: %+v", calls)
	}

	// A tool's refusal comes back verbatim, and ext4 stops at a dirty check.
	refuse := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "ntfsresize" {
			return []byte("ERROR: Cluster 12345 referenced by $MFT is beyond the new size"), errors.New("exit status 1")
		}
		if name == "e2fsck" {
			return []byte("Filesystem still has errors"), errors.New("exit status 4")
		}
		t.Fatalf("resize2fs ran after a dirty e2fsck")
		return nil, nil
	}
	r := ExternalShrinker{Run: refuse}
	if err := r.Shrink(context.Background(), "/dev/sdb2", "ntfs", 4096); err == nil || !strings.Contains(err.Error(), "beyond the new size") {
		t.Fatalf("ntfsresize's refusal was not returned verbatim: %v", err)
	}
	if err := r.Shrink(context.Background(), "/dev/sdb3", "ext4", 4096); err == nil || !strings.Contains(err.Error(), "still has errors") {
		t.Fatalf("a dirty e2fsck did not stop the shrink: %v", err)
	}
	// Refusals before any tool runs.
	calls = nil
	if err := s.Shrink(context.Background(), "/dev/sdb1", "fat32", 4096); err == nil || !strings.Contains(err.Error(), "fat32") || len(calls) != 0 {
		t.Fatalf("fat32: err=%v calls=%v", err, calls)
	}
	if err := s.Shrink(context.Background(), "/dev/sdb1", "ntfs", 0); err == nil || len(calls) != 0 {
		t.Fatalf("zero size: err=%v calls=%v", err, calls)
	}
}

// Without the seam, the executor really execs: fake tools on PATH record
// their argv, and a missing tool is named before anything runs.
func TestExternalShrinkerExecsToolsOnPathAndNamesMissingOnes(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	// The fake tool appends "<name> <argv>" to the log: a shell script on
	// POSIX, a .bat on Windows (LookPath there resolves only PATHEXT names).
	fakeTool := func(tool string) string {
		path := filepath.Join(dir, tool)
		script := "#!/bin/sh\necho \"" + tool + " $@\" >> \"" + log + "\"\n"
		if runtime.GOOS == "windows" {
			path += ".bat"
			script = "@echo " + tool + " %*>>\"" + log + "\"\r\n"
		}
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var ntfsresizePath string
	for _, tool := range []string{"ntfsresize", "e2fsck", "resize2fs"} {
		if p := fakeTool(tool); tool == "ntfsresize" {
			ntfsresizePath = p
		}
	}
	t.Setenv("PATH", dir)
	s := ExternalShrinker{}
	if err := s.Available("ntfs"); err != nil {
		t.Fatalf("ntfsresize on PATH reported unavailable: %v", err)
	}
	if err := s.Shrink(context.Background(), "/dev/fake1", "ext4", 2048*1024); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	if len(lines) != 2 || lines[0] != "e2fsck -f -y /dev/fake1" || lines[1] != "resize2fs /dev/fake1 2048K" {
		t.Fatalf("exec'd argv:\n%s", got)
	}
	// Positive control done; now the missing tool.
	if err := os.Remove(ntfsresizePath); err != nil {
		t.Fatal(err)
	}
	err := s.Available("ntfs")
	if err == nil || !strings.Contains(err.Error(), "ntfsresize is not available") {
		t.Fatalf("missing ntfsresize: %v", err)
	}
	before, _ := os.ReadFile(log)
	if err := s.Shrink(context.Background(), "/dev/fake2", "ntfs", 4096); err == nil || !strings.Contains(err.Error(), "ntfsresize is not available") {
		t.Fatalf("shrink without the tool: %v", err)
	}
	after, _ := os.ReadFile(log)
	if string(after) != string(before) {
		t.Fatal("a shrink refused for a missing tool still ran something")
	}
	if err := s.Available("exfat"); err == nil || !strings.Contains(err.Error(), "exfat") {
		t.Fatalf("exfat availability: %v", err)
	}
}
