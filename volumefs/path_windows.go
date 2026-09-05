//go:build windows

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volumefs

import "strings"

// toRawVolumePath converts a Windows drive letter (e.g. "c:", "C:\") to the
// raw volume device path (\\.\C:) required for reading raw sector bytes.
// os.Open("c:") opens the directory; os.Open(`\\.\C:`) opens the raw volume.
func toRawVolumePath(sourcePath string) string {
	p := strings.TrimRight(sourcePath, `\/`)
	if len(p) == 2 && p[1] == ':' {
		return `\\.\` + strings.ToUpper(p[:1]) + `:`
	}
	return sourcePath
}
