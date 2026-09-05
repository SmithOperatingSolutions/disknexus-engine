// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskplan

import (
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/vss"
)

// #149 slice 2: MBR NTFS (0x07) and WinRE (0x27) partitions are VSS-eligible
// exactly like their GPT counterparts — without this, an MBR system disk
// captures raw (crash-consistent NTFS instead of quiesced).
func TestCorrelateVolumesMBR(t *testing.T) {
	l := &disklayout.DiskLayout{
		Scheme:     "mbr",
		SectorSize: 512,
		Partitions: []disklayout.Partition{
			{Index: 0, MBRType: 0x07, Bootable: true, FirstLBA: 2048, LastLBA: 2048 + 204800 - 1},
			{Index: 1, MBRType: 0x27, FirstLBA: 208896, LastLBA: 208896 + 2048 - 1},
			{Index: 2, MBRType: 0x0C, FirstLBA: 212992, LastLBA: 212992 + 2048 - 1}, // FAT32: raw
		},
	}
	vols := []vss.VolumeOnDisk{
		{VolumeName: `\\?\Volume{c}\`, Extents: []vss.DiskExtent{{StartingOffset: 2048 * 512, Length: 204800 * 512}}},
		{VolumeName: `\\?\Volume{re}\`, Extents: []vss.DiskExtent{{StartingOffset: 208896 * 512, Length: 2048 * 512}}},
		{VolumeName: `\\?\Volume{fat}\`, Extents: []vss.DiskExtent{{StartingOffset: 212992 * 512, Length: 2048 * 512}}},
	}
	got := CorrelateVolumes(l, vols)
	if got[0] != `\\?\Volume{c}\` || got[1] != `\\?\Volume{re}\` {
		t.Fatalf("MBR NTFS/WinRE must correlate: %v", got)
	}
	if _, ok := got[2]; ok {
		t.Fatalf("FAT32 must stay raw: %v", got)
	}
}
