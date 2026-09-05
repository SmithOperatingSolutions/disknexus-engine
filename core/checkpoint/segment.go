// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package checkpoint

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// Segment is the per-checkpoint delta emitted alongside a Progress record
// (#50/#55): the entries-sidecar bytes appended since the previous checkpoint
// and the dedup-index insert tuples for chunks stored since then. Replaying all
// segments up to a checkpoint reconstructs both the sidecar prefix and the
// session's index inserts — so resume can preload the index instead of
// rebuilding it from packs (impossible for cloud, expensive locally, refused
// for managed encryption).
type Segment struct {
	Seq             uint32 // 0-based checkpoint sequence within the backup
	EntriesLenAfter int64  // sidecar length after this segment's bytes
	SidecarBytes    []byte // raw sidecar records appended since the prior checkpoint
	Inserts         []InsertTuple
}

// InsertTuple mirrors index.IndexEntry without importing it (checkpoint is a
// leaf package): one dedup-index insert.
//
// WeakHash is here for #365. It is NOT part of index.IndexEntry — the hash
// index stores strong hashes only — but the dedup index's bloom filter is
// keyed on the weak hash (xxhash of the chunk plaintext), and a resume that
// replays these tuples has no other way to get one: the plaintext is gone with
// the suspended process, and rebuilding the bloom from packs is impossible for
// a cloud repo. Replaying with a zero weak hash flushed a bloom that had
// forgotten the whole prefix, so every later backup re-stored it.
type InsertTuple struct {
	StrongHash  [32]byte
	WeakHash    uint64
	PackNumber  uint32
	StoreOffset uint64
	ChunkLength uint32
}

// Delta is the pipeline-produced payload handed to CheckpointFn with each
// Progress: what changed since the previous checkpoint.
type Delta struct {
	SidecarBytes []byte
	Inserts      []InsertTuple
}

const (
	segMagic = "DNSG"
	// segVersion 2 added the weak hash to each insert tuple (#365). Version 1
	// segments — an upgrade landing on a backup suspended by the old build —
	// still parse, with a zero weak hash: exactly the pre-#365 behavior for
	// that one resumed run, which is strictly better than refusing to resume
	// it at all.
	segVersion     = 2
	insertSizeV1   = 32 + 4 + 8 + 4     // strong hash + pack + offset + length
	insertSize     = 32 + 8 + 4 + 8 + 4 // + weak hash
	segHeaderSize  = 4 + 1 + 4 + 8 + 4 + 4
	segTrailerSize = 4 // crc32
)

// insertSizeFor is the on-disk tuple width for a segment version.
func insertSizeFor(version byte) int {
	if version == 1 {
		return insertSizeV1
	}
	return insertSize
}

// MarshalSegment encodes a segment: magic, version, seq, entriesLenAfter,
// sidecar length + bytes, insert count + tuples, then a CRC32 over everything
// before it. Fixed layout so a torn tail is detected by length or CRC.
func MarshalSegment(s *Segment) []byte {
	n := segHeaderSize + len(s.SidecarBytes) + len(s.Inserts)*insertSize + segTrailerSize
	buf := make([]byte, 0, n)
	buf = append(buf, segMagic...)
	buf = append(buf, segVersion)
	buf = binary.LittleEndian.AppendUint32(buf, s.Seq)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(s.EntriesLenAfter))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s.SidecarBytes)))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(s.Inserts)))
	buf = append(buf, s.SidecarBytes...)
	for _, t := range s.Inserts {
		buf = append(buf, t.StrongHash[:]...)
		buf = binary.LittleEndian.AppendUint64(buf, t.WeakHash)
		buf = binary.LittleEndian.AppendUint32(buf, t.PackNumber)
		buf = binary.LittleEndian.AppendUint64(buf, t.StoreOffset)
		buf = binary.LittleEndian.AppendUint32(buf, t.ChunkLength)
	}
	buf = binary.LittleEndian.AppendUint32(buf, crc32.ChecksumIEEE(buf))
	return buf
}

// UnmarshalSegment decodes one segment from the front of data, returning the
// segment and the number of bytes consumed. A short or corrupt (bad magic/CRC)
// prefix returns an error — callers reading a stream treat that as
// end-of-valid-data (torn tail from a crash).
func UnmarshalSegment(data []byte) (*Segment, int, error) {
	if len(data) < segHeaderSize+segTrailerSize {
		return nil, 0, io.ErrUnexpectedEOF
	}
	if string(data[0:4]) != segMagic {
		return nil, 0, fmt.Errorf("bad segment magic")
	}
	version := data[4]
	if version != segVersion && version != 1 {
		return nil, 0, fmt.Errorf("unsupported segment version %d", version)
	}
	tupleSize := insertSizeFor(version)
	seq := binary.LittleEndian.Uint32(data[5:9])
	entriesLen := int64(binary.LittleEndian.Uint64(data[9:17]))
	scLen := int(binary.LittleEndian.Uint32(data[17:21]))
	insCount := int(binary.LittleEndian.Uint32(data[21:25]))
	total := segHeaderSize + scLen + insCount*tupleSize + segTrailerSize
	if scLen < 0 || insCount < 0 || total > len(data) {
		return nil, 0, io.ErrUnexpectedEOF
	}
	body := data[:total-segTrailerSize]
	wantCRC := binary.LittleEndian.Uint32(data[total-segTrailerSize : total])
	if crc32.ChecksumIEEE(body) != wantCRC {
		return nil, 0, fmt.Errorf("segment CRC mismatch")
	}
	s := &Segment{Seq: seq, EntriesLenAfter: entriesLen}
	off := segHeaderSize
	s.SidecarBytes = append([]byte(nil), data[off:off+scLen]...)
	off += scLen
	s.Inserts = make([]InsertTuple, insCount)
	for i := range s.Inserts {
		copy(s.Inserts[i].StrongHash[:], data[off:off+32])
		p := off + 32
		if version >= 2 {
			s.Inserts[i].WeakHash = binary.LittleEndian.Uint64(data[p : p+8])
			p += 8
		}
		s.Inserts[i].PackNumber = binary.LittleEndian.Uint32(data[p : p+4])
		s.Inserts[i].StoreOffset = binary.LittleEndian.Uint64(data[p+4 : p+12])
		s.Inserts[i].ChunkLength = binary.LittleEndian.Uint32(data[p+12 : p+16])
		off += tupleSize
	}
	return s, total, nil
}

// SegmentsPath is the local append-only segments file for a backup (#55).
func SegmentsPath(repoPath, backupID string) string {
	return Path(repoPath, backupID) + ".seg"
}

// AppendSegmentLocal appends one marshaled segment to the local segments file
// and fsyncs it. Called BEFORE the checkpoint record is written, so a valid
// checkpoint always has its covering segments durable (a segment without a
// checkpoint is a harmless over-run dropped on resume).
func AppendSegmentLocal(repoPath, backupID string, s *Segment) error {
	path := SegmentsPath(repoPath, backupID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating segments dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("opening segments file: %w", err)
	}
	if _, err := f.Write(MarshalSegment(s)); err != nil {
		f.Close()
		return fmt.Errorf("appending segment: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing segments file: %w", err)
	}
	return f.Close()
}

// ReadSegmentsLocal reads all valid segments from the local segments file,
// stopping (without error) at a torn or corrupt tail. Missing file → nil.
func ReadSegmentsLocal(repoPath, backupID string) ([]*Segment, error) {
	data, err := os.ReadFile(SegmentsPath(repoPath, backupID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return ParseSegments(data), nil
}

// ParseSegments decodes consecutive segments from data, stopping at the first
// torn/corrupt one (crash tail). Segments must be sequential from Seq 0; a gap
// stops parsing (later segments without their predecessors are unusable).
func ParseSegments(data []byte) []*Segment {
	var out []*Segment
	for len(data) > 0 {
		s, n, err := UnmarshalSegment(data)
		if err != nil {
			break
		}
		if s.Seq != uint32(len(out)) {
			break
		}
		out = append(out, s)
		data = data[n:]
	}
	return out
}

// TruncateSegmentsLocal cuts the local segments file down to its first
// keepCount segments, discarding over-run segments written after the durable
// checkpoint (a crash window). Without this, a resumed run's next segment would
// collide with a stale one carrying the same sequence number.
func TruncateSegmentsLocal(repoPath, backupID string, keepCount int) error {
	path := SegmentsPath(repoPath, backupID)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var keepBytes int64
	rest := data
	for i := 0; i < keepCount && len(rest) > 0; i++ {
		_, n, err := UnmarshalSegment(rest)
		if err != nil {
			return fmt.Errorf("segment %d unreadable while truncating: %w", i, err)
		}
		keepBytes += int64(n)
		rest = rest[n:]
	}
	if err := os.Truncate(path, keepBytes); err != nil {
		return err
	}
	// fsync so the truncation is durable before new segments are appended.
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// RemoveSegmentsLocal deletes the local segments file (on success or --restart).
func RemoveSegmentsLocal(repoPath, backupID string) error {
	err := os.Remove(SegmentsPath(repoPath, backupID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ReplayState is the reconstruction of a suspended backup's session from its
// segments, cut to a checkpoint's durable prefix.
type ReplayState struct {
	SidecarPrefix []byte // exactly EntriesLen bytes of sidecar records
	Inserts       []InsertTuple
}

// ReplaySegments concatenates segments and cuts them to the checkpoint's
// EntriesLen. It returns an error if the segments do not fully cover the
// checkpoint prefix (missing/torn data) — the caller then falls back to an
// index rebuild (local) or refuses (cloud/managed).
func ReplaySegments(segs []*Segment, c *Checkpoint) (*ReplayState, error) {
	rs := &ReplayState{}
	covered := int64(0)
	for _, s := range segs {
		if covered >= c.EntriesLen {
			break // over-run beyond the checkpoint (crash between segment and checkpoint write)
		}
		take := s.SidecarBytes
		if covered+int64(len(take)) > c.EntriesLen {
			take = take[:c.EntriesLen-covered]
		}
		rs.SidecarPrefix = append(rs.SidecarPrefix, take...)
		covered += int64(len(take))
		rs.Inserts = append(rs.Inserts, s.Inserts...)
	}
	if covered != c.EntriesLen {
		return nil, fmt.Errorf("segments cover %d of %d checkpoint sidecar bytes", covered, c.EntriesLen)
	}
	return rs, nil
}
