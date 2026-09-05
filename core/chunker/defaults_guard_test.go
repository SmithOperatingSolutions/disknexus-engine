// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package chunker_test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// #356 item 10, second half: the chunker's constants call themselves the
// defaults, and they are not.
//
//	chunker.DefaultMinSize = 4 KB    config.DefaultChunkMinSize = 16 KB
//	chunker.DefaultAvgSize = 8 KB    config.DefaultChunkAvgSize = 64 KB
//	chunker.DefaultMaxSize = 64 KB   config.DefaultChunkMaxSize = 512 KB
//	chunker.DefaultMask  = 0x1FFF    config.DefaultBuzhashMask  = 0xFFFF
//
// The left column is the pre-v0.7.5 geometry. The right column is what the
// product has actually chunked at since the #83 field decision, and it is
// what pipeline passes on EVERY backup — WithMinSize/WithMaxSize/WithMask,
// always, from the repo's stored config. So the chunker's own constants reach
// no production path at all; the only thing they do is answer the question
// "what does disknexus chunk at?" with a number that has been wrong for
// several releases.
//
// That is not a cosmetic complaint. #354 was one package building
// config.Default() instead of reading the repo's stored geometry, and every
// backup it wrote deduped against nothing. A constant named Default, in the
// chunker, four times too small, is the same trap with a shorter fuse.
//
// DefaultAvgSize is worse than wrong: nothing reads it, in this package or
// out of it. The chunker's average is a consequence of the mask, so a
// constant asserting an average it never uses is a lie by existence.

func TestChunkerDoesNotClaimTheProductsDefaultGeometry(t *testing.T) {
	src, err := os.ReadFile("chunker.go") // cwd is this package
	if err != nil {
		t.Fatal(err)
	}

	declRe := regexp.MustCompile(`(?m)^\s*(Default\w+)\s*=`)
	var claimed []string
	for _, m := range declRe.FindAllStringSubmatch(string(src), -1) {
		claimed = append(claimed, m[1])
	}
	if len(claimed) > 0 {
		t.Errorf("engine/core/chunker/chunker.go declares %v — these are the pre-v0.7.5 geometry and "+
			"NOT the product's default, which is config.DefaultChunk* (16/64/512 KB, mask 0xFFFF) and is "+
			"passed explicitly by pipeline on every backup. A constant named Default that answers "+
			"\"what does disknexus chunk at?\" four times too small is the #354 trap with a shorter fuse.",
			claimed)
	}

	// The dead one must be gone entirely, not merely renamed.
	if strings.Contains(string(src), "AvgSize") {
		t.Error("engine/core/chunker/chunker.go still carries an average-size constant — nothing in " +
			"the tree reads it, and the chunker's average is a consequence of the mask, so a constant " +
			"asserting one is a lie by existence")
	}
}
