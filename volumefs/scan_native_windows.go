//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	govss "github.com/SmithOperatingSolutions/go-vss"
)

var getVolumeInformationW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetVolumeInformationW")

// isNTFSVolume returns true when the volume identified by drive (e.g. "C:")
// is formatted as NTFS. FSCTL_GET_RETRIEVAL_POINTERS only works on NTFS, so
// the native scan path must be skipped for FAT32, exFAT, and other filesystems
// (otherwise every file would fail FSCTL and trigger spurious InlineData reads).
func isNTFSVolume(drive string) bool {
	rootPtr, err := syscall.UTF16PtrFromString(drive + `\`)
	if err != nil {
		return false
	}
	var fsNameBuf [64]uint16
	r1, _, _ := getVolumeInformationW.Call(
		uintptr(unsafe.Pointer(rootPtr)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&fsNameBuf[0])),
		uintptr(len(fsNameBuf)),
	)
	if r1 == 0 {
		return false
	}
	return syscall.UTF16ToString(fsNameBuf[:]) == "NTFS"
}

// nativeScanAvailable returns true when sourcePath refers to a live NTFS
// Windows volume. Accepts both "c:" (raw drive letter from openSource) and
// "\\.\C:" (device path from toRawVolumePath). Returns false for FAT32/exFAT
// volumes and raw image files — those must use the filesystem-specific parsers.
func nativeScanAvailable(sourcePath string) bool {
	var drive string
	// "\\.\C:" — produced by toRawVolumePath
	if strings.HasPrefix(sourcePath, `\\.\`) && len(sourcePath) == 6 && sourcePath[5] == ':' {
		drive = strings.ToUpper(sourcePath[4:6]) // "C:"
	}
	// "c:" or "C:" — raw drive letter as returned by openSource
	if len(sourcePath) == 2 && sourcePath[1] == ':' {
		drive = strings.ToUpper(sourcePath)
	}
	if drive == "" {
		return false
	}
	// Only use native scan for NTFS; FAT32/exFAT don't support
	// FSCTL_GET_RETRIEVAL_POINTERS and would trigger InlineData reads for
	// every file, bloating the manifest.
	return isNTFSVolume(drive)
}

// driveLetter extracts the uppercase drive letter+colon from Windows path forms.
// "\\.\C:" → "C:", "c:" → "C:"
func driveLetter(sourcePath string) string {
	if strings.HasPrefix(sourcePath, `\\.\`) {
		return strings.ToUpper(sourcePath[4:6]) // "C:"
	}
	return strings.ToUpper(sourcePath[:2]) // "C:"
}

// nativeScan enumerates all files on a mounted Windows volume in two
// concurrent phases:
//
//   - The main goroutine walks the directory tree (stat-only) and sends jobs.
//   - A worker pool calls FSCTL_GET_RETRIEVAL_POINTERS in parallel as jobs arrive.
//   - A collector goroutine drains results so workers never block.
//
// Results are applied to the walked slice once the walk completes, keeping
// all writes to walked in a single goroutine (no races).
// onProgress, if non-nil, is called periodically with (scanned, approxTotal).
func nativeScan(ctx context.Context, sourcePath string, onProgress func(scanned, total int), shadowRoot string) ([]manifest.FileEntry, error) {
	drive := driveLetter(sourcePath) // "C:"
	root := drive + `\`              // "C:\"

	// Extent and resident-content mapping goes through go-vss: FileExtents
	// (FSCTL_GET_RETRIEVAL_POINTERS) returns byte-offset extents, and Open
	// reads with backup semantics + DACL bypass and never follows reparse
	// points. The VSS shadow device is addressed as a Snapshot (the normal
	// --capture-files path); a live volume (--no-vss on NTFS) as a Volume.
	src, err := openExtentSource(shadowRoot, drive)
	if err != nil {
		return nil, err
	}
	// Validate NTFS geometry up front (and warm the per-volume cache) so a
	// non-NTFS or inaccessible volume fails loudly rather than yielding
	// silently extent-less files.
	if _, gerr := src.VolumeGeometry(); gerr != nil {
		return nil, fmt.Errorf("reading NTFS volume geometry for %s: %w", drive, gerr)
	}

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8 // beyond ~8 the NTFS driver serialises internally
	}

	type job struct {
		idx  int    // index into walked
		rel  string // volume-relative path (forward slashes) for go-vss
		size int64  // needed to detect resident files (size > 0, no extents)
	}
	type extResult struct {
		idx        int
		extents    []manifest.VolumeExtent
		inlineData []byte // content of resident files (no FSCTL extents)
	}

	jobs := make(chan job, workers*16)
	extResults := make(chan extResult, workers*16)

	// Workers: FSCTL calls only; started immediately so they overlap the walk.
	// After ctx cancellation, workers drain the jobs channel without doing
	// FSCTL work so the walk goroutine can unblock and close(jobs).
	var workerWg sync.WaitGroup
	for range workers {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					continue // drain without work so walk can unblock
				}
				var ext []manifest.VolumeExtent
				var inline []byte
				if exts, ferr := src.FileExtents(j.rel); ferr == nil {
					ext = make([]manifest.VolumeExtent, len(exts))
					for i, e := range exts {
						ext[i] = manifest.VolumeExtent{
							FileOffset:   e.FileOffset,
							VolumeOffset: e.VolumeOffset,
							Length:       e.Length,
						}
					}
					// Empty result on success means a resident (in-MFT) or
					// zero-length file: read the content inline so it restores
					// without raw-volume byte ranges. Only when FileExtents
					// SUCCEEDED — a real error leaves the entry extent-less
					// rather than inlining a whole large file.
					if len(ext) == 0 && j.size > 0 {
						if rf, oerr := src.Open(j.rel); oerr == nil {
							inline, _ = io.ReadAll(rf)
							rf.Close()
						}
					}
				}
				extResults <- extResult{j.idx, ext, inline}
			}
		}()
	}
	go func() { workerWg.Wait(); close(extResults) }()

	// Collector: drains results into a slice so workers never block on the
	// channel. jobCount is updated by the walk so progress shows an
	// approximate total that improves as the walk proceeds.
	var pending []extResult
	var jobCount atomic.Int64
	var scanned atomic.Int64
	var collectorWg sync.WaitGroup
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		for r := range extResults {
			pending = append(pending, r)
			n := scanned.Add(1)
			if onProgress != nil && n%500 == 0 {
				onProgress(int(n), int(jobCount.Load()))
			}
		}
	}()

	// Walk: sequential in main goroutine; feeds jobs channel.
	// Only walked is modified here — no other goroutine touches it.
	var walked []manifest.FileEntry
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err() // stop the walk immediately on cancellation
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = strings.ReplaceAll(rel, `\`, "/")

		if d.IsDir() {
			walked = append(walked, manifest.FileEntry{
				Path:  rel,
				IsDir: true,
				Mode:  uint32(os.ModeDir | 0755),
			})
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		idx := len(walked)
		walked = append(walked, manifest.FileEntry{
			Path:    rel,
			Size:    info.Size(),
			Mode:    uint32(0644),
			ModTime: info.ModTime(),
		})
		switch {
		case isCloudStub(info):
			// Cloud sync placeholder: data is not on the local disk. Skip it
			// entirely so the catalog doesn't contain unrestore-able entries.
			walked = walked[:idx]
		case info.Size() > 0:
			// Use select so a full jobs channel doesn't block us if ctx is
			// cancelled while workers have stopped consuming.
			select {
			case jobs <- job{idx: idx, rel: rel, size: info.Size()}:
				jobCount.Add(1)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	close(jobs) // signals workers to finish

	// Wait for collector (which implicitly waits for workers via close(extResults)).
	collectorWg.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Apply extents (and any inline resident data) to walked. Single goroutine.
	total := int(jobCount.Load())
	for _, r := range pending {
		walked[r.idx].VolumeExtents = r.extents
		if len(r.inlineData) > 0 {
			walked[r.idx].InlineData = r.inlineData
		}
	}
	if onProgress != nil && total > 0 {
		onProgress(total, total)
	}

	return walked, nil
}

// extentSource is the subset of go-vss's Snapshot / Volume that the native
// scan uses to map file data to physical volume extents. Both *govss.Snapshot
// (a VSS shadow device) and *govss.Volume (a live volume) satisfy it.
type extentSource interface {
	FileExtents(rel string) ([]govss.Extent, error)
	Open(rel string) (*os.File, error)
	VolumeGeometry() (govss.VolumeGeometry, error)
}

// openExtentSource returns a go-vss extent source: the VSS shadow device when
// one is available (the normal --capture-files path), otherwise the live
// volume (--no-vss on NTFS).
func openExtentSource(shadowRoot, drive string) (extentSource, error) {
	if shadowRoot != "" {
		return &govss.Snapshot{DeviceObject: shadowRoot}, nil
	}
	v, err := govss.OpenVolume(drive)
	if err != nil {
		return nil, fmt.Errorf("opening volume %s: %w", drive, err)
	}
	return v, nil
}
