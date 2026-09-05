// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package preprocess_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/preprocess"
)

func TestPENormalizerValidPE(t *testing.T) {
	// Build a minimal PE header
	data := make([]byte, 256)
	data[0] = 'M'
	data[1] = 'Z'

	// e_lfanew at 0x3C points to PE signature at offset 0x80
	binary.LittleEndian.PutUint32(data[0x3C:0x40], 0x80)

	// PE signature
	data[0x80] = 'P'
	data[0x81] = 'E'
	data[0x82] = 0
	data[0x83] = 0

	// COFF header starts at 0x84
	// Machine at COFF+0 = 0x84 (amd64)
	binary.LittleEndian.PutUint16(data[0x84:0x86], 0x8664)
	// NumberOfSections at COFF+2 = 0x86
	binary.LittleEndian.PutUint16(data[0x86:0x88], 3)
	// TimeDateStamp at COFF+4 = 0x88
	binary.LittleEndian.PutUint32(data[0x88:0x8C], 0xDEADBEEF)

	// SizeOfOptionalHeader at COFF+16 = 0x94
	binary.LittleEndian.PutUint16(data[0x94:0x96], 112)

	// Optional header starts at COFF+20 = 0x98
	// Magic (PE32+) at optHeader+0
	binary.LittleEndian.PutUint16(data[0x98:0x9A], 0x20b)
	// CheckSum at optHeader+64 = 0xD8
	binary.LittleEndian.PutUint32(data[0xD8:0xDC], 0xCAFEBABE)

	original := make([]byte, len(data))
	copy(original, data)

	norm := &preprocess.PENormalizer{}
	result := norm.Normalize(data)

	// Original must be unmodified
	if !bytes.Equal(data, original) {
		t.Error("original data was modified")
	}

	// TimeDateStamp should be zeroed
	ts := binary.LittleEndian.Uint32(result[0x88:0x8C])
	if ts != 0 {
		t.Errorf("TimeDateStamp: got 0x%X, want 0", ts)
	}

	// CheckSum should be zeroed
	cs := binary.LittleEndian.Uint32(result[0xD8:0xDC])
	if cs != 0 {
		t.Errorf("CheckSum: got 0x%X, want 0", cs)
	}
}

func TestPENormalizerNonPEPassthrough(t *testing.T) {
	data := []byte("this is not a PE file, just regular data that should pass through")

	norm := &preprocess.PENormalizer{}
	result := norm.Normalize(data)

	if !bytes.Equal(result, data) {
		t.Error("non-PE data was modified")
	}
}

func TestPENormalizerOriginalUnmodified(t *testing.T) {
	data := make([]byte, 256)
	data[0] = 'M'
	data[1] = 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:0x40], 0x80)
	data[0x80] = 'P'
	data[0x81] = 'E'
	binary.LittleEndian.PutUint32(data[0x88:0x8C], 0xDEADBEEF)
	binary.LittleEndian.PutUint16(data[0x94:0x96], 112)
	binary.LittleEndian.PutUint32(data[0xD8:0xDC], 0xCAFEBABE)

	original := make([]byte, len(data))
	copy(original, data)

	norm := &preprocess.PENormalizer{}
	norm.Normalize(data)

	if !bytes.Equal(data, original) {
		t.Error("original data was modified by Normalize")
	}
}

func TestPENormalizerTruncatedPESafe(t *testing.T) {
	// MZ header with e_lfanew pointing past end of data
	data := make([]byte, 100)
	data[0] = 'M'
	data[1] = 'Z'
	binary.LittleEndian.PutUint32(data[0x3C:0x40], 200) // points past end

	norm := &preprocess.PENormalizer{}
	result := norm.Normalize(data) // should not panic
	if len(result) != len(data) {
		t.Errorf("unexpected result length: %d", len(result))
	}
}

func TestPENormalizerSmallDataPassthrough(t *testing.T) {
	data := []byte("tiny")

	norm := &preprocess.PENormalizer{}
	result := norm.Normalize(data)

	if !bytes.Equal(result, data) {
		t.Error("small data was modified")
	}
}

func TestNoopNormalizer(t *testing.T) {
	data := []byte("test data")

	norm := &preprocess.NoopNormalizer{}
	result := norm.Normalize(data)

	if !bytes.Equal(result, data) {
		t.Error("noop normalizer modified data")
	}
}

// buildMFTRecord creates a minimal MFT record with a $STANDARD_INFORMATION
// attribute containing the given timestamps at offset tsOffset within the record.
func buildMFTRecord(timestamps [32]byte) []byte {
	rec := make([]byte, 1024)
	// "FILE" magic
	copy(rec[0:4], []byte("FILE"))
	// First attribute offset at byte 20-21
	firstAttr := uint16(56) // typical for MFT
	binary.LittleEndian.PutUint16(rec[20:22], firstAttr)

	pos := int(firstAttr)

	// $STANDARD_INFORMATION attribute (type 0x10)
	attrType := uint32(0x10)
	contentOffset := uint16(24)  // standard resident attr header size
	attrContentLen := uint32(72) // typical $SI content length
	attrLen := uint32(contentOffset) + attrContentLen
	// Align to 8 bytes
	if attrLen%8 != 0 {
		attrLen += 8 - attrLen%8
	}

	binary.LittleEndian.PutUint32(rec[pos:pos+4], attrType)
	binary.LittleEndian.PutUint32(rec[pos+4:pos+8], attrLen)
	rec[pos+8] = 0 // resident
	binary.LittleEndian.PutUint16(rec[pos+20:pos+22], contentOffset)
	binary.LittleEndian.PutUint32(rec[pos+16:pos+20], attrContentLen)

	// Copy timestamps into content area
	copy(rec[pos+int(contentOffset):pos+int(contentOffset)+32], timestamps[:])
	// Fill remaining content with non-zero marker
	for i := pos + int(contentOffset) + 32; i < pos+int(contentOffset)+int(attrContentLen); i++ {
		if i < len(rec) {
			rec[i] = 0xAB
		}
	}

	pos += int(attrLen)

	// End marker
	binary.LittleEndian.PutUint32(rec[pos:pos+4], 0xFFFFFFFF)

	return rec
}

func TestNTFSTimestampNormalizerValidRecord(t *testing.T) {
	var ts [32]byte
	for i := range ts {
		ts[i] = byte(i + 1)
	}

	rec := buildMFTRecord(ts)
	norm := &preprocess.NTFSTimestampNormalizer{}
	result := norm.Normalize(rec)

	// Find the timestamps in the result — they should be zeroed
	firstAttr := int(binary.LittleEndian.Uint16(result[20:22]))
	contentOff := int(binary.LittleEndian.Uint16(result[firstAttr+20 : firstAttr+22]))
	tsStart := firstAttr + contentOff

	for i := tsStart; i < tsStart+32; i++ {
		if result[i] != 0 {
			t.Fatalf("timestamp byte %d not zeroed: got %#x", i-tsStart, result[i])
		}
	}

	// Content after timestamps should be preserved
	if result[tsStart+32] != 0xAB {
		t.Errorf("non-timestamp content modified: got %#x, want 0xAB", result[tsStart+32])
	}
}

func TestNTFSTimestampNormalizerMultipleRecords(t *testing.T) {
	var ts [32]byte
	for i := range ts {
		ts[i] = 0xFF
	}

	// 3 records in a 4096-byte chunk (last 1024 bytes are non-MFT)
	data := make([]byte, 4096)
	for i := 0; i < 3; i++ {
		rec := buildMFTRecord(ts)
		copy(data[i*1024:(i+1)*1024], rec)
	}
	// Last 1024 bytes: not an MFT record
	for i := 3072; i < 4096; i++ {
		data[i] = 0xDD
	}

	norm := &preprocess.NTFSTimestampNormalizer{}
	result := norm.Normalize(data)

	// Each of the 3 records should have timestamps zeroed
	for r := 0; r < 3; r++ {
		off := r * 1024
		firstAttr := int(binary.LittleEndian.Uint16(result[off+20 : off+22]))
		contentOff := int(binary.LittleEndian.Uint16(result[off+firstAttr+20 : off+firstAttr+22]))
		tsStart := off + firstAttr + contentOff

		for i := tsStart; i < tsStart+32; i++ {
			if result[i] != 0 {
				t.Fatalf("record %d: timestamp byte %d not zeroed", r, i-tsStart)
			}
		}
	}

	// Non-MFT region should be unchanged
	for i := 3072; i < 4096; i++ {
		if result[i] != 0xDD {
			t.Fatalf("non-MFT byte %d modified: got %#x", i, result[i])
		}
	}
}

func TestNTFSTimestampNormalizerNoMagic(t *testing.T) {
	data := make([]byte, 2048)
	for i := range data {
		data[i] = byte(i & 0xFF)
	}

	norm := &preprocess.NTFSTimestampNormalizer{}
	result := norm.Normalize(data)

	// Should return same slice (not a copy) when no FILE magic found
	if &result[0] != &data[0] {
		t.Error("expected same slice pointer for non-MFT data")
	}
}

func TestNTFSTimestampNormalizerOriginalUnmodified(t *testing.T) {
	var ts [32]byte
	for i := range ts {
		ts[i] = 0xFF
	}

	rec := buildMFTRecord(ts)
	original := make([]byte, len(rec))
	copy(original, rec)

	norm := &preprocess.NTFSTimestampNormalizer{}
	norm.Normalize(rec)

	if !bytes.Equal(rec, original) {
		t.Error("original data was modified")
	}
}

func TestNTFSTimestampNormalizerMalformed(t *testing.T) {
	tests := []struct {
		name   string
		modify func([]byte)
	}{
		{
			name: "bad first attribute offset",
			modify: func(rec []byte) {
				binary.LittleEndian.PutUint16(rec[20:22], 2000) // past end
			},
		},
		{
			name: "zero attribute length",
			modify: func(rec []byte) {
				firstAttr := int(binary.LittleEndian.Uint16(rec[20:22]))
				binary.LittleEndian.PutUint32(rec[firstAttr+4:firstAttr+8], 0) // zero length
			},
		},
		{
			name: "attribute length past record",
			modify: func(rec []byte) {
				firstAttr := int(binary.LittleEndian.Uint16(rec[20:22]))
				binary.LittleEndian.PutUint32(rec[firstAttr+4:firstAttr+8], 2000) // past end
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ts [32]byte
			for i := range ts {
				ts[i] = 0xFF
			}
			rec := buildMFTRecord(ts)
			tt.modify(rec)

			norm := &preprocess.NTFSTimestampNormalizer{}
			// Should not panic
			norm.Normalize(rec)
		})
	}
}

func TestCompositeNormalizer(t *testing.T) {
	// Build data that has both PE and MFT signatures
	// Use separate chunks for simplicity — just verify both normalizers are called
	data := make([]byte, 2048)

	// Put an MFT record
	var ts [32]byte
	for i := range ts {
		ts[i] = 0xFF
	}
	rec := buildMFTRecord(ts)
	copy(data[0:1024], rec)

	composite := &preprocess.CompositeNormalizer{
		Normalizers: []preprocess.Normalizer{
			&preprocess.PENormalizer{},
			&preprocess.NTFSTimestampNormalizer{},
		},
	}

	result := composite.Normalize(data)

	// NTFS timestamps should be zeroed
	firstAttr := int(binary.LittleEndian.Uint16(result[20:22]))
	contentOff := int(binary.LittleEndian.Uint16(result[firstAttr+20 : firstAttr+22]))
	tsStart := firstAttr + contentOff
	for i := tsStart; i < tsStart+32; i++ {
		if result[i] != 0 {
			t.Fatalf("timestamp byte %d not zeroed after composite", i-tsStart)
		}
	}
}

func TestCompositeNormalizerEmpty(t *testing.T) {
	data := []byte("hello world")
	composite := &preprocess.CompositeNormalizer{}
	result := composite.Normalize(data)

	if !bytes.Equal(result, data) {
		t.Error("empty composite normalizer modified data")
	}
}
