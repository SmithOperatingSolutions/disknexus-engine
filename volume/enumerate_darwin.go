// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"errors"
	"fmt"
)

// macOS: everything comes from diskutil's plists (see diskutil.go).

var errNoRootDisk = errors.New("diskutil reports no whole disk under /")

func (e Enumerator) volumes() ([]VolumeInfo, error) {
	raw, err := e.diskutil("list", "-plist")
	if err != nil {
		return nil, fmt.Errorf("diskutil list: %w", err)
	}
	return e.volumesFromDiskutil(raw)
}

func (e Enumerator) disks() ([]Disk, error) {
	raw, err := e.diskutil("list", "-plist")
	if err != nil {
		return nil, fmt.Errorf("diskutil list: %w", err)
	}
	return e.disksFromDiskutil(raw)
}

func (e Enumerator) systemDisk() (string, error) {
	raw, err := e.diskutil("info", "-plist", "/")
	if err != nil {
		return "", fmt.Errorf("diskutil info /: %w", err)
	}
	return e.systemDiskFromDiskutil(raw)
}
