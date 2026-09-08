//go:build !darwin

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package volume

import "errors"

// errNoRootDisk is only produced by the diskutil composition, which is
// build-tag free so its fixture tests run everywhere.
var errNoRootDisk = errors.New("diskutil reports no whole disk under /")
