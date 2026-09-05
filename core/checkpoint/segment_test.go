// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package checkpoint_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/checkpoint"
)

// v1SegmentBytes hand-encodes the pre-#365 layout: magic, version 1, seq,
// entriesLenAfter, sidecarLen, insertCount, sidecar, then tuples of 48 bytes
// (strong hash, pack, offset, length — no weak hash), then crc32.
func v1SegmentBytes(sidecar []byte, strongs [][32]byte) []byte {
	var body []byte
	body = append(body, "DNSG"...)
	body = append(body, 1)
	body = binary.LittleEndian.AppendUint32(body, 0)
	body = binary.LittleEndian.AppendUint64(body, 900)
	body = binary.LittleEndian.AppendUint32(body, uint32(len(sidecar)))
	body = binary.LittleEndian.AppendUint32(body, uint32(len(strongs)))
	body = append(body, sidecar...)
	for i, strong := range strongs {
		body = append(body, strong[:]...)
		body = binary.LittleEndian.AppendUint32(body, uint32(i))     // pack
		body = binary.LittleEndian.AppendUint64(body, uint64(i*100)) // offset
		body = binary.LittleEndian.AppendUint32(body, 42)            // length
	}
	return binary.LittleEndian.AppendUint32(body, crc32.ChecksumIEEE(body))
}

func mkSeg(seq uint32, lenAfter int64, sidecar []byte, n int) *checkpoint.Segment {
	s := &checkpoint.Segment{Seq: seq, EntriesLenAfter: lenAfter, SidecarBytes: sidecar}
	for i := 0; i < n; i++ {
		var t checkpoint.InsertTuple
		t.StrongHash[0] = byte(seq)
		t.StrongHash[1] = byte(i)
		t.WeakHash = uint64(seq)<<32 | uint64(i) + 1 // the bloom's key (#365); never zero
		t.PackNumber = seq
		t.StoreOffset = uint64(i * 100)
		t.ChunkLength = 42
		s.Inserts = append(s.Inserts, t)
	}
	return s
}

func TestSegmentRoundTrip(t *testing.T) {
	s := mkSeg(3, 900, []byte("sidecar-bytes-here"), 5)
	data := checkpoint.MarshalSegment(s)
	got, n, err := checkpoint.UnmarshalSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Fatalf("consumed %d of %d bytes", n, len(data))
	}
	if got.Seq != 3 || got.EntriesLenAfter != 900 || !bytes.Equal(got.SidecarBytes, s.SidecarBytes) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Inserts) != 5 || got.Inserts[4] != s.Inserts[4] {
		t.Fatalf("inserts mismatch: %+v", got.Inserts)
	}
}

// TestVersion1SegmentStillParses: #365 widened the insert tuple with the weak
// hash and bumped the segment version to 2. A backup suspended by an older
// build has version-1 segments on disk (or in S3), and refusing them would
// turn an upgrade into an unresumable backup. They must parse, and the weak
// hash they cannot carry decodes as zero.
//
// THE ZERO IS A LOSS, NOT A CORRECT VALUE (#378). It is asserted here because
// it is what a v1 tuple decodes to and there is nothing else it could decode
// to: the weak hash is computed from chunk BYTES the resumed run never reads
// again, so no filter and no fixup anywhere in internal/ can recover it. What
// was missing is any statement of what the loss COSTS — pipeline.go:419 feeds
// these tuples straight into the bloom the resumed run then flushes as the
// repository's own. The cost is bounded, and the bound is asserted next door
// in TestAVersion1ResumesLostWeakHashesCostStorageAndNothingElse. Read that
// before treating this line as "the zero is fine".
func TestVersion1SegmentStillParses(t *testing.T) {
	sidecar := []byte("v1-sidecar")
	strongs := make([][32]byte, 2)
	for i := range strongs {
		strongs[i][0] = byte(i + 7)
	}
	data := v1SegmentBytes(sidecar, strongs)

	got, n, err := checkpoint.UnmarshalSegment(data)
	if err != nil {
		t.Fatalf("a version-1 segment must still parse (an upgrade must not strand a suspended backup): %v", err)
	}
	if n != len(data) {
		t.Fatalf("consumed %d of %d bytes — the v1 tuple width is wrong", n, len(data))
	}
	if !bytes.Equal(got.SidecarBytes, sidecar) || len(got.Inserts) != 2 {
		t.Fatalf("v1 segment decoded wrong: %+v", got)
	}
	if got.Inserts[1].StrongHash[0] != 8 || got.Inserts[1].PackNumber != 1 ||
		got.Inserts[1].StoreOffset != 100 || got.Inserts[1].ChunkLength != 42 {
		t.Fatalf("v1 tuple fields misaligned: %+v", got.Inserts[1])
	}
	if got.Inserts[0].WeakHash != 0 || got.Inserts[1].WeakHash != 0 {
		t.Fatalf("v1 tuples carry no weak hash; got %+v", got.Inserts)
	}
}

// TestSegmentTamperAndTornTail: a flipped byte fails CRC; a truncated stream
// stops at the last intact segment — never yields corrupt data.
func TestSegmentTamperAndTornTail(t *testing.T) {
	a := checkpoint.MarshalSegment(mkSeg(0, 90, bytes.Repeat([]byte{1}, 90), 2))
	b := checkpoint.MarshalSegment(mkSeg(1, 180, bytes.Repeat([]byte{2}, 90), 2))
	stream := append(append([]byte{}, a...), b...)

	// Clean parse: both segments.
	if got := checkpoint.ParseSegments(stream); len(got) != 2 {
		t.Fatalf("clean parse = %d segments, want 2", len(got))
	}

	// Tamper a byte inside segment 1's payload: parse keeps only segment 0.
	tampered := append([]byte{}, stream...)
	tampered[len(a)+30] ^= 0xFF
	if got := checkpoint.ParseSegments(tampered); len(got) != 1 || got[0].Seq != 0 {
		t.Fatalf("tampered parse = %d segments, want just seq 0", len(got))
	}

	// Torn tail (crash mid-append): parse keeps only segment 0.
	torn := stream[:len(a)+10]
	if got := checkpoint.ParseSegments(torn); len(got) != 1 {
		t.Fatalf("torn parse = %d segments, want 1", len(got))
	}

	// Sequence gap: a stream starting at seq 1 is unusable.
	if got := checkpoint.ParseSegments(b); len(got) != 0 {
		t.Fatalf("gap parse = %d segments, want 0", len(got))
	}
}

// TestReplaySegmentsCoverage: replay must refuse when segments do not fully
// cover the checkpoint prefix (the fallback trigger), and cut over-run bytes.
func TestReplaySegmentsCoverage(t *testing.T) {
	segs := []*checkpoint.Segment{
		mkSeg(0, 90, bytes.Repeat([]byte{1}, 90), 1),
		mkSeg(1, 180, bytes.Repeat([]byte{2}, 90), 1),
	}
	c := &checkpoint.Checkpoint{}
	c.EntriesLen = 180
	rs, err := checkpoint.ReplaySegments(segs, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs.SidecarPrefix) != 180 || len(rs.Inserts) != 2 {
		t.Fatalf("replay = %d bytes / %d inserts", len(rs.SidecarPrefix), len(rs.Inserts))
	}

	// Under-coverage (checkpoint expects more than segments carry) → error.
	c2 := &checkpoint.Checkpoint{}
	c2.EntriesLen = 270
	if _, err := checkpoint.ReplaySegments(segs, c2); err == nil {
		t.Fatal("under-coverage must error (triggers rebuild fallback)")
	}
}

func TestAppendReadTruncateLocal(t *testing.T) {
	repo := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := checkpoint.AppendSegmentLocal(repo, "b", mkSeg(uint32(i), int64((i+1)*90), bytes.Repeat([]byte{byte(i)}, 90), 1)); err != nil {
			t.Fatal(err)
		}
	}
	segs, err := checkpoint.ReadSegmentsLocal(repo, "b")
	if err != nil || len(segs) != 3 {
		t.Fatalf("read %d segments (err=%v), want 3", len(segs), err)
	}
	// Truncate to 2 (drop the over-run) and confirm.
	if err := checkpoint.TruncateSegmentsLocal(repo, "b", 2); err != nil {
		t.Fatal(err)
	}
	segs, _ = checkpoint.ReadSegmentsLocal(repo, "b")
	if len(segs) != 2 || segs[1].Seq != 1 {
		t.Fatalf("after truncate: %d segments", len(segs))
	}
	// Remove is idempotent.
	if err := checkpoint.RemoveSegmentsLocal(repo, "b"); err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.RemoveSegmentsLocal(repo, "b"); err != nil {
		t.Fatal(err)
	}
}
