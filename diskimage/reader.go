// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

// Readers, written from each format's specification independently of the
// writers (they share nothing but the magic strings). The tests compare
// every exported byte through them, and an export verifies its finished
// file's content digest through them, so a writer that misplaces a table
// or a bit fails here, not in a VM months later.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

// Reader reads the virtual disk held by a finished image.
type Reader interface {
	Size() int64
	io.ReaderAt
	io.Closer
}

// Open parses a finished image of the format and reads its virtual disk.
// Reads are bounded to the virtual size the io.ReaderAt way: a read that
// straddles the end returns the bytes inside with io.EOF, one beyond it
// fails, and a format's block padding past the disk is never returned.
func Open(format Format, path string) (Reader, error) {
	var r Reader
	var err error
	switch format {
	case Raw:
		r, err = openRaw(path)
	case VHD:
		r, err = openVHD(path)
	case QCOW2:
		r, err = openQCOW2(path)
	case VMDK:
		r, err = openVMDK(path)
	default:
		return nil, fmt.Errorf("no reader for %s", format)
	}
	if err != nil {
		return nil, err
	}
	return bounded{r}, nil
}

type bounded struct{ Reader }

func (b bounded) ReadAt(p []byte, off int64) (int, error) {
	size := b.Size()
	if off < 0 || off > size {
		return 0, fmt.Errorf("read at %d is outside the %d-byte image", off, size)
	}
	short := false
	if off+int64(len(p)) > size {
		p = p[:size-off]
		short = true
	}
	n, err := b.Reader.ReadAt(p, off)
	if err == nil && short {
		err = io.EOF
	}
	return n, err
}

// --- raw ---

type rawReader struct{ f *os.File }

func openRaw(path string) (*rawReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &rawReader{f}, nil
}
func (r *rawReader) Size() int64  { st, _ := r.f.Stat(); return st.Size() }
func (r *rawReader) Close() error { return r.f.Close() }
func (r *rawReader) ReadAt(p []byte, off int64) (int, error) {
	return r.f.ReadAt(p, off)
}

// --- VHD ---

type vhdReader struct {
	f         *os.File
	size      int64
	diskType  uint32
	bat       []uint32
	blockSize int64
	bitmapLen int64
	footer    []byte
	head      []byte
}

func openVHD(path string) (*vhdReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	footer := make([]byte, 512)
	if _, err := f.ReadAt(footer, st.Size()-512); err != nil {
		return nil, fmt.Errorf("footer: %w", err)
	}
	if string(footer[:8]) != "conectix" {
		return nil, fmt.Errorf("footer cookie %q", footer[:8])
	}
	// Checksum: ones complement of the byte sum with the checksum field zeroed.
	want := binary.BigEndian.Uint32(footer[64:68])
	cp := append([]byte(nil), footer...)
	copy(cp[64:68], []byte{0, 0, 0, 0})
	var sum uint32
	for _, b := range cp {
		sum += uint32(b)
	}
	if ^sum != want {
		return nil, fmt.Errorf("footer checksum %08x, want %08x", ^sum, want)
	}
	r := &vhdReader{f: f, footer: footer}
	r.size = int64(binary.BigEndian.Uint64(footer[48:56]))
	r.diskType = binary.BigEndian.Uint32(footer[60:64])
	switch r.diskType {
	case 2:
		if st.Size() != r.size+512 {
			return nil, fmt.Errorf("fixed VHD file is %d bytes, want size+512=%d", st.Size(), r.size+512)
		}
		return r, nil
	case 3:
	default:
		return nil, fmt.Errorf("disk type %d", r.diskType)
	}
	r.head = make([]byte, 512)
	if _, err := f.ReadAt(r.head, 0); err != nil {
		return nil, err
	}
	hdrOff := int64(binary.BigEndian.Uint64(footer[16:24]))
	h := make([]byte, 1024)
	if _, err := f.ReadAt(h, hdrOff); err != nil {
		return nil, err
	}
	if string(h[:8]) != "cxsparse" {
		return nil, fmt.Errorf("dynamic header cookie %q", h[:8])
	}
	cp = append([]byte(nil), h...)
	copy(cp[36:40], []byte{0, 0, 0, 0})
	sum = 0
	for _, b := range cp {
		sum += uint32(b)
	}
	if ^sum != binary.BigEndian.Uint32(h[36:40]) {
		return nil, fmt.Errorf("dynamic header checksum mismatch")
	}
	tableOff := int64(binary.BigEndian.Uint64(h[16:24]))
	entries := binary.BigEndian.Uint32(h[28:32])
	r.blockSize = int64(binary.BigEndian.Uint32(h[32:36]))
	if int64(entries)*r.blockSize < r.size {
		return nil, fmt.Errorf("BAT covers %d bytes of a %d-byte disk", int64(entries)*r.blockSize, r.size)
	}
	bat := make([]byte, entries*4)
	if _, err := f.ReadAt(bat, tableOff); err != nil {
		return nil, err
	}
	r.bat = make([]uint32, entries)
	for i := range r.bat {
		r.bat[i] = binary.BigEndian.Uint32(bat[i*4:])
	}
	r.bitmapLen = (r.blockSize/512/8 + 511) / 512 * 512
	return r, nil
}

func (r *vhdReader) Size() int64  { return r.size }
func (r *vhdReader) Close() error { return r.f.Close() }

func (r *vhdReader) ReadAt(p []byte, off int64) (int, error) {
	if r.diskType == 2 {
		return r.f.ReadAt(p, off) // bounded by Open; the footer sits past size
	}
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		blk := cur / r.blockSize
		in := cur % r.blockSize
		n := int64(len(p) - done)
		if in+n > r.blockSize {
			n = r.blockSize - in
		}
		if r.bat[blk] == 0xFFFFFFFF {
			for i := range p[done : done+int(n)] {
				p[done+i] = 0
			}
			done += int(n)
			continue
		}
		base := int64(r.bat[blk]) * 512
		bm := make([]byte, r.bitmapLen)
		if _, err := r.f.ReadAt(bm, base); err != nil {
			return done, err
		}
		// Sector by sector: a clear bit reads as zeros regardless of file bytes.
		for s := in / 512; s <= (in+n-1)/512; s++ {
			sec := make([]byte, 512)
			if bm[s/8]&(0x80>>(s%8)) != 0 {
				if _, err := r.f.ReadAt(sec, base+r.bitmapLen+s*512); err != nil {
					return done, err
				}
			}
			lo := s * 512
			hi := lo + 512
			if lo < in {
				lo = in
			}
			if hi > in+n {
				hi = in + n
			}
			copy(p[done+int(lo-in):], sec[lo-s*512:hi-s*512])
		}
		done += int(n)
	}
	return done, nil
}

// --- qcow2 ---

type qcow2Reader struct {
	f          *os.File
	size       int64
	cluster    int64
	l1         []uint64
	refcounts  map[int64]int // file cluster → refcount from the refcount blocks
	fileClusts int64
}

func openQCOW2(path string) (*qcow2Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	h := make([]byte, 104)
	if _, err := f.ReadAt(h, 0); err != nil {
		return nil, err
	}
	be := binary.BigEndian
	if be.Uint32(h[0:]) != 0x514649fb {
		return nil, fmt.Errorf("magic %x", h[:4])
	}
	if v := be.Uint32(h[4:]); v != 3 {
		return nil, fmt.Errorf("version %d", v)
	}
	if be.Uint64(h[8:]) != 0 || be.Uint32(h[16:]) != 0 {
		return nil, fmt.Errorf("backing file set")
	}
	r := &qcow2Reader{f: f, cluster: 1 << be.Uint32(h[20:]), size: int64(be.Uint64(h[24:]))}
	if be.Uint32(h[32:]) != 0 {
		return nil, fmt.Errorf("encrypted")
	}
	if st.Size()%r.cluster != 0 {
		return nil, fmt.Errorf("file size %d is not cluster aligned", st.Size())
	}
	r.fileClusts = st.Size() / r.cluster
	l1Size := int64(be.Uint32(h[36:]))
	l1Off := int64(be.Uint64(h[40:]))
	if need := (r.size + r.cluster*r.cluster/8 - 1) / (r.cluster * r.cluster / 8); l1Size < need {
		return nil, fmt.Errorf("L1 has %d entries, %d needed", l1Size, need)
	}
	l1b := make([]byte, l1Size*8)
	if _, err := f.ReadAt(l1b, l1Off); err != nil {
		return nil, err
	}
	r.l1 = make([]uint64, l1Size)
	for i := range r.l1 {
		r.l1[i] = be.Uint64(l1b[i*8:])
	}
	if be.Uint64(h[72:]) != 0 {
		return nil, fmt.Errorf("incompatible features set")
	}
	if order := be.Uint32(h[96:]); order != 4 {
		return nil, fmt.Errorf("refcount order %d", order)
	}
	// Refcounts.
	rtOff := int64(be.Uint64(h[48:]))
	rtClusters := int64(be.Uint32(h[56:]))
	if rtOff == 0 || rtClusters == 0 {
		return nil, fmt.Errorf("no refcount table")
	}
	rt := make([]byte, rtClusters*r.cluster)
	if _, err := f.ReadAt(rt, rtOff); err != nil {
		return nil, err
	}
	r.refcounts = map[int64]int{}
	perBlock := r.cluster / 2
	for i := int64(0); i < rtClusters*r.cluster/8; i++ {
		bo := int64(be.Uint64(rt[i*8:]))
		if bo == 0 {
			continue
		}
		blk := make([]byte, r.cluster)
		if _, err := f.ReadAt(blk, bo); err != nil {
			return nil, err
		}
		for j := int64(0); j < perBlock; j++ {
			if c := be.Uint16(blk[j*2:]); c != 0 {
				r.refcounts[i*perBlock+j] = int(c)
			}
		}
	}
	return r, nil
}

func (r *qcow2Reader) Size() int64  { return r.size }
func (r *qcow2Reader) Close() error { return r.f.Close() }

// dataCluster returns the file offset of a guest cluster, 0 if unallocated.
func (r *qcow2Reader) dataCluster(guest int64) (int64, error) {
	per := r.cluster / 8
	l1e := r.l1[guest/per]
	if l1e&^(1<<63) == 0 {
		return 0, nil
	}
	if l1e&(1<<63) == 0 {
		return 0, fmt.Errorf("L1 entry without COPIED flag")
	}
	e := make([]byte, 8)
	if _, err := r.f.ReadAt(e, int64(l1e&^(1<<63))+(guest%per)*8); err != nil {
		return 0, err
	}
	l2e := binary.BigEndian.Uint64(e)
	if l2e == 0 {
		return 0, nil
	}
	if l2e&1 != 0 {
		return 0, fmt.Errorf("zero-cluster flag set")
	}
	if l2e&(1<<62) != 0 {
		return 0, fmt.Errorf("compressed cluster")
	}
	if l2e&(1<<63) == 0 {
		return 0, fmt.Errorf("L2 entry without COPIED flag")
	}
	off := int64(l2e & 0x00fffffffffffe00)
	if off%r.cluster != 0 {
		return 0, fmt.Errorf("data cluster at %d is not aligned", off)
	}
	return off, nil
}

func (r *qcow2Reader) ReadAt(p []byte, off int64) (int, error) {
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		in := cur % r.cluster
		n := int64(len(p) - done)
		if in+n > r.cluster {
			n = r.cluster - in
		}
		co, err := r.dataCluster(cur / r.cluster)
		if err != nil {
			return done, err
		}
		if co == 0 {
			for i := 0; i < int(n); i++ {
				p[done+i] = 0
			}
		} else if _, err := r.f.ReadAt(p[done:done+int(n)], co+in); err != nil {
			return done, err
		}
		done += int(n)
	}
	return done, nil
}

// checkRefcounts walks the header, L1, L2 tables, data clusters and the
// refcount structures and verifies each file cluster is referenced exactly
// once and every refcount is 1 — what `qemu-img check` verifies.
func (r *qcow2Reader) checkRefcounts() error {
	seen := map[int64]int{}
	mark := func(off int64) { seen[off/r.cluster]++ }
	mark(0)
	h := make([]byte, 104)
	r.f.ReadAt(h, 0)
	be := binary.BigEndian
	l1Off := int64(be.Uint64(h[40:]))
	for c := int64(0); c < (int64(len(r.l1))*8+r.cluster-1)/r.cluster; c++ {
		mark(l1Off + c*r.cluster)
	}
	per := r.cluster / 8
	for _, l1e := range r.l1 {
		if l1e&^(1<<63) == 0 {
			continue
		}
		l2off := int64(l1e &^ (1 << 63))
		mark(l2off)
		tbl := make([]byte, r.cluster)
		if _, err := r.f.ReadAt(tbl, l2off); err != nil {
			return err
		}
		for j := int64(0); j < per; j++ {
			if e := be.Uint64(tbl[j*8:]); e != 0 {
				mark(int64(e & 0x00fffffffffffe00))
			}
		}
	}
	rtOff := int64(be.Uint64(h[48:]))
	rtClusters := int64(be.Uint32(h[56:]))
	for c := int64(0); c < rtClusters; c++ {
		mark(rtOff + c*r.cluster)
	}
	rt := make([]byte, rtClusters*r.cluster)
	r.f.ReadAt(rt, rtOff)
	for i := int64(0); i < rtClusters*r.cluster/8; i++ {
		if bo := int64(be.Uint64(rt[i*8:])); bo != 0 {
			mark(bo)
		}
	}
	for c := int64(0); c < r.fileClusts; c++ {
		if seen[c] != 1 {
			return fmt.Errorf("file cluster %d referenced %d times", c, seen[c])
		}
		if r.refcounts[c] != 1 {
			return fmt.Errorf("file cluster %d has refcount %d", c, r.refcounts[c])
		}
	}
	for c := range r.refcounts {
		if c >= r.fileClusts {
			return fmt.Errorf("refcount for cluster %d beyond the file", c)
		}
	}
	return nil
}

// --- VMDK ---

var vmdkExtentRE = regexp.MustCompile(`(?m)^RW (\d+) FLAT "([^"]+)" (\d+)$`)

type vmdkReader struct {
	f    *os.File
	size int64
}

func openVMDK(path string) (*vmdkReader, error) {
	desc, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.Contains(desc, []byte(`createType="monolithicFlat"`)) {
		return nil, fmt.Errorf("not monolithicFlat")
	}
	if !bytes.Contains(desc, []byte("parentCID=ffffffff")) {
		return nil, fmt.Errorf("has a parent")
	}
	m := vmdkExtentRE.FindSubmatch(desc)
	if m == nil {
		return nil, fmt.Errorf("no FLAT extent line")
	}
	sectors, _ := strconv.ParseInt(string(m[1]), 10, 64)
	if string(m[3]) != "0" {
		return nil, fmt.Errorf("extent offset %s", m[3])
	}
	ext := filepath.Join(filepath.Dir(path), string(m[2]))
	f, err := os.Open(ext)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	if st.Size() != sectors*512 {
		return nil, fmt.Errorf("extent is %d bytes, descriptor says %d sectors", st.Size(), sectors)
	}
	return &vmdkReader{f: f, size: sectors * 512}, nil
}
func (r *vmdkReader) Size() int64  { return r.size }
func (r *vmdkReader) Close() error { return r.f.Close() }
func (r *vmdkReader) ReadAt(p []byte, off int64) (int, error) {
	return r.f.ReadAt(p, off)
}
