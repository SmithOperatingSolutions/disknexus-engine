// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// dnm.go — binary format constants and encode/decode helpers for the .dnm manifest format.
//
// File layout:
//
//	[File Header — 32 bytes]
//	[METADATA section — encodeMetadata() bytes]
//	[CATALOG section — length-prefixed FileEntry records]
//	[ENTRIES section — 45-byte Entry records, identical to .entries sidecar]
//	[Section Index — numSections × sectionIndexSize bytes]
//
// The file header contains the byte offset of the section index, so a reader
// can seek directly to any section without scanning the file.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
)

// Format constants.
const (
	dnmMagic   = "DNMANIF\x00"
	dnmVersion = uint16(1)

	fileHeaderSize   = 32
	sectionIndexSize = 36 // bytes per section index entry
	numSections      = 3

	// Section type identifiers.
	sectionMetadata = uint8(0x01)
	sectionCatalog  = uint8(0x02)
	sectionEntries  = uint8(0x03)

	// FileEntry flag bits packed into a single byte.
	flagIsDir     = uint8(1 << 0)
	flagIsSymlink = uint8(1 << 1)
	flagUnchanged = uint8(1 << 2)
	flagExcluded  = uint8(1 << 3) // blocks zeroed by the capture exclusion map (#94)

	// count16Sentinel escapes uint16 count/length fields: values below the
	// sentinel are stored as a plain uint16 (byte-identical to older files);
	// larger values are stored as the sentinel followed by a uint32. Without
	// the escape, counts >= 65536 silently wrapped mod 65536 on encode while
	// every item was still written, so decoders dropped data without error.
	count16Sentinel = uint16(0xFFFF)

	// zeroTimeNano marks a zero time.Time in encoded timestamp fields.
	// 0 is a valid timestamp (the Unix epoch, common on files extracted from
	// archives) and must round-trip; MinInt64 is ~292 years before year 1678,
	// unrepresentable as a real mtime, and never written by older encoders.
	zeroTimeNano = math.MinInt64
)

// File header layout (32 bytes):
//
//	[0:8]   Magic: "DNMANIF\x00"
//	[8:10]  Version: uint16 LE
//	[10:12] Flags: uint16 LE
//	[12:20] SectionIndexOffset: uint64 LE  (byte offset from start of file)
//	[20:24] SectionIndexCount: uint32 LE
//	[24:28] FileCRC32: uint32 LE (0 = not computed)
//	[28:32] Reserved: [4]byte

// encodeHeader returns a 32-byte file header with the given section index offset.
func encodeHeader(sectionIndexOffset uint64) [fileHeaderSize]byte {
	var buf [fileHeaderSize]byte
	copy(buf[0:8], dnmMagic)
	binary.LittleEndian.PutUint16(buf[8:10], dnmVersion)
	// buf[10:12] Flags = 0
	binary.LittleEndian.PutUint64(buf[12:20], sectionIndexOffset)
	binary.LittleEndian.PutUint32(buf[20:24], numSections)
	// buf[24:28] FileCRC32 = 0
	// buf[28:32] Reserved = 0
	return buf
}

// sectionInfo describes one section as read from the section index.
type sectionInfo struct {
	typ    uint8
	offset uint64
	length uint64
	count  uint64
}

// Section index entry layout (36 bytes):
//
//	[0:1]   SectionType: uint8
//	[1:8]   Reserved: [7]byte
//	[8:16]  SectionOffset: uint64 LE
//	[16:24] SectionLength: uint64 LE
//	[24:32] RecordCount: uint64 LE
//	[32:36] SectionCRC32: uint32 LE (0 = not computed)

// encodeSectionIndex returns a 36-byte section index entry.
func encodeSectionIndex(typ uint8, offset, length, count uint64) [sectionIndexSize]byte {
	var buf [sectionIndexSize]byte
	buf[0] = typ
	// buf[1:8] reserved = 0
	binary.LittleEndian.PutUint64(buf[8:16], offset)
	binary.LittleEndian.PutUint64(buf[16:24], length)
	binary.LittleEndian.PutUint64(buf[24:32], count)
	// buf[32:36] SectionCRC32 = 0
	return buf
}

// decodeSectionIndex decodes a 36-byte buffer into a sectionInfo.
func decodeSectionIndex(buf [sectionIndexSize]byte) sectionInfo {
	return sectionInfo{
		typ:    buf[0],
		offset: binary.LittleEndian.Uint64(buf[8:16]),
		length: binary.LittleEndian.Uint64(buf[16:24]),
		count:  binary.LittleEndian.Uint64(buf[24:32]),
	}
}

// DNMPath returns the path of the .dnm manifest file for a backup.
func DNMPath(repoPath, backupID string) string {
	return filepath.Join(repoPath, "manifests", backupID+".dnm")
}

// ---------------------------------------------------------------------------
// Metadata encode/decode
// ---------------------------------------------------------------------------

// encodeMetadata encodes all Backup scalar fields (no Entries, no FileCatalog)
// into a flat byte slice. The returned slice is the METADATA section content.
func encodeMetadata(b *Backup) []byte {
	var w bytes.Buffer
	writeStr8(&w, b.BackupID)
	writeStr16(&w, b.SourceVolume)
	writeStr8(&w, b.BackupType)
	writeStr8(&w, b.BackupMode)
	writeStr8(&w, b.ParentBackupID)
	writeTimeNano(&w, b.Timestamp)
	writeUint32(&w, uint32(b.SectorSize))
	writeUint32(&w, uint32(b.ClusterSize))
	writeInt64(&w, b.TotalBytes)
	writeInt64(&w, b.TotalChunks)
	writeInt64(&w, b.UniqueChunks)
	writeInt64(&w, b.DedupChunks)
	writeInt64(&w, b.RawBytes)
	writeInt64(&w, b.StoredBytes)
	writeFloat64(&w, b.DedupRatio)
	writeFloat64(&w, b.CompRatio)
	writeStr8(&w, b.Duration)
	writeInt64(&w, b.ChangedChunks)
	writeInt64(&w, b.UnchangedChunks)
	writeCount16(&w, len(b.SourcePaths))
	for _, p := range b.SourcePaths {
		writeStr16(&w, p)
	}
	writeCount16(&w, len(b.WrappedDEK))
	w.Write(b.WrappedDEK)
	// #455: appended at the END, so a pre-digest reader — which stops after
	// WrappedDEK and ignores trailing bytes — still reads everything it
	// knows about. New fields on this format go after these, same rule.
	writeStr8(&w, b.ContentDigest)
	writeStr8(&w, b.ContentDigestCovers)
	// #468: operator exclusions, after the digest pair by the same rule.
	writeCount16(&w, len(b.ExcludePaths))
	for _, p := range b.ExcludePaths {
		writeStr16(&w, p)
	}
	writeCount16(&w, len(b.ExcludeWarnings))
	for _, p := range b.ExcludeWarnings {
		writeStr16(&w, p)
	}
	return w.Bytes()
}

// decodeMetadata decodes a METADATA section into a Backup.
// Entries and FileCatalog are left nil.
func decodeMetadata(data []byte) (Backup, error) {
	r := bytes.NewReader(data)
	var b Backup
	var err error
	if b.BackupID, err = readStr8(r); err != nil {
		return b, fmt.Errorf("BackupID: %w", err)
	}
	if b.SourceVolume, err = readStr16(r); err != nil {
		return b, fmt.Errorf("SourceVolume: %w", err)
	}
	if b.BackupType, err = readStr8(r); err != nil {
		return b, fmt.Errorf("BackupType: %w", err)
	}
	if b.BackupMode, err = readStr8(r); err != nil {
		return b, fmt.Errorf("BackupMode: %w", err)
	}
	if b.ParentBackupID, err = readStr8(r); err != nil {
		return b, fmt.Errorf("ParentBackupID: %w", err)
	}
	if b.Timestamp, err = readTimeNano(r); err != nil {
		return b, fmt.Errorf("Timestamp: %w", err)
	}
	var u32 uint32
	if u32, err = readUint32(r); err != nil {
		return b, fmt.Errorf("SectorSize: %w", err)
	}
	b.SectorSize = int(u32)
	if u32, err = readUint32(r); err != nil {
		return b, fmt.Errorf("ClusterSize: %w", err)
	}
	b.ClusterSize = int(u32)
	if b.TotalBytes, err = readInt64(r); err != nil {
		return b, fmt.Errorf("TotalBytes: %w", err)
	}
	if b.TotalChunks, err = readInt64(r); err != nil {
		return b, fmt.Errorf("TotalChunks: %w", err)
	}
	if b.UniqueChunks, err = readInt64(r); err != nil {
		return b, fmt.Errorf("UniqueChunks: %w", err)
	}
	if b.DedupChunks, err = readInt64(r); err != nil {
		return b, fmt.Errorf("DedupChunks: %w", err)
	}
	if b.RawBytes, err = readInt64(r); err != nil {
		return b, fmt.Errorf("RawBytes: %w", err)
	}
	if b.StoredBytes, err = readInt64(r); err != nil {
		return b, fmt.Errorf("StoredBytes: %w", err)
	}
	if b.DedupRatio, err = readFloat64(r); err != nil {
		return b, fmt.Errorf("DedupRatio: %w", err)
	}
	if b.CompRatio, err = readFloat64(r); err != nil {
		return b, fmt.Errorf("CompRatio: %w", err)
	}
	if b.Duration, err = readStr8(r); err != nil {
		return b, fmt.Errorf("Duration: %w", err)
	}
	if b.ChangedChunks, err = readInt64(r); err != nil {
		return b, fmt.Errorf("ChangedChunks: %w", err)
	}
	if b.UnchangedChunks, err = readInt64(r); err != nil {
		return b, fmt.Errorf("UnchangedChunks: %w", err)
	}
	pathCount, err := readCount16(r)
	if err != nil {
		return b, fmt.Errorf("SourcePathCount: %w", err)
	}
	b.SourcePaths = make([]string, pathCount)
	for i := range b.SourcePaths {
		if b.SourcePaths[i], err = readStr16(r); err != nil {
			return b, fmt.Errorf("SourcePaths[%d]: %w", i, err)
		}
	}
	dekLen, err := readCount16(r)
	if err != nil {
		return b, fmt.Errorf("WrappedDEKLen: %w", err)
	}
	if dekLen > 0 {
		b.WrappedDEK = make([]byte, dekLen)
		if _, err = io.ReadFull(r, b.WrappedDEK); err != nil {
			return b, fmt.Errorf("WrappedDEK: %w", err)
		}
	}
	// #455 content digest: present from the first writer that folded one,
	// absent — reported as clean EOF here — on every manifest written before
	// it. Absence stays absence: verification reports those NOT VERIFIABLE
	// rather than inventing a value to compare against.
	if b.ContentDigest, err = readStr8(r); err != nil {
		if errors.Is(err, io.EOF) {
			return b, nil // pre-digest manifest
		}
		return b, fmt.Errorf("ContentDigest: %w", err)
	}
	if b.ContentDigestCovers, err = readStr8(r); err != nil {
		if errors.Is(err, io.EOF) {
			// A digest with no covers definition is a value nothing can
			// honestly compare against; drop it rather than guess.
			b.ContentDigest = ""
			return b, nil
		}
		return b, fmt.Errorf("ContentDigestCovers: %w", err)
	}
	// #468 operator exclusions: absent on every manifest written before
	// them, which is "none configured" — the same thing an empty list says.
	n, err := readCount16(r)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return b, nil
		}
		return b, fmt.Errorf("ExcludePaths count: %w", err)
	}
	for i := 0; i < n; i++ {
		p, err := readStr16(r)
		if err != nil {
			return b, fmt.Errorf("ExcludePaths[%d]: %w", i, err)
		}
		b.ExcludePaths = append(b.ExcludePaths, p)
	}
	if n, err = readCount16(r); err != nil {
		return b, fmt.Errorf("ExcludeWarnings count: %w", err)
	}
	for i := 0; i < n; i++ {
		p, err := readStr16(r)
		if err != nil {
			return b, fmt.Errorf("ExcludeWarnings[%d]: %w", i, err)
		}
		b.ExcludeWarnings = append(b.ExcludeWarnings, p)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// FileEntry encode/decode
// ---------------------------------------------------------------------------

// encodeFileEntry encodes one FileEntry into binary form (without the length prefix).
//
// Layout:
//
//	PathLen:       uint16 LE + Path bytes
//	SourceIndex:   int32 LE
//	Size:          int64 LE
//	Mode:          uint32 LE
//	ModTimeNano:   int64 LE  (MinInt64 = zero time; 0 = Unix epoch)
//	Flags:         uint8     (bit0=IsDir, bit1=IsSymlink, bit2=Unchanged, bit3=IsExcluded)
//	LinkTargetLen: uint16 LE + LinkTarget bytes
//	StreamOffset:  int64 LE
//	StreamLength:  int64 LE
//	ContentHash:   [32]byte
//	DataBkpIDLen:  uint8 + DataBkpID bytes
//	ExtentCount:   uint16 LE (0xFFFF = escape: real count follows as uint32 LE)
//	Extents:       ExtentCount × (FileOffset int64 + VolumeOffset int64 + Length int64)
func encodeFileEntry(fe FileEntry) []byte {
	var w bytes.Buffer
	writeStr16(&w, fe.Path)
	writeInt32(&w, int32(fe.SourceIndex))
	writeInt64(&w, fe.Size)
	writeUint32(&w, fe.Mode)
	writeTimeNano(&w, fe.ModTime)
	var flags uint8
	if fe.IsDir {
		flags |= flagIsDir
	}
	if fe.IsSymlink {
		flags |= flagIsSymlink
	}
	if fe.Unchanged {
		flags |= flagUnchanged
	}
	if fe.IsExcluded {
		flags |= flagExcluded
	}
	w.WriteByte(flags)
	writeStr16(&w, fe.LinkTarget)
	writeInt64(&w, fe.StreamOffset)
	writeInt64(&w, fe.StreamLength)
	w.Write(fe.ContentHash[:])
	writeStr8(&w, fe.DataBackupID)
	writeCount16(&w, len(fe.VolumeExtents))
	for _, ext := range fe.VolumeExtents {
		writeInt64(&w, ext.FileOffset)
		writeInt64(&w, ext.VolumeOffset)
		writeInt64(&w, ext.Length)
	}
	// InlineData: count16 length prefix + bytes.
	// Appended after extents for backward compatibility: old readers stop here and
	// ignore trailing bytes; new readers of old entries see r.Len()==0 and skip.
	writeCount16(&w, len(fe.InlineData))
	w.Write(fe.InlineData)
	return w.Bytes()
}

// decodeFileEntry decodes one FileEntry from a byte slice.
func decodeFileEntry(data []byte) (FileEntry, error) {
	r := bytes.NewReader(data)
	var fe FileEntry
	var err error
	if fe.Path, err = readStr16(r); err != nil {
		return fe, fmt.Errorf("Path: %w", err)
	}
	si, err := readInt32(r)
	if err != nil {
		return fe, fmt.Errorf("SourceIndex: %w", err)
	}
	fe.SourceIndex = int(si)
	if fe.Size, err = readInt64(r); err != nil {
		return fe, fmt.Errorf("Size: %w", err)
	}
	if fe.Mode, err = readUint32(r); err != nil {
		return fe, fmt.Errorf("Mode: %w", err)
	}
	if fe.ModTime, err = readTimeNano(r); err != nil {
		return fe, fmt.Errorf("ModTime: %w", err)
	}
	flags, err := r.ReadByte()
	if err != nil {
		return fe, fmt.Errorf("Flags: %w", err)
	}
	fe.IsDir = flags&flagIsDir != 0
	fe.IsSymlink = flags&flagIsSymlink != 0
	fe.Unchanged = flags&flagUnchanged != 0
	fe.IsExcluded = flags&flagExcluded != 0
	if fe.LinkTarget, err = readStr16(r); err != nil {
		return fe, fmt.Errorf("LinkTarget: %w", err)
	}
	if fe.StreamOffset, err = readInt64(r); err != nil {
		return fe, fmt.Errorf("StreamOffset: %w", err)
	}
	if fe.StreamLength, err = readInt64(r); err != nil {
		return fe, fmt.Errorf("StreamLength: %w", err)
	}
	if _, err = io.ReadFull(r, fe.ContentHash[:]); err != nil {
		return fe, fmt.Errorf("ContentHash: %w", err)
	}
	if fe.DataBackupID, err = readStr8(r); err != nil {
		return fe, fmt.Errorf("DataBackupID: %w", err)
	}
	extCount, err := readCount16(r)
	if err != nil {
		return fe, fmt.Errorf("ExtentCount: %w", err)
	}
	if extCount > 0 {
		fe.VolumeExtents = make([]VolumeExtent, extCount)
		for i := range fe.VolumeExtents {
			var ext VolumeExtent
			if ext.FileOffset, err = readInt64(r); err != nil {
				return fe, fmt.Errorf("Extent[%d].FileOffset: %w", i, err)
			}
			if ext.VolumeOffset, err = readInt64(r); err != nil {
				return fe, fmt.Errorf("Extent[%d].VolumeOffset: %w", i, err)
			}
			if ext.Length, err = readInt64(r); err != nil {
				return fe, fmt.Errorf("Extent[%d].Length: %w", i, err)
			}
			fe.VolumeExtents[i] = ext
		}
	}
	// InlineData (added in v1 extension; absent in older entries — r.Len()==0 → skip).
	if r.Len() >= 2 {
		inlineLen, err := readCount16(r)
		if err != nil {
			return fe, fmt.Errorf("InlineDataLen: %w", err)
		}
		if inlineLen > 0 {
			fe.InlineData = make([]byte, inlineLen)
			if _, err = io.ReadFull(r, fe.InlineData); err != nil {
				return fe, fmt.Errorf("InlineData: %w", err)
			}
		}
	}
	return fe, nil
}

// ---------------------------------------------------------------------------
// Low-level write helpers (write to *bytes.Buffer; Write never returns an error)
// ---------------------------------------------------------------------------

func writeStr8(w *bytes.Buffer, s string) {
	if len(s) > 255 {
		s = s[:255]
	}
	w.WriteByte(byte(len(s)))
	w.WriteString(s)
}

func writeStr16(w *bytes.Buffer, s string) {
	if len(s) > 65535 {
		s = s[:65535]
	}
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], uint16(len(s)))
	w.Write(buf[:])
	w.WriteString(s)
}

func writeUint16(w *bytes.Buffer, v uint16) {
	var buf [2]byte
	binary.LittleEndian.PutUint16(buf[:], v)
	w.Write(buf[:])
}

func writeUint32(w *bytes.Buffer, v uint32) {
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], v)
	w.Write(buf[:])
}

func writeInt32(w *bytes.Buffer, v int32) {
	writeUint32(w, uint32(v))
}

func writeInt64(w *bytes.Buffer, v int64) {
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(v))
	w.Write(buf[:])
}

func writeFloat64(w *bytes.Buffer, v float64) {
	writeInt64(w, int64(math.Float64bits(v)))
}

// writeCount16 writes a count/length that is nominally uint16 but may exceed
// it: values < count16Sentinel are a plain uint16; larger values are the
// sentinel followed by the real count as uint32.
func writeCount16(w *bytes.Buffer, n int) {
	if n < int(count16Sentinel) {
		writeUint16(w, uint16(n))
		return
	}
	writeUint16(w, count16Sentinel)
	writeUint32(w, uint32(n))
}

// writeTimeNano encodes t as UnixNano, using zeroTimeNano for the zero time
// so that a genuine epoch timestamp (UnixNano == 0) round-trips.
func writeTimeNano(w *bytes.Buffer, t time.Time) {
	if t.IsZero() {
		writeInt64(w, zeroTimeNano)
		return
	}
	writeInt64(w, t.UnixNano())
}

// ---------------------------------------------------------------------------
// Low-level read helpers
// ---------------------------------------------------------------------------

func readStr8(r io.Reader) (string, error) {
	var lenBuf [1]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", err
	}
	if lenBuf[0] == 0 {
		return "", nil
	}
	data := make([]byte, lenBuf[0])
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}
	return string(data), nil
}

func readStr16(r io.Reader) (string, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return "", err
	}
	n := binary.LittleEndian.Uint16(lenBuf[:])
	if n == 0 {
		return "", nil
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", err
	}
	return string(data), nil
}

func readUint16(r io.Reader) (uint16, error) {
	var buf [2]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[:]), nil
}

func readUint32(r io.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf[:]), nil
}

func readInt32(r io.Reader) (int32, error) {
	v, err := readUint32(r)
	return int32(v), err
}

func readInt64(r io.Reader) (int64, error) {
	var buf [8]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(buf[:])), nil
}

func readFloat64(r io.Reader) (float64, error) {
	v, err := readInt64(r)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(uint64(v)), nil
}

// readCount16 reads a count written by writeCount16.
func readCount16(r io.Reader) (int, error) {
	v, err := readUint16(r)
	if err != nil {
		return 0, err
	}
	if v != count16Sentinel {
		return int(v), nil
	}
	v32, err := readUint32(r)
	if err != nil {
		return 0, err
	}
	return int(v32), nil
}

// readTimeNano decodes a timestamp written by writeTimeNano. Older encoders
// wrote 0 for both the zero time and a genuine epoch timestamp; 0 is decoded
// as the epoch, since a real epoch mtime (files extracted from archives or
// container images) must restore correctly, while an unset mtime does not
// occur for entries walked from a real filesystem.
func readTimeNano(r io.Reader) (time.Time, error) {
	v, err := readInt64(r)
	if err != nil {
		return time.Time{}, err
	}
	if v == zeroTimeNano {
		return time.Time{}, nil
	}
	return time.Unix(0, v).UTC(), nil
}

// CorruptCatalogSectionForTest flips bytes inside the CATALOG section —
// test hook proving block restores never read it (#153).
func CorruptCatalogSectionForTest(path string) error {
	r, err := OpenDNMReader(path)
	if err != nil {
		return err
	}
	off, length := r.catalog.offset, r.catalog.length
	r.Close()
	if length == 0 {
		return fmt.Errorf("no catalog section to corrupt")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	junk := make([]byte, length)
	for i := range junk {
		junk[i] = 0xAB
	}
	_, err = f.WriteAt(junk, int64(off))
	return err
}
