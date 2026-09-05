// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package diskplan builds per-partition capture plans for whole-disk machine
// snapshots — on Windows one atomic VSS set spans every disk's eligible
// volumes; everything else captures raw. Shared by the CLI and the local
// service so service-mode disk captures get identical VSS + exclusion
// behavior.
package diskplan

import (
	"fmt"
	"io"
	"regexp"
	"runtime"

	"github.com/SmithOperatingSolutions/disknexus-engine/bmr"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
	"github.com/SmithOperatingSolutions/disknexus-engine/vss"
)

// diskNumberForms match the spellings a physical disk arrives as: the bare
// number the CLI's --disk takes, and the \\.\PhysicalDriveN / \\?\PhysicalDriveN
// device paths panel-configured captures carry (#311 — Windows device names
// are case-insensitive). While only bare numbers parsed here, every
// panel-configured Windows capture fell past the VSS correlation below and
// read the live disk raw.
var (
	bareDiskNumber    = regexp.MustCompile(`^[0-9]+$`)
	physicalDrivePath = regexp.MustCompile(`^\\\\[.?]\\(?i:PhysicalDrive)([0-9]+)$`)
)

// DiskNumberOf extracts the physical disk number from a --disk / device-path
// argument. It is the gate deciding whether BuildDiskMemberPlans correlates
// the disk's volumes for VSS snapshotting.
func DiskNumberOf(arg string) (uint32, bool) {
	num := arg
	if m := physicalDrivePath.FindStringSubmatch(arg); m != nil {
		num = m[1]
	} else if !bareDiskNumber.MatchString(arg) {
		return 0, false
	}
	var n uint32
	fmt.Sscanf(num, "%d", &n)
	return n, true
}

// vssEligible: NTFS-able Windows partitions by either scheme — GPT basic
// data / WinRE GUIDs, or their MBR type-byte counterparts (0x07, 0x27).
func vssEligible(p disklayout.Partition) bool {
	if p.TypeGUID == disklayout.TypeMSBasicData || p.TypeGUID == disklayout.TypeWinRE {
		return true
	}
	return p.MBRType == 0x07 || p.MBRType == 0x27
}

// correlateVolumes maps partition index → snapshot-able volume name by exact
// extent match (a simple volume's single extent starts at the partition
// offset). Only NTFS-able Windows types are candidates; ESP/MSR/unknown stay
// raw. Pure logic — unit-tested without Windows.
func CorrelateVolumes(l *disklayout.DiskLayout, vols []vss.VolumeOnDisk) map[int]string {
	out := map[int]string{}
	for _, p := range l.Partitions {
		if !vssEligible(p) {
			continue
		}
		for _, v := range vols {
			if len(v.Extents) != 1 {
				continue // spanned/striped volumes: not partition-shaped; raw fallback
			}
			e := v.Extents[0]
			if e.StartingOffset == p.Offset(l.SectorSize) && e.Length == p.Length(l.SectorSize) {
				out[p.Index] = v.VolumeName
				break
			}
		}
	}
	return out
}

// buildDiskMemberPlans plans capture for every disk of a machine snapshot.
// On Windows, eligible NTFS volumes across ALL disks are snapshotted in ONE
// atomic VSS set — the whole machine is crash-consistent as of a single
// instant, which matters when data on one disk references another. Everything
// else is captured raw. The returned cleanup releases the set and readers.
func BuildDiskMemberPlans(diskArgs []string, layouts []*disklayout.DiskLayout, cfg config.Config, localPaths ...string) ([][]bmr.MemberPlan, func(), error) {
	plans, _, cleanup, err := BuildDiskMemberPlansWith(diskArgs, layouts, cfg, nil, localPaths...)
	return plans, cleanup, err
}

// BuildDiskMemberPlansWith is BuildDiskMemberPlans with operator exclusions
// (#468): each is resolved against every snapshotted member whose drive
// letter it names, and the outcomes — applied, not found, not NTFS — come
// back per member for the caller to report and record.
func BuildDiskMemberPlansWith(diskArgs []string, layouts []*disklayout.DiskLayout, cfg config.Config, excls []Exclusion, localPaths ...string) ([][]bmr.MemberPlan, []ExclusionOutcome, func(), error) {
	var outcomes []ExclusionOutcome
	plans := make([][]bmr.MemberPlan, len(diskArgs))
	var readers []io.Closer
	var set *vss.Set
	cleanup := func() {
		for _, r := range readers {
			r.Close()
		}
		if set != nil {
			set.Release()
		}
	}

	// Per-disk volume correlation, then one spanning snapshot set.
	byPartPerDisk := make([]map[int]string, len(diskArgs))
	if runtime.GOOS == "windows" {
		var names []string
		for i, diskArg := range diskArgs {
			diskNum, ok := DiskNumberOf(diskArg)
			if !ok {
				continue // image/device path: raw capture
			}
			vols, verr := vss.VolumesOnDisk(diskNum)
			if verr != nil {
				return nil, nil, cleanup, fmt.Errorf("enumerating volumes on disk %d: %w", diskNum, verr)
			}
			byPartPerDisk[i] = CorrelateVolumes(layouts[i], vols)
			for _, v := range byPartPerDisk[i] {
				names = append(names, v)
			}
		}
		if len(names) > 0 {
			var serr error
			set, serr = vss.CreateSnapshotSet(names)
			if serr != nil {
				return nil, nil, cleanup, fmt.Errorf("creating VSS snapshot set: %w", serr)
			}
		}
	}
	devByVol := map[string]string{}
	if set != nil {
		for _, m := range set.Members {
			devByVol[m.Volume] = m.DevicePath
		}
	}

	for i, l := range layouts {
		byPart := byPartPerDisk[i]
		if len(byPart) == 0 {
			plans[i] = bmr.DefaultPlan(l)
			for _, p := range l.Partitions {
				fmt.Printf("  p%d %-16s raw\n", p.Index, p.TypeName)
			}
			continue
		}
		for _, p := range l.Partitions {
			if vol, ok := byPart[p.Index]; ok {
				r, rerr := volume.NewReader(devByVol[vol], cfg.ReadBufferSize)
				if rerr != nil {
					return nil, nil, cleanup, fmt.Errorf("opening shadow device for partition %d: %w", p.Index, rerr)
				}
				readers = append(readers, r)
				// Volatile (pagefile & co) + repo/temp exclusions, scanned
				// from the member's own snapshot device.
				var mr io.Reader = r
				excl := BuildCaptureExclusions(cfg, devByVol[vol], vol, p.Length(l.SectorSize), localPaths...)
				if excl != nil {
					fmt.Printf("  p%d: excluding %d volatile/repo file regions\n", p.Index, excl.Len())
				}
				// Operator exclusions (#468), against this member's own
				// snapshot device; the drive letter picks the member.
				var memberPaths, memberWarnings []string
				if len(excls) > 0 {
					if excl == nil {
						excl = volume.NewExclusionMap()
					}
					memberOut := ApplyExclusions(devByVol[vol], vol, excl, excls)
					for _, o := range memberOut {
						if o.Status != ExclusionNotOnVolume {
							fmt.Printf("  p%d: %s\n", p.Index, o.Describe())
						}
					}
					outcomes = append(outcomes, memberOut...)
					memberPaths, memberWarnings = MemberExclusionRecord(memberOut)
					if excl.Len() == 0 {
						excl = nil
					}
				}
				if excl != nil {
					mr = volume.NewExcludedReader(r, excl)
				}
				plans[i] = append(plans[i], bmr.MemberPlan{Index: p.Index, Kind: disklayout.MemberVolume, Reader: mr,
					ExcludePaths: memberPaths, ExcludeWarnings: memberWarnings})
				fmt.Printf("  p%d %-16s via VSS snapshot\n", p.Index, p.TypeName)
			} else {
				plans[i] = append(plans[i], bmr.MemberPlan{Index: p.Index, Kind: disklayout.MemberRaw})
				fmt.Printf("  p%d %-16s raw\n", p.Index, p.TypeName)
			}
		}
	}
	return plans, outcomes, cleanup, nil
}
