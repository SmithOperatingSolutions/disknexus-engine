// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/dsoprea/go-ext4"
	parser "www.velocidex.com/golang/go-ntfs/parser"
)

// Filesystem facts a drive upgrade (#223) and a removable-media source
// (#457) need without mounting anything: how small a filesystem can go, and
// what identifies it regardless of the letter or mount point it gets.

// FSMinimum is how much of a filesystem's partition is actually in use and
// the smallest partition it could be shrunk into.
type FSMinimum struct {
	Filesystem   string
	TotalBytes   int64
	UsedBytes    int64
	LastUsedEnd  int64 // exact end of the last in-use cluster/block; MinimumBytes is this, rounded up
	MinimumBytes int64 // 0 when not shrinkable
	Shrinkable   bool
	Reason       string // why it is not shrinkable, or how MinimumBytes was derived
}

// FSIdentity is what stays the same about a filesystem across letters,
// mount points and machines: its type, serial (NTFS/FAT: volume serial;
// ext4: UUID) and label.
type FSIdentity struct {
	Filesystem string
	Serial     string
	Label      string
	SizeBytes  int64
}

const mib = 1 << 20

func roundUpMiB(n int64) int64 { return (n + mib - 1) / mib * mib }

// MinimumSize reports the used and minimum sizes of the filesystem that
// starts partOffset bytes into source (a device, snapshot device, or image).
//
// NTFS: the minimum is the last in-use cluster from $Bitmap — the bound
// `ntfsresize --info` reports before it starts relocating clusters, so a
// plan against it never asks the tool for more than it can do. ext4: the
// superblock's used blocks plus the 5% headroom `resize2fs -P` keeps for
// metadata; the tool's own estimate is authoritative and the flow runs it.
// FAT32/exFAT: not shrinkable here.
func MinimumSize(source string, partOffset int64) (FSMinimum, error) {
	f, err := os.Open(toRawVolumePath(source))
	if err != nil {
		return FSMinimum{}, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()
	fsType, err := detectFSAt(f, partOffset)
	if err != nil {
		return FSMinimum{}, fmt.Errorf("detecting filesystem at %d: %w", partOffset, err)
	}
	switch fsType {
	case "ntfs":
		return ntfsMinimum(f, partOffset)
	case "ext4":
		return ext4Minimum(f, partOffset)
	default:
		id, ierr := identityAt(f, fsType, partOffset)
		if ierr != nil {
			return FSMinimum{}, ierr
		}
		return FSMinimum{Filesystem: fsType, TotalBytes: id.SizeBytes, Shrinkable: false,
			Reason: fsType + " filesystems cannot be shrunk by the recovery tools; only NTFS and ext4 can"}, nil
	}
}

// Identity reports the filesystem identity at partOffset in source.
func Identity(source string, partOffset int64) (FSIdentity, error) {
	f, err := os.Open(toRawVolumePath(source))
	if err != nil {
		return FSIdentity{}, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()
	fsType, err := detectFSAt(f, partOffset)
	if err != nil {
		return FSIdentity{}, fmt.Errorf("detecting filesystem at %d: %w", partOffset, err)
	}
	return identityAt(f, fsType, partOffset)
}

// IdentityAuto is Identity for a source whose filesystem starts at 0 or in
// its first partition (a whole volume device or an image of one).
func IdentityAuto(source string) (FSIdentity, error) {
	f, err := os.Open(toRawVolumePath(source))
	if err != nil {
		return FSIdentity{}, fmt.Errorf("opening source: %w", err)
	}
	defer f.Close()
	fsType, partOffset, err := detectFilesystem(f)
	if err != nil {
		return FSIdentity{}, err
	}
	return identityAt(f, fsType, partOffset)
}

func identityAt(f *os.File, fsType string, off int64) (FSIdentity, error) {
	boot := make([]byte, 512)
	if _, err := f.ReadAt(boot, off); err != nil {
		return FSIdentity{}, fmt.Errorf("reading boot sector: %w", err)
	}
	switch fsType {
	case "ntfs":
		g := ntfsGeometry(boot)
		id := FSIdentity{Filesystem: "ntfs", Serial: fmt.Sprintf("%016X", binary.LittleEndian.Uint64(boot[0x48:0x50])),
			SizeBytes: g.totalSectors * g.bytesPerSector}
		id.Label = ntfsLabel(f, off, g)
		return id, nil
	case "fat32":
		bps := int64(binary.LittleEndian.Uint16(boot[0x0B:0x0D]))
		total := int64(binary.LittleEndian.Uint32(boot[0x20:0x24]))
		if total == 0 {
			total = int64(binary.LittleEndian.Uint16(boot[0x13:0x15]))
		}
		label := strings.TrimRight(string(boot[0x47:0x52]), " \x00")
		if label == "NO NAME" {
			label = ""
		}
		return FSIdentity{Filesystem: "fat32", Serial: fmt.Sprintf("%08X", binary.LittleEndian.Uint32(boot[0x43:0x47])),
			Label: label, SizeBytes: total * bps}, nil
	case "exfat":
		shift := uint(boot[0x6C])
		return FSIdentity{Filesystem: "exfat", Serial: fmt.Sprintf("%08X", binary.LittleEndian.Uint32(boot[0x64:0x68])),
			SizeBytes: int64(binary.LittleEndian.Uint64(boot[0x48:0x50])) << shift}, nil
	case "ext4":
		sb, err := ext4Superblock(f, off)
		if err != nil {
			return FSIdentity{}, err
		}
		d := sb.Data()
		u := d.SUuid
		return FSIdentity{Filesystem: "ext4",
			Serial: fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", binary.BigEndian.Uint32(u[0:4]), binary.BigEndian.Uint16(u[4:6]),
				binary.BigEndian.Uint16(u[6:8]), binary.BigEndian.Uint16(u[8:10]), u[10:16]),
			Label:     strings.TrimRight(sb.VolumeName(), "\x00"),
			SizeBytes: ext4Blocks(sb) * int64(sb.BlockSize())}, nil
	}
	return FSIdentity{}, fmt.Errorf("no identity reader for %s", fsType)
}

type ntfsGeom struct {
	bytesPerSector, sectorsPerCluster, totalSectors int64
}

func (g ntfsGeom) clusterBytes() int64 { return g.bytesPerSector * g.sectorsPerCluster }

func ntfsGeometry(boot []byte) ntfsGeom {
	g := ntfsGeom{bytesPerSector: int64(binary.LittleEndian.Uint16(boot[0x0B:0x0D]))}
	spc := boot[0x0D]
	if spc > 0x80 { // large clusters encode a negative exponent
		g.sectorsPerCluster = int64(1) << (256 - int(spc))
	} else {
		g.sectorsPerCluster = int64(spc)
	}
	g.totalSectors = int64(binary.LittleEndian.Uint64(boot[0x28:0x30]))
	return g
}

func openNTFS(f *os.File, off int64) (*parser.NTFSContext, error) {
	sec := io.NewSectionReader(f, off, 1<<62)
	reader, err := parser.NewPagedReader(sec, 1024, 10000)
	if err != nil {
		return nil, fmt.Errorf("creating paged reader: %w", err)
	}
	ntfs, err := parser.GetNTFSContext(reader, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing NTFS: %w", err)
	}
	return ntfs, nil
}

func ntfsLabel(f *os.File, off int64, g ntfsGeom) string {
	ntfs, err := openNTFS(f, off)
	if err != nil {
		return ""
	}
	defer ntfs.Close()
	vol, err := ntfs.GetMFT(3) // $Volume
	if err != nil {
		return ""
	}
	attr, err := vol.GetAttribute(ntfs, 0x60, -1, "") // $VOLUME_NAME
	if err != nil || attr.DataSize() == 0 || attr.DataSize() > 512 {
		return ""
	}
	raw := make([]byte, attr.DataSize())
	if _, err := attr.Data(ntfs).ReadAt(raw, 0); err != nil && err != io.EOF {
		return ""
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(raw[i:i+2]))
	}
	return strings.TrimRight(string(utf16.Decode(u)), "\x00")
}

func ntfsMinimum(f *os.File, off int64) (FSMinimum, error) {
	boot := make([]byte, 512)
	if _, err := f.ReadAt(boot, off); err != nil {
		return FSMinimum{}, fmt.Errorf("reading NTFS boot sector: %w", err)
	}
	g := ntfsGeometry(boot)
	cluster := g.clusterBytes()
	if cluster <= 0 || g.totalSectors <= 0 {
		return FSMinimum{}, fmt.Errorf("implausible NTFS geometry: %d bytes/sector, %d sectors/cluster, %d sectors", g.bytesPerSector, g.sectorsPerCluster, g.totalSectors)
	}
	clusters := g.totalSectors / g.sectorsPerCluster
	total := g.totalSectors * g.bytesPerSector

	ntfs, err := openNTFS(f, off)
	if err != nil {
		return FSMinimum{}, err
	}
	defer ntfs.Close()
	bm, err := ntfs.GetMFT(6) // $Bitmap
	if err != nil {
		return FSMinimum{}, fmt.Errorf("reading $Bitmap: %w", err)
	}
	attr, err := bm.GetAttribute(ntfs, 128, -1, "")
	if err != nil {
		return FSMinimum{}, fmt.Errorf("$Bitmap has no data attribute: %w", err)
	}
	need := (clusters + 7) / 8
	if attr.DataSize() < need {
		return FSMinimum{}, fmt.Errorf("$Bitmap holds %d bytes, %d clusters need %d", attr.DataSize(), clusters, need)
	}
	bitmap := make([]byte, need)
	if _, err := attr.Data(ntfs).ReadAt(bitmap, 0); err != nil && err != io.EOF {
		return FSMinimum{}, fmt.Errorf("reading $Bitmap: %w", err)
	}
	// Bits past the cluster count in the last byte are padding.
	if extra := int(need*8 - clusters); extra > 0 {
		bitmap[need-1] &= 0xFF >> extra
	}
	var used int64
	lastUsed := int64(-1)
	for i, b := range bitmap {
		if b == 0 {
			continue
		}
		used += int64(bits.OnesCount8(b))
		lastUsed = int64(i)*8 + int64(bits.Len8(b)) - 1
	}
	lastEnd := (lastUsed + 1) * cluster
	minimum := roundUpMiB(lastEnd)
	if minimum > total {
		minimum = total
	}
	return FSMinimum{Filesystem: "ntfs", TotalBytes: total, UsedBytes: used * cluster, LastUsedEnd: lastEnd, MinimumBytes: minimum, Shrinkable: true,
		Reason: "last in-use cluster from $Bitmap (what ntfsresize --info reports before relocating)"}, nil
}

func ext4Superblock(f *os.File, off int64) (*ext4.Superblock, error) {
	if _, err := f.Seek(off+ext4.Superblock0Offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seeking to the ext4 superblock: %w", err)
	}
	sb, err := ext4.NewSuperblockWithReader(f)
	if err != nil {
		return nil, fmt.Errorf("parsing the ext4 superblock: %w", err)
	}
	return sb, nil
}

const ext4Incompat64Bit = 0x80

func ext4Blocks(sb *ext4.Superblock) int64 {
	d := sb.Data()
	n := int64(d.SBlocksCountLo)
	if d.SFeatureIncompat&ext4Incompat64Bit != 0 {
		n |= int64(d.SBlocksCountHi) << 32
	}
	return n
}

func ext4FreeBlocks(sb *ext4.Superblock) int64 {
	d := sb.Data()
	n := int64(d.SFreeBlocksCountLo)
	if d.SFeatureIncompat&ext4Incompat64Bit != 0 {
		n |= int64(d.SFreeBlocksCountHi) << 32
	}
	return n
}

func ext4Minimum(f *os.File, off int64) (FSMinimum, error) {
	sb, err := ext4Superblock(f, off)
	if err != nil {
		return FSMinimum{}, err
	}
	bs := int64(sb.BlockSize())
	blocks, free := ext4Blocks(sb), ext4FreeBlocks(sb)
	if blocks <= 0 || free > blocks {
		return FSMinimum{}, fmt.Errorf("implausible ext4 superblock: %d blocks, %d free", blocks, free)
	}
	total := blocks * bs
	used := (blocks - free) * bs
	minimum := roundUpMiB(used + used/20) // resize2fs -P keeps ~5% for metadata
	if minimum > total {
		minimum = total
	}
	return FSMinimum{Filesystem: "ext4", TotalBytes: total, UsedBytes: used, LastUsedEnd: used, MinimumBytes: minimum, Shrinkable: true,
		Reason: "used blocks from the superblock plus 5% headroom (resize2fs -P is authoritative)"}, nil
}
