// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic disk: 1.25 GiB virtual so qcow2 needs two L1 entries and
// VHD hundreds of BAT entries, with data runs chosen to cross every
// boundary the formats care about. Only ~1 MiB is non-zero.
const testDiskSize = int64(1280) << 20

type run struct {
	off int64
	len int
}

var testRuns = []run{
	{0, 512},                     // boot sector
	{4096 - 100, 300},            // straddles a 4 KiB page edge
	{2<<20 - 1000, 3000},         // straddles a VHD block edge
	{64<<20 - 512, 1024},         // straddles a qcow2 cluster edge
	{512<<20 - 4096, 8192},       // straddles the qcow2 L2 span (512 MiB)
	{700 << 20, 200 << 10},       // 200 KiB run in the second L1 entry
	{testDiskSize - 512, 512},    // last sector
	{testDiskSize - 3<<20, 4096}, // page-aligned in the last VHD block
}

// expectedImage is what every reader must return: the runs over zeros.
type expectedImage struct {
	data map[int64][]byte
}

func makeExpected() *expectedImage {
	r := rand.New(rand.NewSource(456))
	e := &expectedImage{data: map[int64][]byte{}}
	for _, ru := range testRuns {
		b := make([]byte, ru.len)
		r.Read(b)
		// Make sure no page of a run is accidentally all zero.
		for i := 0; i < len(b); i += 4096 {
			b[i] |= 1
		}
		e.data[ru.off] = b
	}
	return e
}

func (e *expectedImage) readAt(p []byte, off int64) {
	for i := range p {
		p[i] = 0
	}
	for ro, b := range e.data {
		lo, hi := ro, ro+int64(len(b))
		if hi <= off || lo >= off+int64(len(p)) {
			continue
		}
		s := lo
		if s < off {
			s = off
		}
		t := hi
		if t > off+int64(len(p)) {
			t = off + int64(len(p))
		}
		copy(p[s-off:t-off], b[s-ro:t-ro])
	}
}

// writeTestImage writes the runs through w the way the restore engine
// does: in 64 KiB pieces that include the surrounding zeros, plus some
// all-zero writes that must cost nothing.
func writeTestImage(t *testing.T, w Writer, e *expectedImage) {
	t.Helper()
	if err := w.Truncate(testDiskSize); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	writeTestRuns(t, w, e)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func writeTestRuns(t *testing.T, w Writer, e *expectedImage) {
	t.Helper()
	for _, ru := range testRuns {
		start := ru.off &^ 0xffff
		end := (ru.off + int64(ru.len) + 0xffff) &^ 0xffff
		if end > testDiskSize {
			end = testDiskSize
		}
		for o := start; o < end; o += 64 << 10 {
			n := int64(64 << 10)
			if o+n > end {
				n = end - o
			}
			buf := make([]byte, n)
			e.readAt(buf, o)
			if _, err := w.WriteAt(buf, o); err != nil {
				t.Fatalf("WriteAt(%d): %v", o, err)
			}
		}
	}
	// Zeros over a region nothing else touches: no allocation may result.
	zeros := make([]byte, 1<<20)
	for o := int64(900) << 20; o < int64(1000)<<20; o += int64(len(zeros)) {
		if _, err := w.WriteAt(zeros, o); err != nil {
			t.Fatalf("zero WriteAt(%d): %v", o, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
}

// compareImage reads the whole virtual disk back through the reader in
// odd-sized pieces and compares every byte to the expectation.
func compareImage(t *testing.T, r io.ReaderAt, e *expectedImage) {
	t.Helper()
	if sized, ok := r.(interface{ Size() int64 }); ok && sized.Size() != testDiskSize {
		t.Fatalf("size %d, want %d", sized.Size(), testDiskSize)
	}
	want := sha256.New()
	got := sha256.New()
	const piece = 1<<20 + 4096 + 7 // never aligned to anything
	buf := make([]byte, piece)
	exp := make([]byte, piece)
	for off := int64(0); off < testDiskSize; off += piece {
		n := int64(piece)
		if off+n > testDiskSize {
			n = testDiskSize - off
		}
		if _, err := r.ReadAt(buf[:n], off); err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		e.readAt(exp[:n], off)
		if !bytes.Equal(buf[:n], exp[:n]) {
			for i := int64(0); i < n; i++ {
				if buf[i] != exp[i] {
					t.Fatalf("byte %d differs: got %02x want %02x", off+i, buf[i], exp[i])
				}
			}
		}
		got.Write(buf[:n])
		want.Write(exp[:n])
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatalf("digest mismatch")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func TestRoundTripEveryFormat(t *testing.T) {
	e := makeExpected()
	cases := []struct {
		name   string
		format Format
		opts   Options
		// maxFile bounds the on-disk footprint for formats that carry
		// their own allocation; sparse ones are checked by allocated blocks.
		maxFile int64
	}{
		{"raw", Raw, Options{}, 0},
		{"vhd-dynamic", VHD, Options{}, 0},
		{"vhd-fixed", VHD, Options{Fixed: true}, 0},
		{"qcow2", QCOW2, Options{}, 0},
		{"vmdk", VMDK, Options{}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "disk"+tc.format.Extension())
			w, err := Create(tc.format, path, testDiskSize, tc.opts)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// The writer reads back through its own tables before Close
			// (bmr's per-member digest read-back uses this seam).
			if err := w.Truncate(testDiskSize); err != nil {
				t.Fatal(err)
			}
			writeTestRuns(t, w, e)
			compareImage(t, w, e)
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			r, err := Open(tc.format, path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer r.Close() // Windows: an open handle blocks the TempDir cleanup
			compareImage(t, r, e)
			// Every file the writer names exists and nothing else was left.
			files := w.Files()
			ents, _ := os.ReadDir(dir)
			if len(ents) != len(files) {
				t.Fatalf("dir has %d files, writer names %d: %v", len(ents), len(files), files)
			}
			for _, f := range files {
				if _, err := os.Stat(f); err != nil {
					t.Fatalf("named file missing: %v", err)
				}
			}
			switch tc.format {
			case VHD:
				if !tc.opts.Fixed {
					// Allocation lives in the format: ~1 MiB of data in 2 MiB blocks
					// touches 8 blocks → the file must stay far below the disk size.
					if sz := fileSize(t, path); sz > 40<<20 {
						t.Fatalf("dynamic VHD is %d bytes for ~1 MiB of data", sz)
					}
				}
			case QCOW2:
				if sz := fileSize(t, path); sz > 8<<20 {
					t.Fatalf("qcow2 is %d bytes for ~1 MiB of data", sz)
				}
				if err := r.(bounded).Reader.(*qcow2Reader).checkRefcounts(); err != nil {
					t.Fatalf("refcounts: %v", err)
				}
			}
			assertSparse(t, files)
		})
	}
}

func TestVHDFooterAndGeometry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.vhd")
	w, err := Create(VHD, path, testDiskSize, Options{})
	if err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, w, makeExpected())
	r, err := openVHD(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !bytes.Equal(r.head, r.footer) {
		t.Fatalf("footer copy at the head differs from the trailing footer")
	}
	if string(r.footer[28:32]) != "qem2" {
		t.Fatalf("creator %q", r.footer[28:32])
	}
	// Geometry per the spec's algorithm for 2621440 sectors (below the
	// 66M-sector threshold for 255 sectors/track): 17 → 31 → 63 sectors,
	// 16 heads, 2621440/63/16 = 2600 cylinders.
	cyl, heads, spt := vhdGeometry(testDiskSize / 512)
	if spt != 63 || heads != 16 || cyl != 2600 {
		t.Fatalf("geometry %d/%d/%d", cyl, heads, spt)
	}
	if r.footer[58] != 16 || r.footer[59] != 63 || binary.BigEndian.Uint16(r.footer[56:58]) != 2600 {
		t.Fatalf("footer geometry %v", r.footer[56:60])
	}
	// A large disk takes the 255-sector branch.
	if _, heads, spt := vhdGeometry(200 << 30 / 512); spt != 255 || heads != 16 {
		t.Fatalf("200 GiB geometry %d/%d", heads, spt)
	}
	// A small disk takes the 17-sector branch.
	if cyl, heads, spt := vhdGeometry(1 << 20 / 512); spt != 17 || heads != 4 || cyl != 30 {
		t.Fatalf("1 MiB geometry %d/%d/%d", cyl, heads, spt)
	}
}

func TestVHDRejectsOddSize(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		_, err := Create(VHD, filepath.Join(t.TempDir(), "d.vhd"), 1000, Options{Fixed: fixed})
		if err == nil || !strings.Contains(err.Error(), "multiple of 512") {
			t.Fatalf("fixed=%v: %v", fixed, err)
		}
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.img")
	os.WriteFile(path, []byte("keep"), 0o644)
	if _, err := Create(Raw, path, 4096, Options{}); err == nil || !strings.Contains(err.Error(), "never an overwrite") {
		t.Fatalf("existing file: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "keep" {
		t.Fatalf("file was touched: %q", b)
	}
	// A VMDK refuses when only its extent exists.
	os.WriteFile(filepath.Join(dir, "v-flat.vmdk"), []byte("keep"), 0o644)
	if _, err := Create(VMDK, filepath.Join(dir, "v.vmdk"), 4096, Options{}); err == nil || !strings.Contains(err.Error(), "v-flat.vmdk exists") {
		t.Fatalf("existing extent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "v.vmdk")); err == nil {
		t.Fatalf("descriptor written despite the refusal")
	}
	// And when only the descriptor exists.
	os.WriteFile(filepath.Join(dir, "d.vmdk"), []byte("keep"), 0o644)
	if _, err := Create(VMDK, filepath.Join(dir, "d.vmdk"), 4096, Options{}); err == nil || !strings.Contains(err.Error(), "never an overwrite") {
		t.Fatalf("existing descriptor: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "d.vmdk")); string(b) != "keep" {
		t.Fatalf("descriptor was overwritten: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "d-flat.vmdk")); err == nil {
		t.Fatalf("extent created despite the refusal")
	}
}

func TestWritesOutsideAndGrowAreRefused(t *testing.T) {
	for _, f := range Formats {
		t.Run(string(f), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "d"+f.Extension())
			w, err := Create(f, path, 1<<20, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			if _, err := w.WriteAt([]byte{1}, 1<<20); err == nil || !strings.Contains(err.Error(), "outside") {
				t.Fatalf("write past end: %v", err)
			}
			if err := w.Truncate(2 << 20); err == nil || !strings.Contains(err.Error(), "grow") {
				t.Fatalf("grow: %v", err)
			}
			if err := w.Truncate(1 << 20); err != nil {
				t.Fatalf("same-size truncate: %v", err)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{"raw": Raw, "IMG": Raw, "bin": Raw, "": Raw, "vhd": VHD, "qcow2": QCOW2, "qcow": QCOW2, " vmdk ": VMDK} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Fatalf("%q → %v, %v", in, got, err)
		}
	}
	if _, err := ParseFormat("vhdx"); err == nil || !strings.Contains(err.Error(), "vhdx") {
		t.Fatalf("vhdx: %v", err)
	}
	if Raw.Extension() != ".img" || VHD.Extension() != ".vhd" {
		t.Fatalf("extensions")
	}
}

func TestForEachDataRunSplitsAtPageEdges(t *testing.T) {
	// From offset 100: the partial first page (100..4096) is zero, pages
	// 1 and 2 hold data, page 3 is zero, and the 200-byte tail has data.
	p := make([]byte, 4*4096+100)
	p[4096-100+10] = 1
	p[2*4096-100+10] = 1
	p[4*4096-100] = 1
	var got []string
	forEachDataRun(100, p, func(rel int, chunk []byte) error {
		got = append(got, fmt.Sprintf("%d+%d", rel, len(chunk)))
		return nil
	})
	want := []string{"3996+8192", "16284+200"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runs %v, want %v", got, want)
	}
	if err := forEachDataRun(0, make([]byte, 9000), func(int, []byte) error { t.Fatal("zero buffer produced a run"); return nil }); err != nil {
		t.Fatal(err)
	}
}
