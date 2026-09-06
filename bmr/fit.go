// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package bmr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

// Restoring and cloning into a FIT PLAN (#223 E3).
//
// A same-size-or-larger restore writes every member where it was captured
// (RestoreDisk). A drive upgrade writes members where PlanFit put them: a
// grown partition gets its old bytes and a zeroed tail, a moved partition
// its old bytes at the new place, and a SHRUNK partition the bytes the
// caller hands back after shrinking the filesystem in staging (the engine
// cannot shrink a filesystem in place; the recovery ISO's tools do, and
// RestoreMemberTo is how the caller gets the full-length bytes to stage).

// StagedMember is a shrunk member's bytes, ready to be placed: the caller
// restored the member full-length somewhere (RestoreMemberTo), shrank the
// filesystem there, and hands back the front Length bytes.
type StagedMember struct {
	Reader io.ReaderAt
	Length int64
}

// fitOf resolves where a member lands: base offset and length on the
// target, and what kind of placement it is.
type placement struct {
	base, length int64 // on the target
	oldLength    int64 // captured length
	shrink, grow bool
}

func placementFor(l *disklayout.DiskLayout, fit *disklayout.FitPlan, index int) (placement, error) {
	ss := int64(l.SectorSize)
	var old disklayout.Partition
	found := false
	for _, p := range l.Partitions {
		if p.Index == index {
			old, found = p, true
		}
	}
	if !found {
		return placement{}, fmt.Errorf("partition %d is not in the captured layout", index)
	}
	pl := placement{base: old.Offset(l.SectorSize), length: old.Length(l.SectorSize), oldLength: old.Length(l.SectorSize)}
	if fit == nil {
		return pl, nil
	}
	pp, ok := fit.Partition(index)
	if !ok {
		return placement{}, fmt.Errorf("fit plan has no entry for partition %d", index)
	}
	pl.base = int64(pp.NewFirst) * ss
	pl.length = pp.NewBytes(l.SectorSize)
	pl.shrink, pl.grow = pp.Shrink, pp.Grow
	return pl, nil
}

// bootStructures is the plan's boot structures, or the plain relocation.
func bootStructures(d *disklayout.DiskCapture, fit *disklayout.FitPlan, targetSize int64) (primary []byte, backupOff int64, backup []byte, err error) {
	if fit == nil {
		return disklayout.RelocateGPT(&d.Layout, d.PrimaryGPT, d.BackupGPT, targetSize)
	}
	if !fit.Applicable() {
		return nil, 0, nil, fmt.Errorf("fit plan is not applicable: %v", fit.Refusals)
	}
	if fit.TargetSize != targetSize {
		return nil, 0, nil, fmt.Errorf("fit plan was made for a %d-byte target, not %d", fit.TargetSize, targetSize)
	}
	return disklayout.ApplyFit(&d.Layout, fit, d.PrimaryGPT, d.BackupGPT)
}

// writeStaged copies a staged member's bytes into place and returns the
// SHA-256 of what was written.
func writeStaged(w io.WriterAt, st StagedMember, length int64) ([32]byte, error) {
	if st.Reader == nil || st.Length != length {
		return [32]byte{}, fmt.Errorf("staged member holds %d bytes, the planned partition is %d", st.Length, length)
	}
	return copyAt(w, io.NewSectionReader(st.Reader, 0, length), length)
}

func copyAt(w io.WriterAt, r io.Reader, length int64) ([32]byte, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var off int64
	for off < length {
		n := int64(len(buf))
		if length-off < n {
			n = length - off
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return [32]byte{}, fmt.Errorf("reading at %d: %w", off, err)
		}
		if _, err := w.WriteAt(buf[:n], off); err != nil {
			return [32]byte{}, fmt.Errorf("writing at %d: %w", off, err)
		}
		h.Write(buf[:n])
		off += n
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func hashAt(r io.ReaderAt, off, length int64) ([32]byte, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(r, off, length)); err != nil {
		return [32]byte{}, err
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// RestoreMemberTo restores one member of a machine snapshot's disk,
// full-length, at offset 0 of target — the staging step of a shrink. The
// digest read-back, when ReadAt is set, verifies the staged bytes.
func RestoreMemberTo(ctx context.Context, opts RestoreDiskOptions, partitionIndex int, target Target, readAt io.ReaderAt) error {
	m, err := disklayout.LoadMachineManifest(opts.RepoPath, opts.SnapshotID)
	if err != nil {
		return fmt.Errorf("loading machine manifest: %w", err)
	}
	if opts.DiskIndex < 0 || opts.DiskIndex >= len(m.Disks) {
		return fmt.Errorf("machine snapshot has %d disks; index %d out of range", len(m.Disks), opts.DiskIndex)
	}
	d := m.Disks[opts.DiskIndex]
	for _, mem := range d.Members {
		if mem.Index != partitionIndex {
			continue
		}
		// Volume and raw members both hold the partition's bytes (raw is how
		// a non-snapshotted member is captured); only a skipped member has
		// nothing to stage.
		if mem.Kind == disklayout.MemberSkipped || mem.BackupID == "" {
			return fmt.Errorf("partition %d was skipped at capture (%s); there are no bytes to stage", partitionIndex, mem.Reason)
		}
		pl, err := placementFor(&d.Layout, nil, mem.Index)
		if err != nil {
			return err
		}
		pt := &partitionTarget{t: target, base: 0, length: pl.oldLength}
		return restoreMember(ctx, opts, &d, mem, pt, pl.oldLength, readAt, 0)
	}
	return fmt.Errorf("partition %d has no member in the snapshot", partitionIndex)
}

// restoreMember restores one volume member into pt (a partition-sized
// window) and read-back-verifies it at readAtBase when readAt is set.
func restoreMember(ctx context.Context, opts RestoreDiskOptions, d *disklayout.DiskCapture, mem disklayout.PartitionMember, pt *partitionTarget, length int64, readAt io.ReaderAt, readAtBase int64) error {
	backup, err := manifest.LoadMetadata(opts.RepoPath, mem.BackupID)
	if err != nil {
		return fmt.Errorf("loading member backup %s (partition %d): %w", mem.BackupID, mem.Index, err)
	}
	if backup.TotalBytes != length {
		return fmt.Errorf("partition %d: member backup covers %d bytes but the partition is %d", mem.Index, backup.TotalBytes, length)
	}
	entries, entriesCloser, err := manifest.NewEntryAccessor(opts.RepoPath, mem.BackupID)
	if err != nil {
		return fmt.Errorf("opening entries of member backup %s (partition %d): %w", mem.BackupID, mem.Index, err)
	}
	if opts.OnMemberEntries != nil {
		opts.OnMemberEntries(entries)
	}
	r := restore.NewRestorer(opts.Index, opts.ChunkStore, opts.Logger)
	r.SetNormalizer(opts.Normalizer)
	if opts.OnProgress != nil {
		memIdx, memCount := restoreMemberOrdinal(d.Members, mem.Index)
		r.OnProgress = func(done, total int64) { opts.OnProgress(memIdx, memCount, done, total) }
	}
	_, rerr := r.RestoreEntries(ctx, backup, entries, pt)
	if entriesCloser != nil {
		entriesCloser.Close()
	}
	if rerr != nil {
		return fmt.Errorf("restoring partition %d from %s: %w", mem.Index, mem.BackupID, rerr)
	}
	if readAt == nil {
		return nil
	}
	verdict, derr := restore.VerifyWrittenDigest(backup, io.NewSectionReader(readAt, readAtBase, length))
	switch {
	case derr != nil:
		opts.Logger.Warn("restored partition could not be read back for digest verification", "partition", mem.Index, "backup", mem.BackupID, "err", derr)
	case verdict == restore.DigestMismatch:
		return fmt.Errorf("partition %d: restored bytes do not match member backup %s's capture digest — the partition holds data the capture never produced; do not trust this restore", mem.Index, mem.BackupID)
	case verdict == restore.DigestNotVerifiable:
		opts.Logger.Info("pre-digest member backup; restored partition cannot be digest-verified", "partition", mem.Index, "backup", mem.BackupID)
	}
	return nil
}

// RestoreDiskFit restores a machine snapshot's disk into a fit plan: every
// member at its planned place, grown members zero-filled past their
// captured length, shrunk members from opts.Staged. With a nil plan it is
// RestoreDisk.
func RestoreDiskFit(ctx context.Context, opts RestoreDiskOptions, fit *disklayout.FitPlan, staged map[int]StagedMember) error {
	m, err := disklayout.LoadMachineManifest(opts.RepoPath, opts.SnapshotID)
	if err != nil {
		return fmt.Errorf("loading machine manifest: %w", err)
	}
	if opts.DiskIndex < 0 || opts.DiskIndex >= len(m.Disks) {
		return fmt.Errorf("machine snapshot has %d disks; index %d out of range", len(m.Disks), opts.DiskIndex)
	}
	d := m.Disks[opts.DiskIndex]
	if fit == nil && opts.TargetSize < d.Layout.DiskSize {
		return fmt.Errorf("target (%d bytes) is smaller than the captured disk (%d bytes); plan a fit to shrink", opts.TargetSize, d.Layout.DiskSize)
	}
	// Members first, boot structures last: a target that dies mid-way has
	// no valid table pointing at half-written partitions.
	for _, mem := range d.Members {
		pl, err := placementFor(&d.Layout, fit, mem.Index)
		if err != nil {
			return err
		}
		pt := &partitionTarget{t: opts.Target, base: pl.base, length: pl.length}
		switch {
		case mem.Kind == disklayout.MemberSkipped:
			if err := writeZeros(pt, pl.length); err != nil {
				return fmt.Errorf("zeroing skipped partition %d: %w", mem.Index, err)
			}
		case pl.shrink:
			st, ok := staged[mem.Index]
			if !ok {
				return fmt.Errorf("partition %d is planned to shrink to %d bytes but no staged (shrunk) bytes were supplied for it", mem.Index, pl.length)
			}
			if _, err := writeStaged(pt, st, pl.length); err != nil {
				return fmt.Errorf("placing shrunk partition %d: %w", mem.Index, err)
			}
			if opts.Logger != nil {
				opts.Logger.Info("shrunk partition placed from staging; its capture digest no longer applies — the filesystem check is its verification", "partition", mem.Index)
			}
		default:
			old := &partitionTarget{t: opts.Target, base: pl.base, length: pl.oldLength}
			if err := restoreMember(ctx, opts, &d, mem, old, pl.oldLength, opts.ReadAt, pl.base); err != nil {
				return err
			}
			if pl.grow && pl.length > pl.oldLength {
				tail := &partitionTarget{t: opts.Target, base: pl.base + pl.oldLength, length: pl.length - pl.oldLength}
				if err := writeZeros(tail, pl.length-pl.oldLength); err != nil {
					return fmt.Errorf("zeroing the grown tail of partition %d: %w", mem.Index, err)
				}
			}
		}
	}
	primary, backupOff, backupBytes, err := bootStructures(&d, fit, opts.TargetSize)
	if err != nil {
		return err
	}
	if _, err := opts.Target.WriteAt(primary, d.Layout.PrimaryRegion.Offset); err != nil {
		return fmt.Errorf("writing primary boot region: %w", err)
	}
	if len(backupBytes) > 0 {
		if _, err := opts.Target.WriteAt(backupBytes, backupOff); err != nil {
			return fmt.Errorf("writing backup GPT region: %w", err)
		}
	}
	for ai, ar := range d.Layout.AuxRegions {
		if _, err := opts.Target.WriteAt(d.AuxBytes[ai], ar.Offset); err != nil {
			return fmt.Errorf("writing aux region %d: %w", ai, err)
		}
	}
	return opts.Target.Sync()
}

// CloneDiskOptions parameterizes a direct drive-to-drive clone: no repo,
// the source read live (the caller guarantees it is not the running
// system — the recovery ISO is not booted from it).
type CloneDiskOptions struct {
	Source     io.ReaderAt
	SourceSize int64
	Target     Target
	TargetSize int64
	// Fit is the plan for a target of another size; nil = same-size or
	// larger relocation with the tail left unallocated.
	Fit    *disklayout.FitPlan
	Staged map[int]StagedMember
	// ReadAt, when set, reads the target back after every partition and
	// compares it to what was written — the only verification a clone has.
	ReadAt     io.ReaderAt
	OnProgress func(partition int, done, total int64)
}

// CloneResult reports what a clone wrote.
type CloneResult struct {
	Layout     *disklayout.DiskLayout
	Partitions int
	Bytes      int64
	Digests    map[int]string // partition index → SHA-256 of the bytes written
}

// CloneDisk copies a partitioned disk to another, partition by partition,
// through the same placement rules a fit restore uses.
func CloneDisk(ctx context.Context, opts CloneDiskOptions) (*CloneResult, error) {
	l, err := disklayout.Parse(opts.Source, opts.SourceSize)
	if err != nil {
		return nil, fmt.Errorf("parsing the source disk: %w", err)
	}
	if opts.Fit == nil && opts.TargetSize < l.DiskSize {
		return nil, fmt.Errorf("target (%d bytes) is smaller than the source (%d bytes); plan a fit to shrink", opts.TargetSize, l.DiskSize)
	}
	d := disklayout.DiskCapture{Layout: *l}
	readRegion := func(r disklayout.Range) ([]byte, error) {
		buf := make([]byte, r.Length)
		if _, err := opts.Source.ReadAt(buf, r.Offset); err != nil && err != io.EOF {
			return nil, err
		}
		return buf, nil
	}
	if d.PrimaryGPT, err = readRegion(l.PrimaryRegion); err != nil {
		return nil, fmt.Errorf("reading the source's primary boot region: %w", err)
	}
	if l.BackupRegion.Length > 0 {
		if d.BackupGPT, err = readRegion(l.BackupRegion); err != nil {
			return nil, fmt.Errorf("reading the source's backup GPT region: %w", err)
		}
	}
	for _, ar := range l.AuxRegions {
		b, err := readRegion(ar)
		if err != nil {
			return nil, fmt.Errorf("reading the source's aux region at %d: %w", ar.Offset, err)
		}
		d.AuxBytes = append(d.AuxBytes, b)
	}
	res := &CloneResult{Layout: l, Digests: map[int]string{}}
	for _, p := range l.Partitions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pl, err := placementFor(l, opts.Fit, p.Index)
		if err != nil {
			return nil, err
		}
		pt := &partitionTarget{t: opts.Target, base: pl.base, length: pl.length}
		var sum [32]byte
		switch {
		case pl.shrink:
			st, ok := opts.Staged[p.Index]
			if !ok {
				return nil, fmt.Errorf("partition %d is planned to shrink to %d bytes but no staged (shrunk) bytes were supplied for it", p.Index, pl.length)
			}
			if sum, err = writeStaged(pt, st, pl.length); err != nil {
				return nil, fmt.Errorf("placing shrunk partition %d: %w", p.Index, err)
			}
		default:
			src := io.NewSectionReader(opts.Source, p.Offset(l.SectorSize), pl.oldLength)
			if sum, err = copyAt(pt, src, pl.oldLength); err != nil {
				return nil, fmt.Errorf("copying partition %d: %w", p.Index, err)
			}
			if pl.grow && pl.length > pl.oldLength {
				tail := &partitionTarget{t: opts.Target, base: pl.base + pl.oldLength, length: pl.length - pl.oldLength}
				if err := writeZeros(tail, pl.length-pl.oldLength); err != nil {
					return nil, fmt.Errorf("zeroing the grown tail of partition %d: %w", p.Index, err)
				}
			}
		}
		written := pl.length
		if !pl.shrink && !pl.grow {
			written = pl.oldLength
		}
		if pl.grow {
			written = pl.oldLength
		}
		if opts.ReadAt != nil {
			back, err := hashAt(opts.ReadAt, pl.base, written)
			if err != nil {
				return nil, fmt.Errorf("reading partition %d back: %w", p.Index, err)
			}
			if back != sum {
				return nil, fmt.Errorf("partition %d: the target reads back differently from what was written — the clone is not trustworthy; keep the source drive", p.Index)
			}
		}
		res.Digests[p.Index] = hex.EncodeToString(sum[:])
		res.Partitions++
		res.Bytes += written
		if opts.OnProgress != nil {
			opts.OnProgress(p.Index, res.Bytes, l.DiskSize)
		}
	}
	// The unallocated gaps. A same-size clone carries them verbatim — a
	// clone is the whole disk, and some boot loaders keep code in the gap
	// after the table. Under a plan the gaps are new space and are zeroed,
	// so nothing stale from the target's past can be mistaken for data.
	if err := writeGaps(opts, l, res); err != nil {
		return nil, err
	}
	primary, backupOff, backupBytes, err := bootStructures(&d, opts.Fit, opts.TargetSize)
	if err != nil {
		return nil, err
	}
	if _, err := opts.Target.WriteAt(primary, l.PrimaryRegion.Offset); err != nil {
		return nil, fmt.Errorf("writing primary boot region: %w", err)
	}
	if len(backupBytes) > 0 {
		if _, err := opts.Target.WriteAt(backupBytes, backupOff); err != nil {
			return nil, fmt.Errorf("writing backup GPT region: %w", err)
		}
	}
	for ai, ar := range l.AuxRegions {
		if _, err := opts.Target.WriteAt(d.AuxBytes[ai], ar.Offset); err != nil {
			return nil, fmt.Errorf("writing aux region %d: %w", ai, err)
		}
	}
	if err := opts.Target.Sync(); err != nil {
		return nil, err
	}
	return res, nil
}

// writeGaps fills every byte of the target that is neither a partition nor
// a boot/aux region: from the source when the geometry is unchanged, with
// zeros when a plan moved things.
func writeGaps(opts CloneDiskOptions, l *disklayout.DiskLayout, res *CloneResult) error {
	type span struct{ off, end int64 }
	ss := int64(l.SectorSize)
	var used []span
	used = append(used, span{l.PrimaryRegion.Offset, l.PrimaryRegion.Offset + l.PrimaryRegion.Length})
	for _, ar := range l.AuxRegions {
		used = append(used, span{ar.Offset, ar.Offset + ar.Length})
	}
	for _, p := range l.Partitions {
		pl, err := placementFor(l, opts.Fit, p.Index)
		if err != nil {
			return err
		}
		used = append(used, span{pl.base, pl.base + pl.length})
	}
	// Where the backup structures will land on the target.
	if l.BackupRegion.Length > 0 {
		entryBytes := l.BackupRegion.Length - ss
		backupOff := opts.TargetSize - ss - entryBytes
		if opts.Fit == nil && opts.TargetSize == l.DiskSize {
			backupOff = l.BackupRegion.Offset
		}
		used = append(used, span{backupOff, opts.TargetSize})
	}
	// Sort and walk the holes.
	for i := 1; i < len(used); i++ {
		for j := i; j > 0 && used[j].off < used[j-1].off; j-- {
			used[j], used[j-1] = used[j-1], used[j]
		}
	}
	cursor := int64(0)
	verbatim := opts.Fit == nil && opts.TargetSize == l.DiskSize
	for _, u := range used {
		if u.off > cursor {
			if err := fillGap(opts, cursor, u.off-cursor, verbatim); err != nil {
				return err
			}
			res.Bytes += u.off - cursor
		}
		if u.end > cursor {
			cursor = u.end
		}
	}
	if cursor < opts.TargetSize {
		if err := fillGap(opts, cursor, opts.TargetSize-cursor, verbatim); err != nil {
			return err
		}
	}
	return nil
}

func fillGap(opts CloneDiskOptions, off, length int64, verbatim bool) error {
	w := &partitionTarget{t: opts.Target, base: off, length: length}
	if verbatim && off+length <= opts.SourceSize {
		_, err := copyAt(w, io.NewSectionReader(opts.Source, off, length), length)
		return err
	}
	return writeZeros(w, length)
}

// BootReport is what CheckBootStructures found on a target: enough to tell
// an operator "this should boot" or "this has no boot loader" before the
// old drive is touched. It reports; it never refuses.
type BootReport struct {
	Scheme          string
	Partitions      int
	BackupHeaderOK  bool     // GPT: the backup header at the disk end parses and matches
	ESPIndex        int      // GPT: the EFI System Partition's index, -1 if none
	BIOSBootable    bool     // MBR: a partition carries the active flag; GPT: a BIOS boot partition exists
	BootFiles       []string // files found on the ESP that a firmware or Windows boot needs
	WindowsBoot     bool     // EFI\Microsoft\Boot\BCD present on the ESP
	FallbackLoader  bool     // EFI\BOOT\BOOTX64.EFI present on the ESP
	LinuxBootMarker bool     // an ext4 partition holding /boot or /vmlinuz*
	Notes           []string
}

// CheckBootStructures parses a restored or cloned target and looks for
// what a boot needs. path is the device or image; size its length.
func CheckBootStructures(ctx context.Context, path string, size int64) (*BootReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the target: %w", err)
	}
	defer f.Close()
	var r io.ReaderAt = f
	l, err := disklayout.Parse(r, size)
	if err != nil {
		return nil, fmt.Errorf("the target does not carry a partition table: %w", err)
	}
	rep := &BootReport{Scheme: l.Scheme, Partitions: len(l.Partitions), ESPIndex: -1}
	if rep.Scheme == "" {
		rep.Scheme = "gpt"
	}
	if rep.Scheme == "gpt" {
		if err := l.VerifyBackupHeader(r); err != nil {
			rep.Notes = append(rep.Notes, "backup GPT header: "+err.Error())
		} else {
			rep.BackupHeaderOK = true
		}
	}
	for _, p := range l.Partitions {
		if rep.Scheme == "mbr" && p.Bootable {
			rep.BIOSBootable = true
		}
		switch p.TypeGUID {
		case disklayout.TypeBIOSBoot:
			rep.BIOSBootable = true
		case disklayout.TypeESP:
			rep.ESPIndex = p.Index
			// go-diskfs numbers table entries from 1 in slot order.
			files, err := volumefs.ScanFAT32Partition(path, p.Index+1, p.Offset(l.SectorSize))
			if err != nil {
				rep.Notes = append(rep.Notes, fmt.Sprintf("ESP (partition %d) could not be read as FAT: %v", p.Index, err))
				continue
			}
			for _, f := range files {
				name := normalizeBootPath(f.Path)
				switch name {
				case "efi/microsoft/boot/bcd":
					rep.WindowsBoot = true
					rep.BootFiles = append(rep.BootFiles, f.Path)
				case "efi/boot/bootx64.efi", "efi/boot/bootaa64.efi":
					rep.FallbackLoader = true
					rep.BootFiles = append(rep.BootFiles, f.Path)
				}
			}
		case disklayout.TypeLinuxFS:
			scan, err := volumefs.ScanPartition(ctx, r, p.Offset(l.SectorSize), p.Length(l.SectorSize))
			if err != nil {
				continue
			}
			for _, f := range scan.Files {
				name := normalizeBootPath(f.Path)
				if name == "boot" && f.IsDir || len(name) > 8 && name[:8] == "vmlinuz-" || name == "boot/grub" {
					rep.LinuxBootMarker = true
					break
				}
			}
		}
	}
	if rep.Scheme == "gpt" && rep.ESPIndex < 0 && !rep.BIOSBootable {
		rep.Notes = append(rep.Notes, "no EFI System Partition and no BIOS boot partition: firmware has nothing to boot from")
	}
	if rep.ESPIndex >= 0 && !rep.WindowsBoot && !rep.FallbackLoader {
		rep.Notes = append(rep.Notes, "the EFI System Partition holds no Windows BCD and no fallback loader (EFI\\BOOT\\BOOTX64.EFI)")
	}
	return rep, nil
}

func normalizeBootPath(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' {
			c = '/'
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	s := string(out)
	for len(s) > 2 && s[:2] == "./" {
		s = s[2:]
	}
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}
