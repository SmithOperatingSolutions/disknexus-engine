// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

// dnm_spec_drift_test.go — the specification is checked against the bytes.
//
// docs/manifest_binary_format.md documents the .dnm header and section index
// down to the byte, and third-party readers and recovery tools are built from
// it. It went years describing a 4-byte SectionIndexOffset the writer has never
// written and a 28-byte section index entry that is 36, so anything built from
// it landed on garbage past 4 GiB and misparsed every manifest in existence.
//
// This test makes that class of drift impossible to land. It reads the spec's
// own tables, encodes a REAL manifest, walks the file the writer produced, and
// reconstructs the header and section index from the documented offsets and
// widths. A byte that disagrees is a spec a reader cannot trust.
//
// The division of labour is deliberate: the SPEC supplies layout only (offset,
// width, order) and the CODE supplies values and structure (where sections
// actually start, how long they actually are, what the writer actually stored).
// Nothing here restates a layout constant from dnm.go — restating them would
// make the guard agree with the code by construction and catch nothing.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Inside the engine module (engine/docs), not the product's specs/: the
// split repository must carry the spec this test holds the code to.
const specRelPath = "../../docs/manifest_binary_format.md"

// --- the spec side: parse the documented tables ----------------------------

// specField is one documented row: "12      8     SectionIndexOffset: uint64 LE".
type specField struct {
	offset int
	size   int
	name   string
	line   string
}

// specLayout is everything this guard reads out of the spec.
type specLayout struct {
	header       []specField
	indexEntry   []specField
	entrySize    int // the "(N bytes each)" figure in the section index heading
	entryRecSize int // the "Total: N bytes" figure in the ENTRIES section
}

var (
	specRowRe       = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+([A-Za-z][A-Za-z0-9_]*)`)
	specEntrySizeRe = regexp.MustCompile(`###\s+Section Index Entry\s+\((\d+)\s+bytes each\)`)
	specRecSizeRe   = regexp.MustCompile(`(?m)^Total:\s+(\d+)\s+bytes`)
)

// parseSpecLayout extracts the documented layout from the spec's markdown.
//
// Tables live in the fenced block that follows their heading. A heading whose
// block yields no rows is reported as an error rather than as an empty table:
// an unreadable spec must fail loudly, never pass by scanning nothing (#8 of
// docs/TESTING.md).
func parseSpecLayout(md string) (specLayout, error) {
	var sl specLayout
	var err error

	if sl.header, err = specRowsAfter(md, "### File Header"); err != nil {
		return sl, fmt.Errorf("File Header table: %w", err)
	}
	if sl.indexEntry, err = specRowsAfter(md, "### Section Index Entry"); err != nil {
		return sl, fmt.Errorf("Section Index Entry table: %w", err)
	}

	m := specEntrySizeRe.FindStringSubmatch(md)
	if m == nil {
		return sl, fmt.Errorf(`no "### Section Index Entry (N bytes each)" heading found`)
	}
	if sl.entrySize, err = strconv.Atoi(m[1]); err != nil {
		return sl, fmt.Errorf("section index entry size %q: %w", m[1], err)
	}

	m = specRecSizeRe.FindStringSubmatch(md)
	if m == nil {
		return sl, fmt.Errorf(`no "Total: N bytes" line found for the ENTRIES record`)
	}
	if sl.entryRecSize, err = strconv.Atoi(m[1]); err != nil {
		return sl, fmt.Errorf("entries record size %q: %w", m[1], err)
	}
	return sl, nil
}

// specRowsAfter returns the offset/size/name rows of the first fenced code
// block following the given heading prefix.
func specRowsAfter(md, heading string) ([]specField, error) {
	lines := strings.Split(md, "\n")
	i := 0
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], heading) {
			break
		}
	}
	if i == len(lines) {
		return nil, fmt.Errorf("heading %q not found", heading)
	}
	for ; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "```") {
			break
		}
	}
	if i == len(lines) {
		return nil, fmt.Errorf("heading %q is not followed by a code block", heading)
	}
	var out []specField
	for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
		m := specRowRe.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		off, _ := strconv.Atoi(m[1])
		size, _ := strconv.Atoi(m[2])
		out = append(out, specField{offset: off, size: size, name: m[3], line: strings.TrimSpace(lines[i])})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("heading %q has a code block with no parseable rows", heading)
	}
	return out, nil
}

// --- the code side: what the writer actually wrote -------------------------

// authVal is the value the guard requires at a documented field, either a
// number to be encoded at the documented width or exact raw bytes.
type authVal struct {
	num     uint64
	raw     []byte
	numeric bool
}

func num(v uint64) authVal { return authVal{num: v, numeric: true} }
func raw(b []byte) authVal { return authVal{raw: b} }

// reserved means "zero bytes, however many the spec documents".
func reserved() authVal { return authVal{} }

// dnmFacts is what a real .dnm file says about itself, obtained by writing one
// and walking it — not by reading dnm.go's constants.
type dnmFacts struct {
	data []byte

	headerLen      int // where METADATA begins == the header's length
	metaOffset     uint64
	metaLen        uint64
	catalogOffset  uint64
	catalogLen     uint64
	catalogCount   uint64
	entriesOffset  uint64
	entriesLen     uint64
	entriesCount   uint64
	entryRecordLen uint64 // measured, by writing one more entry
	indexOffset    uint64
	indexRegion    []byte
	sectionCount   uint64
}

// writeSampleManifest writes a manifest with nEntries chunk records and returns
// the bytes on disk. The catalog deliberately carries extents AND inline data
// so the record framing exercised is the real one.
func writeSampleManifest(t *testing.T, nEntries int) (*Backup, []byte) {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "manifests"), 0o755); err != nil {
		t.Fatalf("creating manifests dir: %v", err)
	}
	b := &Backup{
		BackupID:     "8f14e45f-ea8f-4f0a-9c1e-2b3d4a5b6c7d",
		SourceVolume: `\\?\Volume{8f14e45f-0000-0000-0000-000000000000}\`,
		BackupType:   "full",
		BackupMode:   "file",
		Timestamp:    time.Unix(1755000000, 123456789).UTC(),
		SectorSize:   512,
		ClusterSize:  4096,
		TotalBytes:   1 << 30,
		Duration:     "1m12.4s",
		SourcePaths:  []string{"C:/data", "C:/users"},
		WrappedDEK:   bytes.Repeat([]byte{0x5a}, 92),
		FileCatalog: []FileEntry{
			{
				Path: "data/report.txt", Size: 17, Mode: 0o644,
				ModTime:       time.Unix(1754000000, 0).UTC(),
				StreamLength:  17,
				VolumeExtents: []VolumeExtent{{FileOffset: 0, VolumeOffset: 4096, Length: 17}},
				InlineData:    []byte("resident content!"),
			},
			{Path: "data", Mode: 0o755, IsDir: true},
			{Path: "pagefile.sys", Size: 1 << 20, IsExcluded: true},
		},
	}
	for i := range nEntries {
		var h [32]byte
		h[0], h[31] = byte(i+1), 0xEE
		b.Entries = append(b.Entries, Entry{VolumeOffset: int64(i) * 65536, ChunkHash: h, ChunkLength: 65536})
	}
	if err := saveDNM(repo, b); err != nil {
		t.Fatalf("writing sample manifest: %v", err)
	}
	data, err := os.ReadFile(DNMPath(repo, b.BackupID))
	if err != nil {
		t.Fatalf("reading sample manifest: %v", err)
	}
	return b, data
}

// observeDNM walks a written manifest and reports where everything is.
//
// The header length and the per-entry record size are MEASURED, not assumed:
// METADATA is located by finding the encoder's own output inside the file, and
// the record size falls out of the size difference between two manifests whose
// only difference is one extra chunk entry.
func observeDNM(t *testing.T) dnmFacts {
	t.Helper()
	const nEntries = 4
	b, data := writeSampleManifest(t, nEntries)
	_, bigger := writeSampleManifest(t, nEntries+1)

	f := dnmFacts{data: data}
	f.entryRecordLen = uint64(len(bigger) - len(data))
	if f.entryRecordLen == 0 {
		t.Fatalf("one extra chunk entry changed the file size by 0 bytes — the fixture is not exercising ENTRIES")
	}

	meta := encodeMetadata(b)
	idx := bytes.Index(data, meta)
	if idx < 0 {
		t.Fatalf("the METADATA the encoder produces does not appear in the file the writer wrote")
	}
	if bytes.Index(data[idx+1:], meta) >= 0 {
		t.Fatalf("METADATA appears more than once in the file; cannot locate sections unambiguously")
	}
	f.headerLen = idx
	f.metaOffset = uint64(idx)
	f.metaLen = uint64(len(meta))

	f.catalogOffset = f.metaOffset + f.metaLen
	for _, fe := range b.FileCatalog {
		f.catalogLen += 4 + uint64(len(encodeFileEntry(fe)))
		f.catalogCount++
	}
	f.entriesOffset = f.catalogOffset + f.catalogLen
	f.entriesCount = uint64(len(b.Entries))
	f.entriesLen = f.entriesCount * f.entryRecordLen
	f.indexOffset = f.entriesOffset + f.entriesLen
	if f.indexOffset >= uint64(len(data)) {
		t.Fatalf("walked past the end of the file: sections end at %d in a %d-byte manifest", f.indexOffset, len(data))
	}
	f.indexRegion = data[f.indexOffset:]
	// The walk located exactly these three sections; every one must be indexed.
	f.sectionCount = 3
	return f
}

// --- the comparison --------------------------------------------------------

// checkSpec reconstructs the header and section index from the spec's layout
// and reports every way the result differs from the real file. It is a pure
// function of (spec text, observed file) so a deliberately wrong spec can be
// fed to it as a positive control.
func checkSpec(md string, f dnmFacts) ([]string, error) {
	sl, err := parseSpecLayout(md)
	if err != nil {
		return nil, err
	}
	var bad []string

	// --- file header ---
	hdrAuth := map[string]authVal{
		"Magic":              raw([]byte(dnmMagic)),
		"Version":            num(uint64(dnmVersion)),
		"Flags":              num(0),
		"SectionIndexOffset": num(f.indexOffset),
		"SectionIndexCount":  num(f.sectionCount),
		"FileCRC32":          num(0), // never computed; see the spec's note
		"Reserved":           reserved(),
	}
	want, msgs := buildFromSpec(sl.header, hdrAuth, "file header")
	bad = append(bad, msgs...)
	if len(want) != f.headerLen {
		bad = append(bad, fmt.Sprintf(
			"file header: the spec's fields span %d bytes but the writer emits a %d-byte header "+
				"(METADATA begins at offset %d) — a reader would start every section at the wrong place",
			len(want), f.headerLen, f.headerLen))
	} else if got := f.data[:f.headerLen]; !bytes.Equal(want, got) {
		bad = append(bad, fmt.Sprintf(
			"file header: a reader built from the spec reads\n    %x\nbut the writer wrote\n    %x\n  %s",
			want, got, diffFields(sl.header, want, got)))
	}

	// --- section index ---
	if sl.entrySize*int(f.sectionCount) != len(f.indexRegion) {
		bad = append(bad, fmt.Sprintf(
			"section index: the spec says %d bytes per entry, so %d sections would occupy %d bytes, "+
				"but the writer wrote %d — a reader lands mid-entry on the second section and "+
				"seeks to a garbage offset",
			sl.entrySize, f.sectionCount, sl.entrySize*int(f.sectionCount), len(f.indexRegion)))
	} else {
		sections := []struct {
			typ                   uint64
			offset, length, count uint64
		}{
			{1, f.metaOffset, f.metaLen, 1},
			{2, f.catalogOffset, f.catalogLen, f.catalogCount},
			{3, f.entriesOffset, f.entriesLen, f.entriesCount},
		}
		var full []byte
		for _, s := range sections {
			auth := map[string]authVal{
				"SectionType":   num(s.typ),
				"Reserved":      reserved(),
				"SectionOffset": num(s.offset),
				"SectionLength": num(s.length),
				"RecordCount":   num(s.count),
				"SectionCRC32":  num(0), // never computed
			}
			entry, msgs := buildFromSpec(sl.indexEntry, auth, fmt.Sprintf("section index entry type %d", s.typ))
			bad = append(bad, msgs...)
			if len(entry) != sl.entrySize {
				bad = append(bad, fmt.Sprintf(
					"section index entry type %d: the spec's fields span %d bytes but its heading says %d bytes each",
					s.typ, len(entry), sl.entrySize))
				entry = make([]byte, sl.entrySize)
			}
			full = append(full, entry...)
		}
		if !bytes.Equal(full, f.indexRegion) {
			bad = append(bad, fmt.Sprintf(
				"section index: a reader built from the spec reads\n    %x\nbut the writer wrote\n    %x",
				full, f.indexRegion))
		}
	}

	// --- ENTRIES record size ---
	if uint64(sl.entryRecSize) != f.entryRecordLen {
		bad = append(bad, fmt.Sprintf(
			"ENTRIES: the spec says a chunk record is %d bytes; adding one entry grew the file by %d — "+
				"a reader would decode every chunk hash after the first from the wrong offset",
			sl.entryRecSize, f.entryRecordLen))
	}
	return bad, nil
}

// buildFromSpec lays the authoritative values out exactly where and how wide
// the spec says, and reports rows the spec makes impossible to honour.
func buildFromSpec(rows []specField, auth map[string]authVal, what string) ([]byte, []string) {
	var bad []string
	end := 0
	for _, r := range rows {
		if r.offset+r.size > end {
			end = r.offset + r.size
		}
	}
	buf := make([]byte, end)
	covered := make([]bool, end)
	for _, r := range rows {
		v, ok := auth[r.name]
		if !ok {
			bad = append(bad, fmt.Sprintf("%s: spec documents a field %q the code does not write (%s)", what, r.name, r.line))
			continue
		}
		for i := r.offset; i < r.offset+r.size; i++ {
			if covered[i] {
				bad = append(bad, fmt.Sprintf("%s: byte %d is claimed by two fields; %q overlaps", what, i, r.name))
			}
			covered[i] = true
		}
		switch {
		case !v.numeric && len(v.raw) == 0:
			// A reserved run: zeroes of whatever width the spec documents.
		case !v.numeric:
			if len(v.raw) != r.size {
				bad = append(bad, fmt.Sprintf("%s: spec gives %q %d bytes; the writer stores %d (%s)",
					what, r.name, r.size, len(v.raw), r.line))
				continue
			}
			copy(buf[r.offset:], v.raw)
		default:
			switch r.size {
			case 1:
				buf[r.offset] = byte(v.num)
			case 2:
				binary.LittleEndian.PutUint16(buf[r.offset:], uint16(v.num))
			case 4:
				binary.LittleEndian.PutUint32(buf[r.offset:], uint32(v.num))
			case 8:
				binary.LittleEndian.PutUint64(buf[r.offset:], v.num)
			default:
				bad = append(bad, fmt.Sprintf("%s: spec gives %q an un-encodable width of %d bytes (%s)",
					what, r.name, r.size, r.line))
			}
		}
	}
	for i, c := range covered {
		if !c {
			bad = append(bad, fmt.Sprintf("%s: byte %d is documented by no field — the spec's rows do not tile the record", what, i))
			break
		}
	}
	return buf, bad
}

// diffFields names the documented fields whose bytes disagree, so the failure
// says which row of the spec to fix rather than only that two hex strings differ.
func diffFields(rows []specField, want, got []byte) string {
	var names []string
	for _, r := range rows {
		if r.offset+r.size > len(want) || r.offset+r.size > len(got) {
			continue
		}
		if !bytes.Equal(want[r.offset:r.offset+r.size], got[r.offset:r.offset+r.size]) {
			names = append(names, fmt.Sprintf("%s (spec row: %s)", r.name, r.line))
		}
	}
	if len(names) == 0 {
		return "(no single documented field accounts for it)"
	}
	return "disagreeing fields: " + strings.Join(names, "; ")
}

// readSpecLF reads the spec with line endings normalized to LF. Git on the
// Windows runners checks .md files out with CRLF (no .gitattributes rule pins
// them), and both halves of this guard join expectations with "\n": the table
// parser's line splits and — the way this was actually caught — the positive
// control's multi-line needle, which stopped matching and correctly reported
// "changed nothing ... proves nothing" on windows-latest while Linux was
// green. Normalizing at the single read site fixes parser and controls alike.
func readSpecLF(t *testing.T) string {
	t.Helper()
	md, err := os.ReadFile(specRelPath)
	if err != nil {
		t.Fatalf("reading %s: %v", specRelPath, err)
	}
	return strings.ReplaceAll(string(md), "\r\n", "\n")
}

// --- the tests -------------------------------------------------------------

// TestSpecMatchesDNMLayout is the drift guard: docs/manifest_binary_format.md
// must describe the bytes the writer actually produces.
func TestSpecMatchesDNMLayout(t *testing.T) {
	md := readSpecLF(t)
	f := observeDNM(t)
	bad, err := checkSpec(md, f)
	if err != nil {
		t.Fatalf("the .dnm layout could not be read out of %s: %v\n"+
			"An unparseable spec fails this guard on purpose — it is the state in which "+
			"the documented format can drift from the code unnoticed.", specRelPath, err)
	}
	for _, m := range bad {
		t.Errorf("docs/manifest_binary_format.md disagrees with the .dnm the writer produces:\n  %s", m)
	}
}

// TestSpecDriftGuardCatchesAWrongTable is the positive control: the guard must
// distinguish a correct spec from a wrong one. Without it, a parser that
// silently matched nothing would pass against any spec at all — including the
// one that documented a 4-byte SectionIndexOffset and a 28-byte index entry.
func TestSpecDriftGuardCatchesAWrongTable(t *testing.T) {
	md := readSpecLF(t)
	f := observeDNM(t)

	// Control: the shipped spec passes. Asserted first, on the same fixture,
	// so a mutant failing for an unrelated reason cannot be mistaken for a
	// working guard.
	if bad, err := checkSpec(md, f); err != nil || len(bad) > 0 {
		t.Fatalf("positive control failed — the shipped spec must pass before a mutant proves anything: err=%v, mismatches=%v", err, bad)
	}

	mutants := []struct {
		name, old, new, wantSubstr string
	}{
		{
			name:       "section index entry size understated",
			old:        "### Section Index Entry (36 bytes each)",
			new:        "### Section Index Entry (28 bytes each)",
			wantSubstr: "28 bytes per entry",
		},
		{
			// The table this spec actually shipped with for years.
			name: "the historical 4-byte SectionIndexOffset table",
			old: "12      8     SectionIndexOffset: uint64 LE  (byte offset from file start; 0 = see trailer)\n" +
				"20      4     SectionIndexCount: uint32 LE   (number of section index entries; always 3)\n" +
				"24      4     FileCRC32: uint32 LE           (reserved; never computed)\n" +
				"28      4     Reserved: [4]byte",
			new: "12      4     SectionIndexOffset: uint32 LE\n" +
				"16      4     SectionIndexCount: uint32 LE\n" +
				"20      4     FileCRC32: uint32 LE\n" +
				"24      8     Reserved: [8]byte",
			wantSubstr: "SectionIndexCount",
		},
		{
			name:       "SectionIndexOffset narrowed, leaving the record untiled",
			old:        "12      8     SectionIndexOffset",
			new:        "12      4     SectionIndexOffset",
			wantSubstr: "do not tile the record",
		},
		{
			name:       "ENTRIES record size wrong",
			old:        "Total:         45 bytes",
			new:        "Total:         44 bytes",
			wantSubstr: "chunk record is 44 bytes",
		},
	}
	for _, m := range mutants {
		t.Run(m.name, func(t *testing.T) {
			mutated := strings.Replace(md, m.old, m.new, 1)
			if mutated == md {
				t.Fatalf("mutant %q changed nothing — the spec no longer contains %q, so this control proves nothing", m.name, m.old)
			}
			bad, err := checkSpec(mutated, f)
			if err != nil {
				t.Fatalf("mutated spec failed to parse (%v); a parse error is not the mismatch this control is checking for", err)
			}
			if len(bad) == 0 {
				t.Fatalf("the guard accepted a spec that says %q — it cannot tell a correct table from a wrong one", m.new)
			}
			if !strings.Contains(strings.Join(bad, "\n"), m.wantSubstr) {
				t.Fatalf("the guard rejected the mutant but not for the documented reason; want a message containing %q, got:\n%s",
					m.wantSubstr, strings.Join(bad, "\n"))
			}
		})
	}
}
