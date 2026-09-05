// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import (
	"io"
	"os"
)

// SourceSize sizes an opened capture source. Seek(END) works for regular
// files, images, and VHD-mounted devices — but Windows rejects it on raw
// \\.\PhysicalDriveN handles ("Incorrect function", #83), where the size
// must come from IOCTL_DISK_GET_LENGTH_INFO. One shared derivation for the
// CLI and the agent (#309): the agent's hand-rolled `st, _ := f.Stat()`
// returned a nil FileInfo on raw devices and the nil-deref killed the
// entire service, one second into every whole-disk capture.
func SourceSize(f *os.File) (int64, error) {
	cur, curErr := f.Seek(0, io.SeekCurrent)
	if size, err := f.Seek(0, io.SeekEnd); err == nil {
		if curErr == nil {
			if _, err := f.Seek(cur, io.SeekStart); err != nil {
				return 0, err
			}
		}
		return size, nil
	}
	return DeviceSize(f) // IOCTL: no offset movement
}
