//go:build !linux

// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package diskimage

import "testing"

// Allocated-block accounting is asserted on Linux; the Linux unit job runs
// it on every push. Other platforms cover the byte-for-byte round trip.
func assertSparse(t *testing.T, files []string) {}
