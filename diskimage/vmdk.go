// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VMDK monolithicFlat: a text descriptor naming one FLAT extent that is a
// plain raw image beside it. The extent is written sparsely like a raw
// image; VMware, VirtualBox and qemu all open the pair.
type vmdkWriter struct {
	raw        *rawWriter
	descriptor string
	extent     string
}

func newVMDK(path string, size int64) (*vmdkWriter, error) {
	if size%512 != 0 {
		return nil, fmt.Errorf("a VMDK's size must be a multiple of 512 bytes")
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	extent := base + "-flat.vmdk"
	if _, err := os.Stat(extent); err == nil {
		return nil, fmt.Errorf("%s exists: an export is a new file, never an overwrite", extent)
	}
	raw, err := newRaw(extent, size)
	if err != nil {
		return nil, err
	}
	w := &vmdkWriter{raw: raw, descriptor: path, extent: extent}
	df, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		_, err = df.WriteString(w.descriptorText(size))
		if cerr := df.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		raw.Close()
		os.Remove(extent)
		return nil, err
	}
	return w, nil
}

func (w *vmdkWriter) descriptorText(size int64) string {
	var cid [4]byte
	_, _ = rand.Read(cid[:])
	sectors := size / 512
	// The geometry VMware derives for a flat disk: 63 sectors, 255 heads.
	cylinders := sectors / (63 * 255)
	if cylinders < 1 {
		cylinders = 1
	}
	return fmt.Sprintf(`# Disk DescriptorFile
version=1
encoding="UTF-8"
CID=%08x
parentCID=ffffffff
isNativeSnapshot="no"
createType="monolithicFlat"

# Extent description
RW %d FLAT "%s" 0

# The Disk Data Base
#DDB

ddb.virtualHWVersion = "4"
ddb.geometry.cylinders = "%d"
ddb.geometry.heads = "255"
ddb.geometry.sectors = "63"
ddb.adapterType = "lsilogic"
ddb.toolsVersion = "0"
`, binary.BigEndian.Uint32(cid[:]), sectors, filepath.Base(w.extent), cylinders)
}

func (w *vmdkWriter) WriteAt(p []byte, off int64) (int, error) { return w.raw.WriteAt(p, off) }
func (w *vmdkWriter) ReadAt(p []byte, off int64) (int, error)  { return w.raw.ReadAt(p, off) }
func (w *vmdkWriter) Truncate(size int64) error                { return w.raw.Truncate(size) }
func (w *vmdkWriter) Sync() error                              { return w.raw.Sync() }
func (w *vmdkWriter) Files() []string                          { return []string{w.descriptor, w.extent} }
func (w *vmdkWriter) Close() error                             { return w.raw.Close() }
