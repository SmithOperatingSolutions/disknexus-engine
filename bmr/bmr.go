// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package bmr orchestrates bare-metal disk capture and restore (issue #69,
// docs/BARE_METAL_RECOVERY.md): a whole disk becomes a machine snapshot — the
// GPT layout captured verbatim plus one member backup per partition — and a
// restore reassembles a byte-exact disk on a same-size target.
//
// The package is platform-neutral (core imports only): it operates on
// io.ReaderAt / io.WriterAt, so it works identically on disk images (tests,
// Linux) and raw devices (cmd wires volume.Reader/Writer and VSS on Windows).
package bmr

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/disklayout"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/volumefs"
)

// MemberPlan says how one partition will be captured. The cmd layer builds the
// plan (VSS-snapshotting eligible volumes on Windows); the default plan
// captures every partition raw.
type MemberPlan struct {
	Index int // GPT entry index
	Kind  disklayout.MemberKind
	// Reader supplies the partition content for MemberVolume members (e.g. a
	// VSS shadow-device reader). Ignored for MemberRaw (read from the disk
	// itself) and MemberSkipped.
	Reader io.Reader
	// ExcludePaths / ExcludeWarnings (#468): the operator exclusions this
	// member was captured with and the ones that did not apply to it, as
	// the planner resolved them. Stamped onto the member's manifest.
	ExcludePaths    []string
	ExcludeWarnings []string
	// ScanPath (#151): quiesced source for the member's file-catalog scan
	// (the VSS shadow device on Windows). When empty, catalogs scan the
	// disk at the partition offset.
	ScanPath string
	Reason   string // for skipped members
}

// scanMemberCatalog builds the member's file catalog. nil files with nil
// error means "not catalogable" (non-NTFS member) — skipped silently.
func scanMemberCatalog(ctx context.Context, spec DiskSpec, mp MemberPlan, p disklayout.Partition) ([]manifest.FileEntry, error) {
	if mp.Kind == disklayout.MemberSkipped {
		return nil, nil
	}
	if mp.ScanPath != "" {
		res, err := volumefs.ScanVolume(ctx, mp.ScanPath, p.Length(512), nil, mp.ScanPath)
		if err != nil {
			return nil, err
		}
		return res.Files, nil
	}
	res, err := volumefs.ScanPartition(ctx, spec.Disk, p.Offset(512), p.Length(512))
	if err != nil {
		if strings.Contains(err.Error(), "unsupported member filesystem") || strings.Contains(err.Error(), "detecting member filesystem") {
			return nil, nil // ESP/MSR/FAT/etc: no catalog, not an error
		}
		return nil, err
	}
	return res.Files, nil
}

// DefaultPlan captures every partition of the layout as a raw member.
func DefaultPlan(l *disklayout.DiskLayout) []MemberPlan {
	out := make([]MemberPlan, 0, len(l.Partitions))
	for _, p := range l.Partitions {
		out = append(out, MemberPlan{Index: p.Index, Kind: disklayout.MemberRaw})
	}
	return out
}

// DiskSpec describes one disk in a (possibly multi-disk) machine capture.
type DiskSpec struct {
	Source   string // informational: device/image path
	Disk     io.ReaderAt
	DiskSize int64
	Plan     []MemberPlan // defaults to DefaultPlan(layout) when nil
	// CaptureFiles (#151): scan NTFS members into per-member catalogs.
	CaptureFiles bool
}

// CaptureOptions configures a machine capture over one or more disks.
// Either set Disks, or the legacy single-disk fields (Source/Disk/DiskSize/
// Plan) — not both.
type CaptureOptions struct {
	RepoPath string
	Disks    []DiskSpec
	Hostname string
	// CaptureFiles (#151): scan NTFS members into per-member file catalogs
	// (stored on each member's own backup manifest) — one machine snapshot
	// then serves both BMR and browse/restore-one-file recovery.
	CaptureFiles bool
	// Legacy single-disk fields (equivalent to Disks with one entry).
	Source   string
	Disk     io.ReaderAt
	DiskSize int64
	Plan     []MemberPlan
	// NewPipeline builds a fresh pipeline per member backup (carries keys,
	// progress hooks, config). Each call must return an independent instance.
	NewPipeline func() *pipeline.Pipeline
}

// Capture backs up one or more whole disks as a single machine snapshot and
// returns its ID. Every member is an ordinary repo backup (dedup'd,
// verifiable, restorable on its own); the machine manifest binds them — and
// every disk's byte-exact GPT layout — under one snapshot identity.
func Capture(ctx context.Context, opts CaptureOptions) (string, *disklayout.MachineManifest, error) {
	specs := opts.Disks
	if opts.CaptureFiles {
		for i := range specs {
			specs[i].CaptureFiles = true
		}
	}
	if len(specs) == 0 {
		specs = []DiskSpec{{Source: opts.Source, Disk: opts.Disk, DiskSize: opts.DiskSize, Plan: opts.Plan, CaptureFiles: opts.CaptureFiles}}
	}

	m := &disklayout.MachineManifest{
		MachineID: opts.Hostname,
		Hostname:  opts.Hostname,
		OS:        runtime.GOOS,
		CreatedAt: time.Now().UTC(),
	}
	for di, spec := range specs {
		dc, err := captureOneDisk(ctx, opts.RepoPath, spec, opts.NewPipeline)
		if err != nil {
			return "", nil, fmt.Errorf("disk %d (%s): %w", di, spec.Source, err)
		}
		m.Disks = append(m.Disks, dc)
	}

	snapID := manifest.NewBackupID()
	if err := disklayout.SaveMachineManifest(opts.RepoPath, snapID, m); err != nil {
		return "", nil, fmt.Errorf("saving machine manifest: %w", err)
	}
	return snapID, m, nil
}

// captureOneDisk backs up one disk's partitions per its plan and returns the
// DiskCapture record (verbatim GPT regions + member backup IDs).
func captureOneDisk(ctx context.Context, repoPath string, spec DiskSpec, newPipeline func() *pipeline.Pipeline) (disklayout.DiskCapture, error) {
	var zero disklayout.DiskCapture
	l, err := disklayout.Parse(spec.Disk, spec.DiskSize)
	if err != nil {
		return zero, fmt.Errorf("parsing disk layout: %w", err)
	}

	readRegion := func(r disklayout.Range) ([]byte, error) {
		buf := make([]byte, r.Length)
		if _, err := spec.Disk.ReadAt(buf, r.Offset); err != nil {
			return nil, err
		}
		return buf, nil
	}
	primary, err := readRegion(l.PrimaryRegion)
	if err != nil {
		return zero, fmt.Errorf("reading primary GPT region: %w", err)
	}
	backup, err := readRegion(l.BackupRegion)
	if err != nil {
		return zero, fmt.Errorf("reading backup GPT region: %w", err)
	}

	plan := spec.Plan
	if plan == nil {
		plan = DefaultPlan(l)
	}
	partByIndex := map[int]disklayout.Partition{}
	for _, p := range l.Partitions {
		partByIndex[p.Index] = p
	}

	var members []disklayout.PartitionMember
	for _, mp := range plan {
		p, ok := partByIndex[mp.Index]
		if !ok {
			return zero, fmt.Errorf("plan references partition index %d not in layout", mp.Index)
		}
		switch mp.Kind {
		case disklayout.MemberSkipped:
			members = append(members, disklayout.PartitionMember{Index: mp.Index, Kind: mp.Kind, Reason: mp.Reason})
			continue
		case disklayout.MemberRaw, disklayout.MemberVolume:
		default:
			return zero, fmt.Errorf("partition %d: unknown member kind %q", mp.Index, mp.Kind)
		}

		length := p.Length(l.SectorSize)
		var src io.Reader
		if mp.Kind == disklayout.MemberRaw {
			src = io.NewSectionReader(spec.Disk, p.Offset(l.SectorSize), length)
		} else {
			if mp.Reader == nil {
				return zero, fmt.Errorf("partition %d: volume member without a reader", mp.Index)
			}
			src = mp.Reader
		}

		p2 := newPipeline()
		p2.BackupID = manifest.NewBackupID()
		if len(mp.ExcludePaths) > 0 || len(mp.ExcludeWarnings) > 0 {
			paths, warns := mp.ExcludePaths, mp.ExcludeWarnings
			p2.StampManifest = func(m *manifest.Backup) {
				m.ExcludePaths, m.ExcludeWarnings = paths, warns
			}
		}
		if spec.CaptureFiles {
			// Member catalogs (#151): extents are PARTITION-relative — the
			// member stream is the partition, so restore-files reads line
			// up. Prefer the quiesced scan source (VSS shadow, Windows)
			// when the plan provides one; otherwise scan the disk at the
			// partition offset. Non-NTFS members skip silently; real scan
			// failures fail the capture (the user asked for catalogs).
			files, serr := scanMemberCatalog(ctx, spec, mp, p)
			if serr != nil {
				return zero, fmt.Errorf("cataloging partition %d (%s): %w", mp.Index, p.TypeName, serr)
			}
			if files != nil {
				p2.SetFileCatalog("volume", []string{fmt.Sprintf("%s#p%d", spec.Source, mp.Index)}, files)
			}
		}
		name := fmt.Sprintf("%s#p%d(%s)", spec.Source, mp.Index, p.TypeName)
		res, err := p2.Backup(ctx, src, name, length, repoPath)
		if err != nil {
			return zero, fmt.Errorf("backing up partition %d (%s): %w", mp.Index, p.TypeName, err)
		}
		members = append(members, disklayout.PartitionMember{Index: mp.Index, Kind: mp.Kind, BackupID: res.BackupID})
	}

	// Aux structural regions (#149): the MBR EBR chain sits in gaps no
	// member covers — captured verbatim so logicals survive restore.
	var aux [][]byte
	for _, ar := range l.AuxRegions {
		ab, err := readRegion(ar)
		if err != nil {
			return zero, fmt.Errorf("reading aux region at %d: %w", ar.Offset, err)
		}
		aux = append(aux, ab)
	}

	return disklayout.DiskCapture{
		Source:     spec.Source,
		Layout:     *l,
		PrimaryGPT: primary,
		BackupGPT:  backup,
		AuxBytes:   aux,
		Members:    members,
	}, nil
}

// restoreMemberOrdinal gives (position, count) of a member among restorable
// members for progress labeling.
func restoreMemberOrdinal(members []disklayout.PartitionMember, index int) (int, int) {
	pos, count := 0, 0
	for _, m := range members {
		if m.Kind == disklayout.MemberSkipped {
			continue
		}
		if m.Index == index {
			pos = count
		}
		count++
	}
	return pos, count
}

// Target is where a disk restore writes: an image file or a raw device writer.
type Target interface {
	io.WriterAt
	Sync() error
}

// partitionTarget adapts a disk-offset range of the target to restore.Target.
type partitionTarget struct {
	t      Target
	base   int64
	length int64
}

func (p *partitionTarget) WriteAt(data []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(data)) > p.length {
		return 0, fmt.Errorf("write [%d,+%d) outside partition length %d", off, len(data), p.length)
	}
	return p.t.WriteAt(data, p.base+off)
}

// Truncate is a bounds assertion: a partition range has fixed size.
func (p *partitionTarget) Truncate(size int64) error {
	if size > p.length {
		return fmt.Errorf("restore stream %d exceeds partition length %d", size, p.length)
	}
	return nil
}

func (p *partitionTarget) Sync() error { return p.t.Sync() }

// RestoreDiskOptions configures a whole-disk restore.
type RestoreDiskOptions struct {
	RepoPath   string
	SnapshotID string
	DiskIndex  int // which disk of the machine snapshot
	Target     Target
	TargetSize int64
	// Open read-side stores once per restore (keys handled by cmd).
	Index      *index.DedupIndex
	ChunkStore *store.ChunkStore
	Logger     *slog.Logger
	// Normalizer is the repo's recorded normalizer (pipeline.Binding.Normalizer;
	// nil for a repo that records none). Chunk identity is the hash of
	// NORMALIZED bytes while ORIGINAL bytes are stored, so a restore that omits
	// it fails the integrity check on perfectly healthy data — which is what a
	// bare-metal restore from a `--normalize` repo used to do.
	Normalizer preprocess.Normalizer
	// OnProgress (#153): per-member restore progress for percent/rate/ETA
	// rendering — (memberIdx, memberCount, bytesDone, bytesTotal).
	OnProgress func(member, members int, done, total int64)
	// OnMemberEntries (#157): called with each member's chunk entries just
	// before that member restores — drivers build the per-pack fetch plan
	// (dense download vs ranged chunk fetch) from exactly what's needed.
	// An accessor, not a slice (#506): the member's entries stay on disk.
	OnMemberEntries func(entries manifest.EntryAccessor)

	// ReadAt reads back what Target received — the digest read-back seam
	// (#465). nil skips the check (a caller that cannot re-read its target).
	ReadAt io.ReaderAt
}

// RestoreDisk reassembles one captured disk byte-exactly onto a same-size
// target: verbatim GPT regions, then every member into its partition range
// (skipped members restore as zeros). Larger/smaller targets are refused in
// P1 — GPT header relocation is the P2 recovery-key work.
func RestoreDisk(ctx context.Context, opts RestoreDiskOptions) error {
	m, err := disklayout.LoadMachineManifest(opts.RepoPath, opts.SnapshotID)
	if err != nil {
		return fmt.Errorf("loading machine manifest: %w", err)
	}
	if opts.DiskIndex < 0 || opts.DiskIndex >= len(m.Disks) {
		return fmt.Errorf("machine snapshot has %d disks; index %d out of range", len(m.Disks), opts.DiskIndex)
	}
	d := m.Disks[opts.DiskIndex]
	if opts.TargetSize < d.Layout.DiskSize {
		return fmt.Errorf("target (%d bytes) is smaller than the captured disk (%d bytes); shrinking restores are not supported", opts.TargetSize, d.Layout.DiskSize)
	}

	// GPT regions: verbatim on a same-size target; on a LARGER target the
	// backup structures are relocated to the true end of the new disk and the
	// primary header's AlternateLBA patched (#76) — everything else, including
	// the disk signature and partition GUIDs BCD/fstab reference, stays
	// byte-identical. Extra space is left unallocated.
	primary, backupOff, backupBytes, err := disklayout.RelocateGPT(&d.Layout, d.PrimaryGPT, d.BackupGPT, opts.TargetSize)
	if err != nil {
		return err
	}
	if _, err := opts.Target.WriteAt(primary, d.Layout.PrimaryRegion.Offset); err != nil {
		return fmt.Errorf("writing primary GPT region: %w", err)
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

	partByIndex := map[int]disklayout.Partition{}
	for _, p := range d.Layout.Partitions {
		partByIndex[p.Index] = p
	}

	for _, mem := range d.Members {
		p := partByIndex[mem.Index]
		length := p.Length(d.Layout.SectorSize)
		pt := &partitionTarget{t: opts.Target, base: p.Offset(d.Layout.SectorSize), length: length}

		if mem.Kind == disklayout.MemberSkipped {
			if err := writeZeros(pt, length); err != nil {
				return fmt.Errorf("zeroing skipped partition %d: %w", mem.Index, err)
			}
			continue
		}

		// Header first, entries through an accessor (#506): a member's entry
		// list is never held whole — at 1.26M entries that was ~100 MB on the
		// recovery ISO's 2 GB, per member.
		backup, err := manifest.LoadMetadata(opts.RepoPath, mem.BackupID)
		if err != nil {
			return fmt.Errorf("loading member backup %s (partition %d): %w", mem.BackupID, mem.Index, err)
		}
		if backup.TotalBytes != length {
			return fmt.Errorf("partition %d: member backup covers %d bytes but partition is %d", mem.Index, backup.TotalBytes, length)
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
			r.OnProgress = func(done, total int64) {
				opts.OnProgress(memIdx, memCount, done, total)
			}
		}
		_, rerr := r.RestoreEntries(ctx, backup, entries, pt)
		if entriesCloser != nil {
			entriesCloser.Close()
		}
		if rerr != nil {
			return fmt.Errorf("restoring partition %d from %s: %w", mem.Index, mem.BackupID, rerr)
		}
		// #465: the member's span, read back and held against its capture
		// digest — per member, because "partition N disagrees" is actionable
		// and "the disk failed" is not. This is the restore an operator
		// boots a machine from; every chunk above verified individually, and
		// only the span fold can see that what LANDED is not the capture.
		if opts.ReadAt != nil {
			verdict, derr := restore.VerifyWrittenDigest(backup,
				io.NewSectionReader(opts.ReadAt, p.Offset(d.Layout.SectorSize), length))
			switch {
			case derr != nil:
				opts.Logger.Warn("restored partition could not be read back for digest verification",
					"partition", mem.Index, "backup", mem.BackupID, "err", derr)
			case verdict == restore.DigestMismatch:
				return fmt.Errorf("partition %d: restored bytes do not match member backup %s's capture "+
					"digest — the partition holds data the capture never produced; do not trust this restore",
					mem.Index, mem.BackupID)
			case verdict == restore.DigestNotVerifiable:
				opts.Logger.Info("pre-digest member backup; restored partition cannot be digest-verified",
					"partition", mem.Index, "backup", mem.BackupID)
			}
		}
	}
	return opts.Target.Sync()
}

func writeZeros(w io.WriterAt, length int64) error {
	buf := make([]byte, 1<<20)
	var off int64
	for off < length {
		n := int64(len(buf))
		if length-off < n {
			n = length - off
		}
		if _, err := w.WriteAt(buf[:n], off); err != nil {
			return err
		}
		off += n
	}
	return nil
}
