// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package preprocess

import "encoding/binary"

// Normalizer transforms chunk data before hashing to improve dedup across
// semantically-identical-but-byte-different content.
type Normalizer interface {
	// Normalize returns a normalized copy of data for hashing purposes.
	// The original data is never modified.
	Normalize(data []byte) []byte
}

// PENormalizer zeros PE header timestamps and checksums to improve dedup
// of Windows executables across builds.
type PENormalizer struct{}

// Normalize zeros TimeDateStamp and CheckSum fields in any PE headers
// found within the data. Returns a copy; the original is unmodified.
func (n *PENormalizer) Normalize(data []byte) []byte {
	if len(data) < 64 {
		return data
	}

	// Look for PE signature: "MZ" at the start
	// PE headers can appear at chunk boundaries, so we also scan for
	// the PE\0\0 signature anywhere in the chunk.
	out := make([]byte, len(data))
	copy(out, data)

	// Try MZ header at offset 0
	if len(out) >= 64 && out[0] == 'M' && out[1] == 'Z' {
		normalizePEFromMZ(out)
	}

	// Also scan for embedded PE\0\0 signatures
	for i := 0; i+4 <= len(out); i++ {
		if out[i] == 'P' && out[i+1] == 'E' && out[i+2] == 0 && out[i+3] == 0 {
			normalizePEHeader(out, i)
		}
	}

	return out
}

// normalizePEFromMZ handles a chunk that starts with an MZ header.
func normalizePEFromMZ(data []byte) {
	if len(data) < 64 {
		return
	}

	// e_lfanew at offset 0x3C points to PE signature
	peOffset := int(binary.LittleEndian.Uint32(data[0x3C:0x40]))
	if peOffset < 0 || peOffset+4 > len(data) {
		return
	}

	// Verify PE\0\0 signature
	if data[peOffset] != 'P' || data[peOffset+1] != 'E' || data[peOffset+2] != 0 || data[peOffset+3] != 0 {
		return
	}

	normalizePEHeader(data, peOffset)
}

// validCOFFMachine reports whether m is a known IMAGE_FILE_MACHINE_* value.
func validCOFFMachine(m uint16) bool {
	switch m {
	case 0x014c, // i386
		0x0200, // ia64
		0x01c0, // arm
		0x01c4, // armnt (thumb-2)
		0xaa64, // arm64
		0x8664: // amd64
		return true
	}
	return false
}

// isPlausiblePEHeader validates the COFF header fields following a PE\0\0
// signature. The 4-byte signature alone matches random data roughly once per
// 4 GiB, which is routine at backup scale — and zeroing "timestamp" bytes in
// arbitrary data creates hash collisions between genuinely different chunks
// (the pipeline hashes normalized bytes but stores originals, so a collision
// silently restores the wrong bytes). Requiring a known Machine, a sane
// section count, and a valid optional-header magic pushes the false-positive
// probability from ~2^-32 to negligible. Truncated headers (e.g. a PE header
// split across a chunk boundary) fail validation and are left unnormalized:
// a missed dedup opportunity, never corruption.
func isPlausiblePEHeader(data []byte, peOffset int) bool {
	coffOffset := peOffset + 4
	if coffOffset+20 > len(data) {
		return false
	}
	machine := binary.LittleEndian.Uint16(data[coffOffset : coffOffset+2])
	if !validCOFFMachine(machine) {
		return false
	}
	numSections := binary.LittleEndian.Uint16(data[coffOffset+2 : coffOffset+4])
	if numSections == 0 || numSections > 96 { // PE spec limit
		return false
	}
	sizeOfOptHdr := binary.LittleEndian.Uint16(data[coffOffset+16 : coffOffset+18])
	if sizeOfOptHdr >= 2 {
		optOffset := coffOffset + 20
		if optOffset+2 > len(data) {
			return false
		}
		magic := binary.LittleEndian.Uint16(data[optOffset : optOffset+2])
		if magic != 0x10b && magic != 0x20b && magic != 0x107 { // PE32, PE32+, ROM
			return false
		}
	}
	return true
}

// normalizePEHeader zeros volatile fields in a PE header starting at peOffset.
// The caller is expected to have verified the PE\0\0 signature; the COFF
// fields are validated here before any bytes are modified.
func normalizePEHeader(data []byte, peOffset int) {
	if !isPlausiblePEHeader(data, peOffset) {
		return
	}

	// COFF header starts at peOffset+4
	coffOffset := peOffset + 4

	// TimeDateStamp is at COFF header offset 4, size 4
	tsOffset := coffOffset + 4
	if tsOffset+4 <= len(data) {
		data[tsOffset] = 0
		data[tsOffset+1] = 0
		data[tsOffset+2] = 0
		data[tsOffset+3] = 0
	}

	// To find CheckSum, we need to get to the optional header.
	// SizeOfOptionalHeader is at COFF offset 16.
	sizeOfOptHdrOffset := coffOffset + 16
	if sizeOfOptHdrOffset+2 > len(data) {
		return
	}
	sizeOfOptHdr := binary.LittleEndian.Uint16(data[sizeOfOptHdrOffset : sizeOfOptHdrOffset+2])
	if sizeOfOptHdr == 0 {
		return
	}

	// Optional header starts after the 20-byte COFF header
	optOffset := coffOffset + 20

	// CheckSum is at optional header offset 64; the full 4-byte field must
	// lie within the declared optional header.
	csOffset := optOffset + 64
	if csOffset+4 <= len(data) && csOffset+4 <= optOffset+int(sizeOfOptHdr) {
		data[csOffset] = 0
		data[csOffset+1] = 0
		data[csOffset+2] = 0
		data[csOffset+3] = 0
	}
}

// NTFSTimestampNormalizer zeros the four 8-byte timestamps in
// $STANDARD_INFORMATION (0x10) attributes of MFT records. These timestamps
// differ across machines even for identical files, hurting cross-machine dedup.
// Only the hash input is modified; original data is stored faithfully.
type NTFSTimestampNormalizer struct{}

const mftRecordSize = 1024

// Normalize scans for MFT "FILE" records at 1024-byte boundaries and zeros
// the 32 bytes of timestamps in each $STANDARD_INFORMATION attribute.
func (n *NTFSTimestampNormalizer) Normalize(data []byte) []byte {
	// Quick scan: any "FILE" magic at 1024-byte boundaries?
	found := false
	for off := 0; off+4 <= len(data); off += mftRecordSize {
		if data[off] == 'F' && data[off+1] == 'I' && data[off+2] == 'L' && data[off+3] == 'E' {
			found = true
			break
		}
	}
	if !found {
		return data
	}

	out := make([]byte, len(data))
	copy(out, data)

	for off := 0; off+mftRecordSize <= len(out); off += mftRecordSize {
		if out[off] != 'F' || out[off+1] != 'I' || out[off+2] != 'L' || out[off+3] != 'E' {
			continue
		}
		normalizeMFTRecord(out[off : off+mftRecordSize])
	}

	return out
}

// normalizeMFTRecord zeros timestamps in $STANDARD_INFORMATION within a
// single 1024-byte MFT record.
func normalizeMFTRecord(rec []byte) {
	if len(rec) < 22 {
		return
	}

	// First attribute offset is at bytes 20-21 (uint16 LE).
	firstAttr := int(binary.LittleEndian.Uint16(rec[20:22]))
	if firstAttr < 22 || firstAttr >= len(rec) {
		return
	}

	// Walk the attribute list.
	pos := firstAttr
	for pos+8 <= len(rec) {
		attrType := binary.LittleEndian.Uint32(rec[pos : pos+4])
		if attrType == 0xFFFFFFFF {
			break // end marker
		}
		attrLen := int(binary.LittleEndian.Uint32(rec[pos+4 : pos+8]))
		if attrLen <= 0 || pos+attrLen > len(rec) {
			break // malformed
		}

		if attrType == 0x10 { // $STANDARD_INFORMATION
			// Non-resident flag at offset 8.
			if pos+9 > len(rec) || rec[pos+8] != 0 {
				// Non-resident or truncated — skip.
				pos += attrLen
				continue
			}
			// Content offset at attr+20 (uint16 LE).
			if pos+22 > len(rec) {
				pos += attrLen
				continue
			}
			contentOff := int(binary.LittleEndian.Uint16(rec[pos+20 : pos+22]))
			tsStart := pos + contentOff
			tsEnd := tsStart + 32 // 4 timestamps × 8 bytes
			if tsStart >= pos+attrLen || tsEnd > pos+attrLen || tsEnd > len(rec) {
				pos += attrLen
				continue
			}
			clear(rec[tsStart:tsEnd])
		}

		pos += attrLen
	}
}

// CompositeNormalizer chains multiple normalizers, piping the output of each
// into the next.
type CompositeNormalizer struct {
	Normalizers []Normalizer
}

func (c *CompositeNormalizer) Normalize(data []byte) []byte {
	for _, n := range c.Normalizers {
		data = n.Normalize(data)
	}
	return data
}

// NoopNormalizer passes data through unchanged.
type NoopNormalizer struct{}

func (n *NoopNormalizer) Normalize(data []byte) []byte {
	return data
}
