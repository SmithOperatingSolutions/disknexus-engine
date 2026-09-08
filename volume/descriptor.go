// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"encoding/binary"
	"strings"
)

// STORAGE_DEVICE_DESCRIPTOR.BusType values that mean an external drive.
const (
	busTypeUSB = 7
	busTypeSD  = 12
	busTypeMMC = 13
)

type diskProps struct {
	removable bool
	serial    string
}

// parseDeviceDescriptor reads the fields of STORAGE_DEVICE_DESCRIPTOR the
// enumeration uses: RemovableMedia (offset 10), SerialNumberOffset (24)
// and BusType (28). A drive on USB, SD or MMC counts as removable whatever
// its media bit says: that is the external drive an operator plugs in.
// Build-tag free so the parse is tested on every OS.
func parseDeviceDescriptor(b []byte) diskProps {
	if len(b) < 32 {
		return diskProps{}
	}
	var p diskProps
	p.removable = b[10] != 0
	switch bus := binary.LittleEndian.Uint32(b[28:32]); bus {
	case busTypeUSB, busTypeSD, busTypeMMC:
		p.removable = true
	}
	if off := binary.LittleEndian.Uint32(b[24:28]); off != 0 && int(off) < len(b) {
		end := off
		for end < uint32(len(b)) && b[end] != 0 {
			end++
		}
		p.serial = strings.TrimSpace(string(b[off:end]))
	}
	return p
}
