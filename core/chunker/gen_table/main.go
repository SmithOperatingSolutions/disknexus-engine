// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Generates a deterministic Buzhash table from a fixed seed.
// Run once, paste output into buzhash.go.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

func main() {
	fmt.Println("var buzhashTable = [256]uint64{")
	for i := 0; i < 256; i++ {
		// Derive each entry from SHA-256("disknexus-buzhash-v1-" + index)
		// This is deterministic and produces well-distributed values.
		seed := fmt.Sprintf("disknexus-buzhash-v1-%d", i)
		h := sha256.Sum256([]byte(seed))
		val := binary.LittleEndian.Uint64(h[:8])

		if i%4 == 0 {
			fmt.Print("\t")
		}
		fmt.Printf("0x%016x, ", val)
		if i%4 == 3 {
			fmt.Println()
		}
	}
	fmt.Println("}")
}
