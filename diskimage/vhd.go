// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"time"
)

// VHD (Microsoft Virtual Hard Disk Image Format Specification, 2006).
// Fixed: the raw image followed by one 512-byte footer. Dynamic: a copy
// of the footer, a dynamic header, a block allocation table, then 2 MiB
// data blocks each led by a sector bitmap, and the footer last. Blocks
// are allocated on the first non-zero write; a block's bitmap marks the
// sectors that were written, so everything else reads as zeros.

const (
	vhdSector      = 512
	vhdBlockSize   = 2 << 20
	vhdTypeFixed   = 2
	vhdTypeDynamic = 3
	vhdCookie      = "conectix"
	vhdDynCookie   = "cxsparse"
)

var vhdEpoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// vhdGeometry is the spec's CHS calculation for a size in sectors.
func vhdGeometry(totalSectors int64) (cyl uint16, heads, spt uint8) {
	if totalSectors > 65535*16*255 {
		totalSectors = 65535 * 16 * 255
	}
	var cylTimesHeads int64
	if totalSectors >= 65535*16*63 {
		spt, heads = 255, 16
		cylTimesHeads = totalSectors / int64(spt)
	} else {
		spt = 17
		cylTimesHeads = totalSectors / int64(spt)
		heads = uint8((cylTimesHeads + 1023) / 1024)
		if heads < 4 {
			heads = 4
		}
		if cylTimesHeads >= int64(heads)*1024 || heads > 16 {
			spt, heads = 31, 16
			cylTimesHeads = totalSectors / int64(spt)
		}
		if cylTimesHeads >= int64(heads)*1024 {
			spt, heads = 63, 16
			cylTimesHeads = totalSectors / int64(spt)
		}
	}
	cyl = uint16(cylTimesHeads / int64(heads))
	return
}

func vhdChecksum(b []byte) uint32 {
	var sum uint32
	for _, x := range b {
		sum += uint32(x)
	}
	return ^sum
}

func vhdFooter(size int64, diskType uint32, dataOffset uint64, uuid [16]byte) []byte {
	f := make([]byte, vhdSector)
	copy(f[0:8], vhdCookie)
	binary.BigEndian.PutUint32(f[8:12], 2)           // features: reserved bit
	binary.BigEndian.PutUint32(f[12:16], 0x00010000) // format version 1.0
	binary.BigEndian.PutUint64(f[16:24], dataOffset)
	binary.BigEndian.PutUint32(f[24:28], uint32(time.Now().UTC().Sub(vhdEpoch).Seconds()))
	// Creator app. qemu's vpc driver sizes a disk from the CHS geometry
	// (which cannot represent most sizes exactly) unless the creator is one
	// it knows writes the true size in the footer; "qem2" is the marker it
	// documents for exactly that. Hyper-V and VirtualBox always use the
	// footer size and ignore the creator.
	copy(f[28:32], "qem2")
	binary.BigEndian.PutUint32(f[32:36], 0x00010000)
	copy(f[36:40], "Wi2k")
	binary.BigEndian.PutUint64(f[40:48], uint64(size))
	binary.BigEndian.PutUint64(f[48:56], uint64(size))
	cyl, heads, spt := vhdGeometry(size / vhdSector)
	binary.BigEndian.PutUint16(f[56:58], cyl)
	f[58], f[59] = heads, spt
	binary.BigEndian.PutUint32(f[60:64], diskType)
	copy(f[68:84], uuid[:])
	binary.BigEndian.PutUint32(f[64:68], vhdChecksum(f))
	return f
}

func newUUID() [16]byte {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

// --- fixed ---

type fixedVHD struct {
	raw  *rawWriter
	size int64
	uuid [16]byte
}

func newFixedVHD(path string, size int64) (*fixedVHD, error) {
	if size%vhdSector != 0 {
		return nil, fmt.Errorf("a VHD's size must be a multiple of %d bytes", vhdSector)
	}
	raw, err := newRaw(path, size+vhdSector)
	if err != nil {
		return nil, err
	}
	w := &fixedVHD{raw: raw, size: size, uuid: newUUID()}
	// The footer is written now so an interrupted export is still a VHD
	// (a short one), never a raw image mistaken for one.
	if _, err := raw.f.WriteAt(vhdFooter(size, vhdTypeFixed, 0xFFFFFFFFFFFFFFFF, w.uuid), size); err != nil {
		raw.Close()
		return nil, err
	}
	return w, nil
}

func (w *fixedVHD) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > w.size {
		return 0, fmt.Errorf("write at %d+%d is outside the %d-byte disk", off, len(p), w.size)
	}
	return w.raw.WriteAt(p, off)
}
func (w *fixedVHD) ReadAt(p []byte, off int64) (int, error) {
	return readMapped(w.raw.f, w.size, 1<<20, p, off, func(unit int64) int64 { return unit << 20 })
}
func (w *fixedVHD) Truncate(size int64) error {
	if size > w.size {
		return fmt.Errorf("cannot grow a %d-byte image to %d", w.size, size)
	}
	return nil
}
func (w *fixedVHD) Sync() error     { return w.raw.Sync() }
func (w *fixedVHD) Files() []string { return w.raw.Files() }
func (w *fixedVHD) Close() error    { return w.raw.Close() }

// --- dynamic ---

type dynamicVHD struct {
	f          *os.File
	size       int64
	uuid       [16]byte
	bat        []uint32 // block index → sector of the block's bitmap; 0xFFFFFFFF = unallocated
	batOffset  int64
	nextSector int64 // next free sector for a block
	bitmapSecs int64 // sectors per bitmap
}

func newDynamicVHD(path string, size int64) (*dynamicVHD, error) {
	if size%vhdSector != 0 {
		return nil, fmt.Errorf("a VHD's size must be a multiple of %d bytes", vhdSector)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	blocks := (size + vhdBlockSize - 1) / vhdBlockSize
	w := &dynamicVHD{f: f, size: size, uuid: newUUID(), bat: make([]uint32, blocks)}
	for i := range w.bat {
		w.bat[i] = 0xFFFFFFFF
	}
	// Layout: footer copy (512) | dynamic header (1024) | BAT (sector-padded) | blocks...
	w.batOffset = vhdSector + 1024
	batBytes := (int64(blocks)*4 + vhdSector - 1) / vhdSector * vhdSector
	w.nextSector = (w.batOffset + batBytes) / vhdSector
	sectorsPerBlock := int64(vhdBlockSize / vhdSector)
	w.bitmapSecs = (sectorsPerBlock/8 + vhdSector - 1) / vhdSector
	if err := w.writeHeaders(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return w, nil
}

func (w *dynamicVHD) writeHeaders() error {
	footer := vhdFooter(w.size, vhdTypeDynamic, uint64(vhdSector), w.uuid)
	if _, err := w.f.WriteAt(footer, 0); err != nil {
		return err
	}
	h := make([]byte, 1024)
	copy(h[0:8], vhdDynCookie)
	binary.BigEndian.PutUint64(h[8:16], 0xFFFFFFFFFFFFFFFF)
	binary.BigEndian.PutUint64(h[16:24], uint64(w.batOffset))
	binary.BigEndian.PutUint32(h[24:28], 0x00010000)
	binary.BigEndian.PutUint32(h[28:32], uint32(len(w.bat)))
	binary.BigEndian.PutUint32(h[32:36], vhdBlockSize)
	binary.BigEndian.PutUint32(h[36:40], vhdChecksum(h))
	if _, err := w.f.WriteAt(h, vhdSector); err != nil {
		return err
	}
	return w.writeBAT()
}

func (w *dynamicVHD) writeBAT() error {
	b := make([]byte, (int64(len(w.bat))*4+vhdSector-1)/vhdSector*vhdSector)
	for i, v := range w.bat {
		binary.BigEndian.PutUint32(b[i*4:], v)
	}
	_, err := w.f.WriteAt(b, w.batOffset)
	return err
}

func (w *dynamicVHD) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > w.size {
		return 0, fmt.Errorf("write at %d+%d is outside the %d-byte disk", off, len(p), w.size)
	}
	err := forEachDataRun(off, p, func(rel int, chunk []byte) error {
		return w.writeRun(off+int64(rel), chunk)
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// writeRun stores one non-zero run, block by block, allocating blocks and
// marking the sectors it touches in their bitmaps.
func (w *dynamicVHD) writeRun(off int64, p []byte) error {
	for len(p) > 0 {
		block := off / vhdBlockSize
		inBlock := off % vhdBlockSize
		n := int64(len(p))
		if inBlock+n > vhdBlockSize {
			n = vhdBlockSize - inBlock
		}
		if w.bat[block] == 0xFFFFFFFF {
			// Allocate: a zeroed bitmap then the block's sectors.
			w.bat[block] = uint32(w.nextSector)
			if _, err := w.f.WriteAt(make([]byte, w.bitmapSecs*vhdSector), w.nextSector*vhdSector); err != nil {
				return err
			}
			w.nextSector += w.bitmapSecs + vhdBlockSize/vhdSector
		}
		bitmapOff := int64(w.bat[block]) * vhdSector
		dataOff := bitmapOff + w.bitmapSecs*vhdSector + inBlock
		if _, err := w.f.WriteAt(p[:n], dataOff); err != nil {
			return err
		}
		// Mark the sectors: sector s of the block → bit (7 - s%8) of byte s/8.
		first, last := inBlock/vhdSector, (inBlock+n-1)/vhdSector
		bm := make([]byte, w.bitmapSecs*vhdSector)
		if _, err := w.f.ReadAt(bm, bitmapOff); err != nil {
			return err
		}
		for s := first; s <= last; s++ {
			bm[s/8] |= 0x80 >> (s % 8)
		}
		if _, err := w.f.WriteAt(bm, bitmapOff); err != nil {
			return err
		}
		off += n
		p = p[n:]
	}
	return nil
}

// ReadAt reads through the in-memory BAT: an unallocated block is zeros,
// an allocated one holds only the sectors this writer put there (its
// unwritten sectors were never touched and read as zeros).
func (w *dynamicVHD) ReadAt(p []byte, off int64) (int, error) {
	return readMapped(w.f, w.size, vhdBlockSize, p, off, func(block int64) int64 {
		if w.bat[block] == 0xFFFFFFFF {
			return -1
		}
		return int64(w.bat[block])*vhdSector + w.bitmapSecs*vhdSector
	})
}

func (w *dynamicVHD) Truncate(size int64) error {
	if size > w.size {
		return fmt.Errorf("cannot grow a %d-byte image to %d", w.size, size)
	}
	return nil
}
func (w *dynamicVHD) Sync() error     { return w.f.Sync() }
func (w *dynamicVHD) Files() []string { return []string{w.f.Name()} }

// Close writes the BAT as it stands and the trailing footer.
func (w *dynamicVHD) Close() error {
	if err := w.writeBAT(); err != nil {
		w.f.Close()
		return err
	}
	footer := vhdFooter(w.size, vhdTypeDynamic, uint64(vhdSector), w.uuid)
	if _, err := w.f.WriteAt(footer, w.nextSector*vhdSector); err != nil {
		w.f.Close()
		return err
	}
	// The footer copy at the head must match the trailing one byte for byte.
	if _, err := w.f.WriteAt(footer, 0); err != nil {
		w.f.Close()
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}
