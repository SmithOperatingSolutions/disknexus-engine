// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package index

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
)

// Applying pending deltas at open (#357 phase 2, read side).
//
// A repository published as deltas is authoritative-index PLUS whatever
// deltas have not been folded in yet. That union has to be visible to every
// reader — restore, verify, the next backup's dedup, GC's mark phase — and
// the cheapest way to make sure nobody forgets is to make it happen where
// they all already meet: the index open.
//
// So the contract is a directory. Whoever fetched the repo (cloudsync's
// DownloadIndex, GC's temp materialization) drops the pending delta objects
// in <index dir>/deltas/, and every open merges them. A local repo has no
// such directory and behaves exactly as before.
//
// Applied entries land in the in-memory write buffer rather than being
// flushed back into hash-index.db. That is a deliberate cost choice: the
// buffer is O(pending deltas) — bounded by the compaction threshold — where
// rewriting the sorted file would be O(whole index) on every open, including
// every restore. Nothing is written; a reader that opens, resolves and
// discards leaves the repository byte-identical.

// DeltaSubdir is where an index open looks for pending delta objects.
const DeltaSubdir = "deltas"

// deltaSuffix / deltaEncSuffix name the two forms a delta object takes,
// mirroring bloom.bin / bloom.bin.enc: encrypted repos encrypt their index at
// rest, and a delta is index.
const (
	deltaSuffix    = ".delta"
	deltaEncSuffix = ".delta.enc"
)

// applyPendingDeltas merges every delta object in dir/deltas into the index.
// Returns the number of entries merged.
//
// A delta that will not parse is a HARD failure, not a skip. Its entries are
// chunk locations some completed backup depends on; continuing without them
// would present a silently incomplete index to a restore that hard-fails on
// the first missing chunk, at which point the operator has no way to tell an
// unreadable delta from lost data.
func (d *DedupIndex) applyPendingDeltas(baseWasEmpty bool) (int, error) {
	dir := filepath.Join(d.dir, DeltaSubdir)
	names, err := deltaObjectNames(dir)
	if err != nil {
		return 0, err
	}
	merged := 0
	for _, name := range names {
		blob, err := d.readDeltaObject(filepath.Join(dir, name))
		if err != nil {
			return 0, err
		}
		delta, err := ParseDelta(blob)
		if err != nil {
			return 0, fmt.Errorf("index delta %s: %w", name, err)
		}
		delta.ApplyTo(d)
		merged += len(delta.Entries)
	}
	// A repo whose whole index still lives in deltas has no bloom.bin — but
	// the bloom we just built from those deltas' weak hashes is COMPLETE, so
	// this is not the missing-bloom corruption BloomSuspect exists to catch
	// (which is bloom.bin gone while hash-index.db is populated). Saying
	// otherwise would refuse every backup to a repo that has not compacted yet.
	if merged > 0 && baseWasEmpty {
		d.bloomMissing = false
	}
	return merged, nil
}

// deltaObjectNames lists the delta objects in dir, in a deterministic order.
// A missing directory means no pending deltas, which is the common case.
func deltaObjectNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing index deltas: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, deltaSuffix) || strings.HasSuffix(n, deltaEncSuffix) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// readDeltaObject reads one delta, decrypting when the repo's index is
// encrypted. An .enc delta with no key is a refusal, not a skip: see
// applyPendingDeltas.
func (d *DedupIndex) readDeltaObject(path string) ([]byte, error) {
	if !strings.HasSuffix(path, deltaEncSuffix) {
		blob, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading index delta: %w", err)
		}
		return blob, nil
	}
	if d.key == nil {
		return nil, fmt.Errorf("index delta %s is encrypted and this index was opened without a key — "+
			"merging the repository's index needs the repo key (store.IndexKeyFor)", filepath.Base(path))
	}
	tmp := path + ".plain"
	if err := crypto.DecryptFile(d.key, path, tmp); err != nil {
		return nil, fmt.Errorf("decrypting index delta %s: %w", filepath.Base(path), err)
	}
	defer os.Remove(tmp)
	blob, err := os.ReadFile(tmp)
	if err != nil {
		return nil, fmt.Errorf("reading decrypted index delta: %w", err)
	}
	return blob, nil
}

// encryptCapturedDelta mirrors what Flush does to bloom.bin and
// hash-index.db: an encrypted repo's index — and a delta IS index — never
// rests in the clear once the run has finished writing it.
func (d *DedupIndex) encryptCapturedDelta(path string) error {
	if d.key == nil {
		return nil
	}
	// ".enc" beside the plaintext, the same convention bloom.bin.enc uses.
	if err := crypto.EncryptFile(d.key, path, path+".enc"); err != nil {
		return fmt.Errorf("encrypting index delta: %w", err)
	}
	return nil
}
