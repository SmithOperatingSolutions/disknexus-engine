// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

//go:build filesystem

package volumefs

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	parser "www.velocidex.com/golang/go-ntfs/parser"
)

func rawBytes(t *testing.T, path string, off, n int64) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return b
}

// The NTFS minimum is what a drive-upgrade plan shrinks against (#223): it
// must be at least what the files occupy (authority: the scanner's catalog),
// at most the volume, and it must MOVE when the last cluster comes into use
// — the sensitivity a plan that ignored $Bitmap could never show.
func TestNTFSMinimumSizeFollowsTheBitmap(t *testing.T) {
	img := testdataPath(t, "ntfs.img")
	boot := rawBytes(t, img, 0, 512)
	bps := int64(binary.LittleEndian.Uint16(boot[0x0B:0x0D]))
	total := int64(binary.LittleEndian.Uint64(boot[0x28:0x30])) * bps
	cluster := bps * int64(boot[0x0D])

	m, err := MinimumSize(img, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Filesystem != "ntfs" || !m.Shrinkable || m.TotalBytes != total {
		t.Fatalf("minimum = %+v, want shrinkable ntfs of %d bytes", m, total)
	}
	res, err := ScanVolume(context.Background(), img, total, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var fileBytes int64
	for _, f := range res.Files {
		if !f.IsDir {
			fileBytes += f.Size
		}
	}
	if fileBytes == 0 {
		t.Fatal("fixture catalog has no file bytes — the bound below checks nothing")
	}
	if m.UsedBytes < fileBytes {
		t.Fatalf("UsedBytes %d < the catalog's %d file bytes — the bitmap read is missing clusters", m.UsedBytes, fileBytes)
	}
	if m.MinimumBytes < m.UsedBytes || m.MinimumBytes > total || m.MinimumBytes%(1<<20) != 0 && m.MinimumBytes != total {
		t.Fatalf("MinimumBytes %d outside [used %d, total %d] or not MiB-rounded", m.MinimumBytes, m.UsedBytes, total)
	}

	// Sensitivity: mark the LAST cluster in use on a copy of the image. The
	// bit's location comes from the parser's own run list for $Bitmap, an
	// independent path from MinimumSize's read.
	copyPath := filepath.Join(t.TempDir(), "ntfs-lastbit.img")
	data, err := os.ReadFile(img)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := parser.NewPagedReader(io.NewSectionReader(bytesReaderAt(data), 0, int64(len(data))), 1024, 10000)
	ntfs, err := parser.GetNTFSContext(reader, 0)
	if err != nil {
		t.Fatal(err)
	}
	bm, _ := ntfs.GetMFT(6)
	attr, err := bm.GetAttribute(ntfs, 128, -1, "")
	if err != nil {
		t.Fatal(err)
	}
	runs := attr.RunList()
	if len(runs) == 0 {
		t.Fatal("$Bitmap has no runs")
	}
	clusters := total / cluster
	lastByte := (clusters - 1) / 8
	lastBit := uint((clusters - 1) % 8)
	bitmapOff := runs[0].Offset*cluster + lastByte
	if runs[0].Length*cluster <= lastByte {
		t.Fatalf("fixture's $Bitmap first run (%d clusters) does not cover byte %d; extend the test", runs[0].Length, lastByte)
	}
	ntfs.Close()

	// Authority (§3): the bitmap bytes themselves, read at the run's
	// location. Used bytes are the popcount; the last used end is the last
	// set bit. Exact, before any MiB rounding can hide an off-by-a-few.
	raw := data[runs[0].Offset*cluster : runs[0].Offset*cluster+lastByte+1]
	var popcount int64
	lastSet := int64(-1)
	for i, b := range raw {
		for bit := 0; bit < 8; bit++ {
			if int64(i)*8+int64(bit) >= clusters {
				break
			}
			if b&(1<<uint(bit)) != 0 {
				popcount++
				lastSet = int64(i)*8 + int64(bit)
			}
		}
	}
	if m.UsedBytes != popcount*cluster {
		t.Fatalf("UsedBytes %d, the bitmap's popcount says %d", m.UsedBytes, popcount*cluster)
	}
	if m.LastUsedEnd != (lastSet+1)*cluster {
		t.Fatalf("LastUsedEnd %d, the bitmap's last set bit says %d", m.LastUsedEnd, (lastSet+1)*cluster)
	}
	if data[bitmapOff]&(1<<lastBit) != 0 {
		t.Fatal("fixture's last cluster is already in use — no sensitivity to show")
	}
	data[bitmapOff] |= 1 << lastBit
	if err := os.WriteFile(copyPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	m2, err := MinimumSize(copyPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m2.UsedBytes != m.UsedBytes+cluster {
		t.Fatalf("marking one cluster changed UsedBytes by %d, want %d", m2.UsedBytes-m.UsedBytes, cluster)
	}
	if m2.MinimumBytes != total {
		t.Fatalf("with the last cluster in use MinimumBytes = %d, want the whole volume %d — a shrink planned from this would cut off data", m2.MinimumBytes, total)
	}
}

func bytesReaderAt(b []byte) io.ReaderAt { return &byteRA{b} }

type byteRA struct{ b []byte }

func (r *byteRA) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// ext4's used and minimum sizes come from the superblock; the test reads
// the same fields raw.
func TestExt4MinimumSizeFromTheSuperblock(t *testing.T) {
	img := testdataPath(t, "ext4.img")
	sb := rawBytes(t, img, 1024, 1024)
	blocks := int64(binary.LittleEndian.Uint32(sb[4:8]))
	free := int64(binary.LittleEndian.Uint32(sb[12:16]))
	bs := int64(1024) << binary.LittleEndian.Uint32(sb[24:28])
	m, err := MinimumSize(img, 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Filesystem != "ext4" || !m.Shrinkable || m.TotalBytes != blocks*bs || m.UsedBytes != (blocks-free)*bs {
		t.Fatalf("minimum = %+v, want ext4 total %d used %d", m, blocks*bs, (blocks-free)*bs)
	}
	if m.MinimumBytes < m.UsedBytes || m.MinimumBytes > m.TotalBytes {
		t.Fatalf("MinimumBytes %d outside [used, total]", m.MinimumBytes)
	}
	id, err := Identity(img, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := sb[104:120]
	want := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", binary.BigEndian.Uint32(u[0:4]), binary.BigEndian.Uint16(u[4:6]),
		binary.BigEndian.Uint16(u[6:8]), binary.BigEndian.Uint16(u[8:10]), u[10:16])
	if id.Filesystem != "ext4" || id.Serial != want || id.SizeBytes != blocks*bs {
		t.Fatalf("identity = %+v, want ext4 uuid %s size %d", id, want, blocks*bs)
	}
}

// Identity is what a removable source is recognized by (#457): the serial
// the boot sector carries, read here straight from the bytes. FAT
// filesystems are not shrinkable, and say so.
func TestIdentityReadsEachFilesystemsSerial(t *testing.T) {
	ntfs := testdataPath(t, "ntfs.img")
	boot := rawBytes(t, ntfs, 0, 512)
	id, err := Identity(ntfs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%016X", binary.LittleEndian.Uint64(boot[0x48:0x50])); id.Filesystem != "ntfs" || id.Serial != want {
		t.Fatalf("ntfs identity = %+v, want serial %s", id, want)
	}
	if auto, err := IdentityAuto(ntfs); err != nil || auto != id {
		t.Fatalf("IdentityAuto = %+v, %v; want %+v", auto, err, id)
	}

	fat := decompressFAT32(t)
	fboot := rawBytes(t, fat, 0, 512)
	fid, err := Identity(fat, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%08X", binary.LittleEndian.Uint32(fboot[0x43:0x47])); fid.Filesystem != "fat32" || fid.Serial != want {
		t.Fatalf("fat32 identity = %+v, want serial %s", fid, want)
	}
	if fm, err := MinimumSize(fat, 0); err != nil || fm.Shrinkable || fm.Filesystem != "fat32" || fm.Reason == "" {
		t.Fatalf("fat32 minimum = %+v, %v; want not shrinkable with a reason", fm, err)
	}

	exfat := testdataPath(t, "exfat.img")
	eboot := rawBytes(t, exfat, 0, 512)
	eid, err := Identity(exfat, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%08X", binary.LittleEndian.Uint32(eboot[0x64:0x68])); eid.Filesystem != "exfat" || eid.Serial != want {
		t.Fatalf("exfat identity = %+v, want serial %s", eid, want)
	}
	if eid.SizeBytes != int64(binary.LittleEndian.Uint64(eboot[0x48:0x50]))<<uint(eboot[0x6C]) {
		t.Fatalf("exfat size = %d", eid.SizeBytes)
	}

	// Not a filesystem: an error, never an empty identity.
	blank := filepath.Join(t.TempDir(), "blank.img")
	os.WriteFile(blank, make([]byte, 1<<20), 0o644)
	if _, err := Identity(blank, 0); err == nil {
		t.Fatal("a blank image produced an identity")
	}
	if _, err := MinimumSize(blank, 0); err == nil {
		t.Fatal("a blank image produced a minimum size")
	}
}
