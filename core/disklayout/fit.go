// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
)

// Fitting a captured layout onto a different drive (#223: replace or upgrade
// the disk in the same machine).
//
// RelocateGPT (#76) handles "same size or larger, leave the tail
// unallocated". A drive upgrade needs more: grow into the new space, move a
// trailing recovery partition out of the way, and — the flagship HDD→SSD
// case — SHRINK onto a smaller drive. PlanFit decides all of that and says
// what it decided; ApplyFit rewrites the captured boot structures to match.
// Every partition and disk GUID stays byte-identical, so BCD and fstab
// references survive the upgrade exactly as they survive a same-size restore.
//
// A plan never writes anything. What it refuses, it refuses by name; what it
// cannot know (a filesystem's minimum size, when the caller gave none) it
// warns about and leaves to the caller to confirm with the filesystem tools.

// TargetGeometry describes the drive a layout is being fitted onto.
type TargetGeometry struct {
	Size           int64 // bytes
	LogicalSector  int   // 512 or 4096; 0 = assume the captured layout's
	PhysicalSector int   // 512 or 4096 (512e drives: logical 512, physical 4096); 0 = unknown
}

// FitOptions are the operator's choices.
type FitOptions struct {
	// Grow extends the last data partition into the space a larger target
	// has beyond the captured layout.
	Grow bool
	// MoveRecoveryToEnd lets partitions that FOLLOW the last data partition
	// (a Windows Recovery partition, typically) be moved to the end of the
	// larger target so the data partition can grow; without it, growth is
	// blocked and reported.
	MoveRecoveryToEnd bool
	// Realign moves every partition to a 1 MiB (or physical-sector) boundary.
	// Off, only partitions the plan moves anyway are placed aligned; others
	// keep their captured position and are reported when misaligned.
	Realign bool
	// MinSize, when set, answers "how small can this partition's filesystem
	// go" in bytes (volumefs.MinimumSize). nil means unknown: a shrink is
	// planned to whatever size fits and WARNED as unconfirmed.
	MinSize func(p Partition) (minBytes int64, known bool)
}

// PlannedPartition is one partition's place on the target.
type PlannedPartition struct {
	Index    int    // slot in the entry array, as in Partition.Index
	TypeName string // for messages
	Name     string
	OldFirst uint64
	OldLast  uint64
	NewFirst uint64
	NewLast  uint64
	Shrink   bool
	Grow     bool
	Moved    bool
	MinBytes int64 // filesystem minimum the shrink was planned against; 0 = unknown
}

// OldBytes and NewBytes are the partition's byte sizes before and after.
func (p PlannedPartition) OldBytes(ss int) int64 { return int64(p.OldLast-p.OldFirst+1) * int64(ss) }
func (p PlannedPartition) NewBytes(ss int) int64 { return int64(p.NewLast-p.NewFirst+1) * int64(ss) }

// FitPlan is PlanFit's answer.
type FitPlan struct {
	Scheme           string
	SectorSize       int
	TargetSize       int64
	NewLastUsableLBA uint64
	Partitions       []PlannedPartition // in disk order
	Warnings         []string           // things the operator should know; the plan still applies
	Refusals         []string           // reasons the plan MUST NOT be applied; empty means applicable
}

// Applicable reports whether the plan may be applied.
func (p *FitPlan) Applicable() bool { return len(p.Refusals) == 0 }

// Changed reports whether any partition moves or resizes.
func (p *FitPlan) Changed() bool {
	for _, pp := range p.Partitions {
		if pp.Shrink || pp.Grow || pp.Moved {
			return true
		}
	}
	return false
}

// Partition returns the planned partition for a slot index.
func (p *FitPlan) Partition(index int) (PlannedPartition, bool) {
	for _, pp := range p.Partitions {
		if pp.Index == index {
			return pp, true
		}
	}
	return PlannedPartition{}, false
}

// dataTypes are the partition types whose filesystems the upgrade may shrink
// or grow (NTFS/ext4 live here). Everything else keeps its size: it may move,
// never resize.
var dataTypes = map[string]bool{
	TypeMSBasicData: true,
	TypeLinuxFS:     true,
	TypeLinuxHome:   true,
}

// mbrDataTypes: the MBR equivalents (NTFS/exFAT, Linux).
var mbrDataTypes = map[byte]bool{0x07: true, 0x83: true}

func isData(p Partition, scheme string) bool {
	if scheme == "mbr" {
		return mbrDataTypes[p.MBRType]
	}
	return dataTypes[strings.ToUpper(p.TypeGUID)]
}

func describe(p Partition) string {
	if p.Name != "" {
		return fmt.Sprintf("partition %d (%s, %q)", p.Index, p.TypeName, p.Name)
	}
	return fmt.Sprintf("partition %d (%s)", p.Index, p.TypeName)
}

func alignUp(lba, unit uint64) uint64 {
	if unit == 0 || lba%unit == 0 {
		return lba
	}
	return lba + unit - lba%unit
}

func alignDown(lba, unit uint64) uint64 {
	if unit == 0 {
		return lba
	}
	return lba - lba%unit
}

// PlanFit fits l onto tg. It returns a plan whose Refusals say why it must
// not be applied, or whose Partitions say where everything lands. An error
// is only for inputs that are not a layout at all.
func PlanFit(l *DiskLayout, tg TargetGeometry, opts FitOptions) (*FitPlan, error) {
	if l == nil || l.SectorSize == 0 || len(l.Partitions) == 0 {
		return nil, fmt.Errorf("fit: not a partitioned layout")
	}
	ss := uint64(l.SectorSize)
	plan := &FitPlan{Scheme: l.Scheme, SectorSize: l.SectorSize, TargetSize: tg.Size}
	if plan.Scheme == "" {
		plan.Scheme = "gpt"
	}
	refuse := func(f string, a ...any) { plan.Refusals = append(plan.Refusals, fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { plan.Warnings = append(plan.Warnings, fmt.Sprintf(f, a...)) }

	if tg.LogicalSector != 0 && tg.LogicalSector != l.SectorSize {
		refuse("the target's logical sector size is %d bytes but the captured disk used %d; partition offsets are counted in sectors, so a byte-for-byte restore would not boot — this drive needs a different capture", tg.LogicalSector, l.SectorSize)
	}
	if tg.Size <= 0 || tg.Size%int64(ss) != 0 {
		refuse("target size %d is not a whole number of %d-byte sectors", tg.Size, ss)
	}
	if len(plan.Refusals) > 0 {
		return plan, nil
	}
	sectors := uint64(tg.Size) / ss

	// Where the table structures live on the target.
	var newLastUsable uint64
	if plan.Scheme == "gpt" {
		entryBytes := uint64(l.BackupRegion.Length) - ss // [entries][header]
		entrySectors := (entryBytes + ss - 1) / ss
		if sectors < entrySectors+3 {
			refuse("target (%d sectors) is too small to hold a GPT at all", sectors)
			return plan, nil
		}
		newLastUsable = sectors - 1 - entrySectors - 1
	} else {
		newLastUsable = sectors - 1
	}
	plan.NewLastUsableLBA = newLastUsable

	// Alignment unit: 1 MiB, or the physical sector when larger.
	align := uint64(1<<20) / ss
	if ps := uint64(tg.PhysicalSector); ps > 0 && ps/ss > align {
		align = ps / ss
	}

	parts := append([]Partition(nil), l.Partitions...)
	sort.Slice(parts, func(i, j int) bool { return parts[i].FirstLBA < parts[j].FirstLBA })
	planned := make([]PlannedPartition, len(parts))
	for i, p := range parts {
		planned[i] = PlannedPartition{Index: p.Index, TypeName: p.TypeName, Name: p.Name,
			OldFirst: p.FirstLBA, OldLast: p.LastLBA, NewFirst: p.FirstLBA, NewLast: p.LastLBA}
	}
	sizeOf := func(i int) uint64 { return planned[i].NewLast - planned[i].NewFirst + 1 }
	lastData := -1
	for i, p := range parts {
		if isData(p, plan.Scheme) {
			lastData = i
		}
	}
	// MBR logical partitions (inside the extended container) are never
	// moved or resized: their EBR chain restores verbatim.
	isLogical := func(i int) bool {
		return plan.Scheme == "mbr" && parts[i].Index >= mbrPrimarySlots(l)
	}

	tailEnd := planned[len(planned)-1].OldLast
	switch {
	case newLastUsable >= tailEnd:
		// Fits. Grow when asked.
		if opts.Grow && lastData >= 0 && newLastUsable > tailEnd {
			if isLogical(lastData) {
				warn("%s is an MBR logical partition; growth is not supported for logical partitions, the new space stays unallocated", describe(parts[lastData]))
				break
			}
			trailing := planned[lastData+1:]
			growTo := newLastUsable
			if len(trailing) > 0 {
				if !opts.MoveRecoveryToEnd {
					names := make([]string, 0, len(trailing))
					for i := range trailing {
						names = append(names, describe(parts[lastData+1+i]))
					}
					warn("%s cannot grow: %s follow it; move them to the end of the drive to grow into the new space", describe(parts[lastData]), strings.Join(names, ", "))
					break
				}
				// Pack the trailing partitions at the end, last one last.
				end := newLastUsable
				for i := len(trailing) - 1; i >= 0; i-- {
					k := lastData + 1 + i
					if isLogical(k) {
						warn("%s is an MBR logical partition and cannot be moved; growth is blocked", describe(parts[k]))
						growTo = 0
						break
					}
					size := sizeOf(k)
					first := alignDown(end+1-size, align)
					planned[k].NewFirst, planned[k].NewLast, planned[k].Moved = first, first+size-1, true
					end = first - 1
				}
				if growTo == 0 {
					for i := range trailing {
						k := lastData + 1 + i
						planned[k].NewFirst, planned[k].NewLast, planned[k].Moved = planned[k].OldFirst, planned[k].OldLast, false
					}
					break
				}
				growTo = end
			}
			if growTo > planned[lastData].NewLast {
				planned[lastData].NewLast, planned[lastData].Grow = growTo, true
			}
		}
	default:
		// Does not fit: shrink data partitions from the tail backwards.
		// Partitions after a shrunk one move earlier and land ALIGNED, which
		// can leave the tail past the end again by up to one alignment unit
		// per moved partition — so shrink, re-lay, measure, and shrink the
		// overflow too, until the tail fits or nothing can give.
		minKnown := map[int]uint64{}
		warnedUnknown := map[int]bool{}
		minSectorsOf := func(i int) uint64 {
			if m, ok := minKnown[i]; ok {
				return m
			}
			m := align
			if opts.MinSize != nil {
				if minBytes, known := opts.MinSize(parts[i]); known {
					m = alignUp((uint64(minBytes)+ss-1)/ss, align)
					planned[i].MinBytes = minBytes
					minKnown[i] = m
					return m
				}
			}
			if !warnedUnknown[i] {
				warnedUnknown[i] = true
				warn("the minimum size of %s is unknown; its shrink is planned to what fits and must be confirmed with the filesystem tools before anything is written", describe(parts[i]))
			}
			minKnown[i] = m
			return m
		}
		need := tailEnd - newLastUsable
		for pass := 0; pass < 16 && need > 0; pass++ {
			before := need
			for i := len(parts) - 1; i >= 0 && need > 0; i-- {
				if !isData(parts[i], plan.Scheme) || isLogical(i) {
					continue
				}
				minSectors := minSectorsOf(i)
				allowance := uint64(0)
				if sizeOf(i) > minSectors {
					allowance = sizeOf(i) - minSectors
				}
				take := need
				if take > allowance {
					take = allowance
				}
				if take == 0 {
					continue
				}
				planned[i].NewLast -= take
				planned[i].Shrink = true
				need -= take
			}
			if need > 0 && need == before {
				break // nothing can give
			}
			relayout(planned, parts, align, isLogical, plan)
			if end := planned[len(planned)-1].NewLast; end > newLastUsable {
				// The partitions behind a shrink start on alignment
				// boundaries, so an overrun smaller than one unit cannot be
				// absorbed by shrinking that little: take a whole unit.
				need = alignUp(end-newLastUsable, align)
			} else {
				need = 0
			}
		}
		if need > 0 {
			refuse("the target is %d MiB too small even after shrinking every NTFS/ext4 partition to its minimum; a partition that cannot shrink (system, recovery, swap, or an MBR logical partition) or data that does not fit is in the way", (need*ss+(1<<20)-1)/(1<<20))
		}
	}

	// Realign: every partition to the alignment unit, in order, when asked.
	if opts.Realign && plan.Applicable() {
		pos := alignUp(l.FirstUsableLBA, align)
		for i := range planned {
			if isLogical(i) {
				continue
			}
			size := sizeOf(i)
			first := alignUp(pos, align)
			if first != planned[i].NewFirst {
				planned[i].NewFirst, planned[i].NewLast, planned[i].Moved = first, first+size-1, true
			}
			pos = planned[i].NewLast + 1
		}
		if last := planned[len(planned)-1]; last.NewLast > newLastUsable {
			// Realignment pushed the tail off the disk: drop it and say so.
			for i := range planned {
				if planned[i].Moved && !planned[i].Shrink {
					planned[i].NewFirst, planned[i].NewLast, planned[i].Moved = planned[i].OldFirst, planned[i].OldLast, false
				}
			}
			warn("realigning every partition needs %d more sectors than the target has; partitions keep their captured alignment", last.NewLast-newLastUsable)
		}
	}

	// Report misalignment on whatever stays where it was.
	for i, p := range planned {
		if p.NewFirst%align != 0 {
			warn("%s starts at sector %d, not on a %d KiB boundary; on an SSD this costs write performance (realign to fix)", describe(parts[i]), p.NewFirst, align*ss/1024)
		}
	}
	// Final overlap and bounds check — the plan's own invariant.
	for i := range planned {
		if planned[i].NewLast < planned[i].NewFirst {
			refuse("%s would have no sectors", describe(parts[i]))
		}
		if i > 0 && planned[i].NewFirst <= planned[i-1].NewLast {
			refuse("%s would overlap %s", describe(parts[i]), describe(parts[i-1]))
		}
		if planned[i].NewLast > newLastUsable {
			refuse("%s would end past the target's last usable sector", describe(parts[i]))
		}
	}
	plan.Partitions = planned
	return plan, nil
}

// relayout places partitions in disk order after a shrink: a partition
// keeps its position until something before it shrank; from there on each
// one follows the previous, aligned.
func relayout(planned []PlannedPartition, parts []Partition, align uint64, isLogical func(int) bool, plan *FitPlan) {
	shifted := false
	for i := range planned {
		size := planned[i].NewLast - planned[i].NewFirst + 1
		if shifted {
			if isLogical(i) {
				plan.Refusals = append(plan.Refusals, fmt.Sprintf("%s is an MBR logical partition and would have to move; logical partitions restore verbatim only", describe(parts[i])))
				continue
			}
			first := alignUp(planned[i-1].NewLast+1, align)
			if first != planned[i].NewFirst {
				planned[i].NewFirst, planned[i].NewLast, planned[i].Moved = first, first+size-1, true
			}
		}
		if planned[i].Shrink {
			shifted = true
		}
	}
}

// mbrPrimarySlots is how many primaries an MBR layout lists before its
// logical partitions (parseMBR numbers primaries first, in slot order).
func mbrPrimarySlots(l *DiskLayout) int {
	n := 0
	for _, p := range l.Partitions {
		if len(l.AuxRegions) == 0 || p.FirstLBA < auxStart(l) {
			n++
		}
	}
	return n
}

func auxStart(l *DiskLayout) uint64 {
	var first int64 = -1
	for _, r := range l.AuxRegions {
		if first < 0 || r.Offset < first {
			first = r.Offset
		}
	}
	if first < 0 {
		return ^uint64(0)
	}
	return uint64(first) / uint64(l.SectorSize)
}

// ApplyFit rewrites the captured boot structures for a plan: entries carry
// their new extents, the GPT headers their new last-usable and alternate
// LBAs, every CRC is recomputed, and every GUID is untouched. It returns the
// primary region for LBA0 onward and, for GPT, the backup structures and
// where they go. A plan with refusals is refused.
func ApplyFit(l *DiskLayout, plan *FitPlan, capturedPrimary, capturedBackup []byte) (primary []byte, backupOffset int64, backup []byte, err error) {
	if !plan.Applicable() {
		return nil, 0, nil, fmt.Errorf("fit plan has refusals: %s", strings.Join(plan.Refusals, "; "))
	}
	if plan.Scheme == "mbr" {
		return applyFitMBR(l, plan, capturedPrimary)
	}
	ss := int64(l.SectorSize)
	if int64(len(capturedPrimary)) != l.PrimaryRegion.Length || int64(len(capturedBackup)) != l.BackupRegion.Length {
		return nil, 0, nil, fmt.Errorf("captured region sizes do not match the layout")
	}
	hdrOff := ss
	if int64(len(capturedPrimary)) < hdrOff+int64(gptHeaderSize) {
		return nil, 0, nil, fmt.Errorf("primary region too short for a GPT header")
	}
	primary = append([]byte(nil), capturedPrimary...)
	pHdr := primary[hdrOff:]
	headerSize := binary.LittleEndian.Uint32(pHdr[12:16])
	entryLBA := binary.LittleEndian.Uint64(pHdr[72:80])
	entryCount := binary.LittleEndian.Uint32(pHdr[80:84])
	entrySize := binary.LittleEndian.Uint32(pHdr[84:88])
	entryBytes := int64(entryCount) * int64(entrySize)
	entryOff := int64(entryLBA) * ss
	if entryOff+entryBytes > int64(len(primary)) {
		return nil, 0, nil, fmt.Errorf("primary region does not contain the entry array")
	}
	entries := primary[entryOff : entryOff+entryBytes]
	for _, pp := range plan.Partitions {
		if pp.Index < 0 || pp.Index >= int(entryCount) {
			return nil, 0, nil, fmt.Errorf("plan names slot %d beyond the %d-entry array", pp.Index, entryCount)
		}
		e := entries[pp.Index*int(entrySize) : (pp.Index+1)*int(entrySize)]
		binary.LittleEndian.PutUint64(e[32:40], pp.NewFirst)
		binary.LittleEndian.PutUint64(e[40:48], pp.NewLast)
	}
	entriesCRC := crc32.ChecksumIEEE(entries)
	entrySectors := (entryBytes + ss - 1) / ss
	newLastLBA := plan.TargetSize/ss - 1
	newBackupEntryLBA := newLastLBA - entrySectors
	hdr := pHdr[:headerSize]
	binary.LittleEndian.PutUint64(hdr[32:40], uint64(newLastLBA))
	binary.LittleEndian.PutUint64(hdr[48:56], plan.NewLastUsableLBA)
	binary.LittleEndian.PutUint32(hdr[88:92], entriesCRC)
	recomputeHeaderCRC(hdr)

	backup = make([]byte, entryBytes+ss)
	copy(backup, entries)
	bHdr := backup[entryBytes : entryBytes+int64(headerSize)]
	copy(bHdr, hdr)
	binary.LittleEndian.PutUint64(bHdr[24:32], uint64(newLastLBA))
	binary.LittleEndian.PutUint64(bHdr[32:40], 1)
	binary.LittleEndian.PutUint64(bHdr[72:80], uint64(newBackupEntryLBA))
	recomputeHeaderCRC(bHdr)
	return primary, newBackupEntryLBA * ss, backup, nil
}

// applyFitMBR rewrites the primary partition entries in the captured boot
// sector. Logical partitions are never in a plan that reaches here (PlanFit
// refuses moving them), so the EBR chain restores verbatim.
func applyFitMBR(l *DiskLayout, plan *FitPlan, capturedPrimary []byte) ([]byte, int64, []byte, error) {
	if len(capturedPrimary) < 512 {
		return nil, 0, nil, fmt.Errorf("captured MBR region is shorter than a sector")
	}
	primary := append([]byte(nil), capturedPrimary...)
	// Map parse index → on-disk slot: parseMBR numbers non-empty,
	// non-extended slots in order.
	slotOf := map[int]int{}
	k := 0
	for slot := 0; slot < 4; slot++ {
		e := primary[mbrEntryBase+slot*16 : mbrEntryBase+slot*16+16]
		typ := e[4]
		sectors := binary.LittleEndian.Uint32(e[12:16])
		if typ == mbrTypeEmpty || sectors == 0 || isExtended(typ) {
			continue
		}
		slotOf[k] = slot
		k++
	}
	for _, pp := range plan.Partitions {
		slot, ok := slotOf[pp.Index]
		if !ok {
			if pp.Moved || pp.Shrink || pp.Grow {
				return nil, 0, nil, fmt.Errorf("plan changes MBR partition %d, which is not a primary", pp.Index)
			}
			continue
		}
		if pp.NewFirst > 0xFFFFFFFF || pp.NewLast-pp.NewFirst+1 > 0xFFFFFFFF {
			return nil, 0, nil, fmt.Errorf("MBR partition %d would not fit a 32-bit LBA", pp.Index)
		}
		e := primary[mbrEntryBase+slot*16 : mbrEntryBase+slot*16+16]
		binary.LittleEndian.PutUint32(e[8:12], uint32(pp.NewFirst))
		binary.LittleEndian.PutUint32(e[12:16], uint32(pp.NewLast-pp.NewFirst+1))
	}
	return primary, 0, nil, nil
}
