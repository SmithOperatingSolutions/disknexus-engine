// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"encoding/binary"
	"fmt"
	"os"
)

// qcow2 version 3 (QEMU docs/interop/qcow2.rst): 64 KiB clusters, 16-bit
// refcounts, no backing file, compression or snapshots. Cluster 0 holds
// the header, the L1 table follows it, and L2 tables and data clusters
// are appended in the order they are first needed. The refcount table
// and its blocks go last, at Close, when every cluster is known and each
// is referenced exactly once.
const (
	qcowMagic        = 0x514649fb // "QFI\xfb"
	qcowClusterBits  = 16
	qcowCluster      = 1 << qcowClusterBits
	qcowL2Entries    = qcowCluster / 8
	qcowRefcountBits = 16
	qcowRefsPerBlock = qcowCluster * 8 / qcowRefcountBits
	qcowHeaderLen    = 104
	qcowCopied       = uint64(1) << 63
)

type qcow2Writer struct {
	f         *os.File
	size      int64
	l1        []uint64   // guest L2 index → file offset of that L2 table (0 = none)
	l2        [][]uint64 // per L1 entry: guest cluster → file offset (0 = unallocated)
	l1Offset  int64
	nextClust int64 // next free cluster
}

func newQCOW2(path string, size int64) (*qcow2Writer, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	clusters := (size + qcowCluster - 1) / qcowCluster
	l1Size := (clusters + qcowL2Entries - 1) / qcowL2Entries
	w := &qcow2Writer{f: f, size: size, l1: make([]uint64, l1Size), l2: make([][]uint64, l1Size)}
	w.l1Offset = qcowCluster
	l1Clusters := (l1Size*8 + qcowCluster - 1) / qcowCluster
	w.nextClust = 1 + l1Clusters
	if err := w.writeHeader(0, 0); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return w, nil
}

// writeHeader writes cluster 0. Before Close the refcount fields are zero,
// which qemu rejects: an interrupted export is visibly unfinished.
func (w *qcow2Writer) writeHeader(refTableOffset int64, refTableClusters int64) error {
	h := make([]byte, qcowCluster)
	be := binary.BigEndian
	be.PutUint32(h[0:], qcowMagic)
	be.PutUint32(h[4:], 3)
	be.PutUint32(h[20:], qcowClusterBits)
	be.PutUint64(h[24:], uint64(w.size))
	be.PutUint32(h[36:], uint32(len(w.l1)))
	be.PutUint64(h[40:], uint64(w.l1Offset))
	be.PutUint64(h[48:], uint64(refTableOffset))
	be.PutUint32(h[56:], uint32(refTableClusters))
	be.PutUint32(h[96:], 4) // refcount_order: 16-bit entries
	be.PutUint32(h[100:], qcowHeaderLen)
	_, err := w.f.WriteAt(h, 0)
	return err
}

func (w *qcow2Writer) alloc() int64 {
	c := w.nextClust
	w.nextClust++
	return c
}

func (w *qcow2Writer) WriteAt(p []byte, off int64) (int, error) {
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

func (w *qcow2Writer) writeRun(off int64, p []byte) error {
	for len(p) > 0 {
		guest := off / qcowCluster
		in := off % qcowCluster
		n := int64(len(p))
		if in+n > qcowCluster {
			n = qcowCluster - in
		}
		l1i := guest / qcowL2Entries
		l2i := guest % qcowL2Entries
		if w.l2[l1i] == nil {
			w.l2[l1i] = make([]uint64, qcowL2Entries)
			w.l1[l1i] = uint64(w.alloc()*qcowCluster) | qcowCopied
		}
		if w.l2[l1i][l2i] == 0 {
			w.l2[l1i][l2i] = uint64(w.alloc()*qcowCluster) | qcowCopied
		}
		dataOff := int64(w.l2[l1i][l2i]&^qcowCopied) + in
		if _, err := w.f.WriteAt(p[:n], dataOff); err != nil {
			return err
		}
		off += n
		p = p[n:]
	}
	return nil
}

// ReadAt reads through the in-memory L1/L2 maps; an unallocated cluster
// is zeros.
func (w *qcow2Writer) ReadAt(p []byte, off int64) (int, error) {
	return readMapped(w.f, w.size, qcowCluster, p, off, func(guest int64) int64 {
		tbl := w.l2[guest/qcowL2Entries]
		if tbl == nil || tbl[guest%qcowL2Entries] == 0 {
			return -1
		}
		return int64(tbl[guest%qcowL2Entries] &^ qcowCopied)
	})
}

func (w *qcow2Writer) Truncate(size int64) error {
	if size > w.size {
		return fmt.Errorf("cannot grow a %d-byte image to %d", w.size, size)
	}
	return nil
}
func (w *qcow2Writer) Sync() error     { return w.f.Sync() }
func (w *qcow2Writer) Files() []string { return []string{w.f.Name()} }

// Close writes the L1 and L2 tables, then the refcount table and blocks
// covering every cluster of the file (each referenced once), then the
// header pointing at them.
func (w *qcow2Writer) Close() error {
	err := w.finish()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (w *qcow2Writer) finish() error {
	be := binary.BigEndian
	for i, tbl := range w.l2 {
		if tbl == nil {
			continue
		}
		b := make([]byte, qcowCluster)
		for j, e := range tbl {
			be.PutUint64(b[j*8:], e)
		}
		if _, err := w.f.WriteAt(b, int64(w.l1[i]&^qcowCopied)); err != nil {
			return err
		}
	}
	l1b := make([]byte, (int64(len(w.l1))*8+qcowCluster-1)/qcowCluster*qcowCluster)
	for i, e := range w.l1 {
		be.PutUint64(l1b[i*8:], e)
	}
	if _, err := w.f.WriteAt(l1b, w.l1Offset); err != nil {
		return err
	}
	// The refcount structures count themselves: iterate to a fixed point.
	tableOff := w.nextClust
	used := w.nextClust
	var blocks, tableClusters int64
	for {
		total := used + tableClusters + blocks
		nb := (total + qcowRefsPerBlock - 1) / qcowRefsPerBlock
		ntc := (nb*8 + qcowCluster - 1) / qcowCluster
		if nb == blocks && ntc == tableClusters {
			break
		}
		blocks, tableClusters = nb, ntc
	}
	total := used + tableClusters + blocks
	table := make([]byte, tableClusters*qcowCluster)
	for i := int64(0); i < blocks; i++ {
		be.PutUint64(table[i*8:], uint64((tableOff+tableClusters+i)*qcowCluster))
	}
	if _, err := w.f.WriteAt(table, tableOff*qcowCluster); err != nil {
		return err
	}
	for i := int64(0); i < blocks; i++ {
		blk := make([]byte, qcowCluster)
		for c := i * qcowRefsPerBlock; c < (i+1)*qcowRefsPerBlock && c < total; c++ {
			be.PutUint16(blk[(c-i*qcowRefsPerBlock)*2:], 1)
		}
		if _, err := w.f.WriteAt(blk, (tableOff+tableClusters+i)*qcowCluster); err != nil {
			return err
		}
	}
	if err := w.f.Truncate(total * qcowCluster); err != nil {
		return err
	}
	if err := w.writeHeader(tableOff*qcowCluster, tableClusters); err != nil {
		return err
	}
	return w.f.Sync()
}
