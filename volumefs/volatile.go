// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"fmt"
	"math"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	parser "www.velocidex.com/golang/go-ntfs/parser"
)

// volatileFiles are user-visible files that change every boot.
var volatileFiles = []string{
	"pagefile.sys",
	"hiberfil.sys",
	"swapfile.sys",
}

// BuildVolatileExclusionMap scans an NTFS volume for volatile files and
// returns an ExclusionMap marking their physical byte ranges. For non-NTFS
// filesystems, an empty map is returned.
func BuildVolatileExclusionMap(sourcePath string, volumeSize int64) (*volume.ExclusionMap, error) {
	f, err := os.Open(toRawVolumePath(sourcePath))
	if err != nil {
		return nil, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()

	fsType, partOffset, err := detectFilesystem(f)
	if err != nil {
		return nil, fmt.Errorf("detecting filesystem: %w", err)
	}

	if fsType != "ntfs" {
		return volume.NewExclusionMap(), nil
	}

	reader, err := parser.NewPagedReader(f, 1024, 10000)
	if err != nil {
		return nil, fmt.Errorf("creating paged reader: %w", err)
	}

	ntfs, err := parser.GetNTFSContext(reader, partOffset)
	if err != nil {
		return nil, fmt.Errorf("parsing NTFS: %w", err)
	}
	defer ntfs.Close()

	root, err := ntfs.GetMFT(5)
	if err != nil {
		return nil, fmt.Errorf("getting root MFT entry: %w", err)
	}

	exclMap := volume.NewExclusionMap()

	// Exclude user-visible volatile files in the root directory.
	for _, name := range volatileFiles {
		entry, err := root.Open(ntfs, name)
		if err != nil {
			continue // file doesn't exist on this volume
		}
		addStreamRanges(ntfs, entry, "", partOffset, exclMap)
	}

	// $LogFile is MFT entry #2.
	logEntry, err := ntfs.GetMFT(2)
	if err == nil {
		addStreamRanges(ntfs, logEntry, "", partOffset, exclMap)
	}

	// $UsnJrnl:$J is the change journal data stream.
	usnEntry, err := root.Open(ntfs, "$Extend/$UsnJrnl")
	if err == nil {
		addStreamRanges(ntfs, usnEntry, "$J", partOffset, exclMap)
	}

	return exclMap, nil
}

// addStreamRanges opens a data stream on the MFT entry and excludes all of
// its non-sparse physical device ranges. streamName is the alternate data
// stream name ("" for the default $DATA stream). The ranges must be physical
// device offsets, not go-ntfs's file-logical run offsets — otherwise the map
// would zero the wrong regions of the volume (e.g. the boot sector and $MFT
// instead of the pagefile clusters).
func addStreamRanges(ntfs *parser.NTFSContext, entry *parser.MFT_ENTRY, streamName string, partOffset int64, m *volume.ExclusionMap) {
	// Pass a large size cap so full clusters of the volatile stream are
	// excluded (including any trailing slack).
	for _, ext := range physicalExtents(ntfs, entry, streamName, partOffset, math.MaxInt64) {
		if ext.Length > 0 {
			m.AddRange(ext.VolumeOffset, ext.Length)
		}
	}
}
