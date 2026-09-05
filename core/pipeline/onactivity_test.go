// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"context"
	"crypto/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"
)

// OnActivity is the capture-stall watchdog's only input, and it is separate
// from OnProgress on purpose.
//
// OnProgress fires on a ticker whether or not any bytes moved, so a watchdog
// fed by "the callback ran" would call a wedged capture healthy forever. What
// distinguishes the two is the byte count, which is what this hook carries.
//
// It also has to work with OnProgress unset: the progress goroutine used to
// start only when OnProgress was non-nil, so a caller that wanted liveness
// without a progress display would have silently got neither.
func TestOnActivityReportsAdvancingBytesWithoutOnProgress(t *testing.T) {
	repoPath, sourcePath, cfg := setupRepo(t)

	// Big enough that chunking spans many progress ticks.
	sourceData := make([]byte, 32<<20)
	rand.Read(sourceData)
	if err := os.WriteFile(sourcePath, sourceData, 0644); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []int64

	p := pipeline.New(cfg, newLogger(), noEnc())
	p.ProgressInterval = time.Millisecond
	p.OnActivity = func(b int64) {
		mu.Lock()
		seen = append(seen, b)
		mu.Unlock()
	}
	// OnProgress deliberately left nil.

	reader, err := volume.NewReader(sourcePath, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Backup(context.Background(), reader, sourcePath, reader.Size(), repoPath)
	reader.Close()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("OnActivity was never called with OnProgress unset — the progress goroutine still starts " +
			"only for OnProgress, so a capture reports no liveness signal at all unless something also " +
			"wanted a progress display")
	}
	// The count must actually advance; a hook that reported a constant would
	// satisfy "was called" while telling the watchdog nothing.
	if seen[len(seen)-1] <= 0 {
		t.Fatalf("OnActivity only ever reported %d bytes — the watchdog cannot tell progress from a wedge "+
			"on a counter that does not move; saw %d calls", seen[len(seen)-1], len(seen))
	}
}
