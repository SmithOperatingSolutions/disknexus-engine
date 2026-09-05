# Hash Index Scaling Improvements

## Status

The scaling problem described in this document has been solved with the **hybrid
hash table** approach (implemented in `internal/index/hashtable.go`). The two
earlier proposals (segmented flush, LSM-tree) are superseded — they are preserved
below for historical context.

---

## Implemented: Hybrid Hash Table (`hash-index.htab`)

### Problem

The original `HashIndex` used a sorted flat file (`hash-index.db`) with:
1. An in-memory write buffer that is sorted and written to numbered delta files
   on each `FlushDelta` call.
2. After every `mergeThreshold` (10) delta flushes, all delta files and the
   session index are merge-sorted into a growing session index file — an
   O(N log N) stall that grows throughout the backup.
3. Each `Lookup` binary-searches up to 10 delta files + the session index +
   the main sorted file — 17–20 random `ReadAt` calls per lookup as the
   session progresses.

At 23M chunks per backup this produced measurable and growing latency per chunk
processed.

### Design

Build a **disk-backed open-addressed hash table** (`hash-index.htab`) at backup
start from the sorted `hash-index.db`. All lookups and inserts during the backup
hit the hash table (O(1), 1–2 reads). At final `Flush()`, read everything from
the hash table, sort it, and write the new `hash-index.db`. The `.htab` file is
an ephemeral working file — always rebuilt at the next backup start.

**File layout** (see `on_disk_structs.md` for the full spec):

```
[0:8]   magic "DNHTAB\x01\x00"
[8:16]  numSlots (uint64 LE)
[16:24] count    (uint64 LE, written at Close)
[24:]   slots — numSlots × 48 bytes (zero StrongHash = empty)
```

**Lookup path** (`O(1)`, 1–2 reads typical):
1. In-memory buffer map — O(1).
2. Hash table — `hashPrefix8(strongHash) % numSlots` → linear probe until empty
   slot or match.

**FlushDelta path** (replaces delta file writes):
- Insert each in-memory entry into the hash table.
- If load exceeds 85%, `growHashTable()` doubles the slot count, reads all
  entries, writes a new `.htab.tmp`, and atomically renames it.

**Flush path** (end of backup):
1. `htab.ReadAll()` — sequential scan, skip empty slots.
2. Append remaining in-memory buffer.
3. Sort by `hashPrefix8` (as before).
4. Write sorted `.tmp` → atomic rename to `hash-index.db`.
5. Rebuild hash table from the new sorted file.

**Startup** (`NewHashIndex`):
1. Remove stale `.htab`, `.htab.tmp`, `.delta.*`, `.session`, `.session.tmp`.
2. `numSlots = max(existingEntries × 2, 1024)`.
3. Build the hash table using a **K-pass bucket build** (`K=8`, constant
   `htabBuildK`):
   - **Phase 1:** One sequential read of the sorted file distributes each entry
     into one of K temp bucket files (`hash-index.db.b0.tmp` … `.b7.tmp`) by
     `homeSlot / segSize`. Each bucket covers 1/K of the slot range.
   - **Phase 2:** For each segment k: read the bucket into memory, precompute
     home slots, sort entries by home slot for sequential cache access, linear-
     probe into a `partSlots` buffer covering that segment, carry overflow to
     segment k+1, stream `partSlots` to the output file.
   - Wrap-around carry (entries from the last segment wrapping to slot 0) is
     handled with random writes; at 50% load this is always empty.
   - Bucket temp files are deleted after processing.

**Startup cost** (one-time, sequential):

| Repository size | Sorted file | htab file | Peak RAM  | Build time (500 MB/s disk) |
|----------------|-------------|-----------|-----------|---------------------------|
| 1M entries      | 48 MB       | 96 MB     | ~22 MB    | ~0.1s                     |
| 10M entries     | 480 MB      | 960 MB    | ~220 MB   | ~1s                       |
| 20M entries     | 960 MB      | 1.9 GB    | ~440 MB   | ~2s                       |

Peak RAM = largest single segment's slot buffer (`numSlots/K × 48 B`) +
bucket data (`numEntries/K × 48 B`). The old in-memory build required 1.9 GB
at once for a 20M-entry index; the K-pass approach caps it at ~440 MB.

After the build, `Pipeline.OnIndexReady` is called (if set) with the entry
count and elapsed time, letting the caller print startup progress before
chunking begins.

The `.htab` file is rebuilt after every `Flush()` and deleted at `Close()`.
It is never present between backup runs.

### Result

| Metric | Before | After |
|--------|--------|-------|
| Lookup cost | 17–20 random reads, growing | 1–2 random reads, constant |
| FlushDelta cost | O(m log m) sort + file write | O(m) inserts into htab |
| Session merge stalls | Every 10 FlushDelta calls | None |
| Peak memory (dedup) | ~640 MB (parentHashes map) | ~20 MB (bloom filter only) |
| `SetMemFlushed` mode | Separate code path | No-op (htab handles it) |

**Files changed:**
- `internal/index/hashtable.go` — new `diskHashTable` type
- `internal/index/hashindex.go` — replaced delta/session machinery with htab
- `internal/index/hashtable_test.go` — dedicated unit tests for diskHashTable
- `internal/index/hashindex_test.go` — updated for new behavior

---

## Earlier Proposal: Segmented Flush *(superseded)*

> *This approach was considered before the hash table was implemented. Preserved
> for context.*

Replace the single monolithic index file with multiple sorted **segment files**
(`hash-index.seg.NNNN`). New entries are written as immutable sorted segments;
reads binary-search all segments newest-first. Background compaction merges
segments to bound read amplification.

**Why superseded:** The hash table provides O(1) lookups without any compaction
machinery, no read amplification, and simpler code. The only tradeoff is disk
space for the temporary `.htab` file during backup — acceptable given the
elimination of both the session-merge stalls and the per-lookup random I/O.

---

## Earlier Proposal: LSM-Tree Index *(superseded)*

> *This approach was considered for very large repositories (100M+ chunks).
> Preserved for context.*

A leveled LSM-tree (memtable → L1 → L2 → L3+) with bloom filters per sorted
run and a background compactor. Would provide O(1) amortized inserts and
O(L × log N) reads bounded by the number of levels.

**Why superseded:** The hash table already achieves O(1) inserts and O(1) reads
with far less implementation complexity (~200 lines vs. multiple new types and a
background goroutine). LSM complexity would only be justified if the `.htab`
file size became a problem — at 20M entries the htab is 1.9 GB on disk during
the backup, which is acceptable for the target hardware.

---

## Complexity Summary

| Approach               | Lookup       | FlushDelta | Flush          | Complexity |
|------------------------|--------------|------------|----------------|------------|
| Original (map + binary search) | O(log N), growing | O(m log m) + merge stalls | O(N log N) | Low |
| **Hash table (current)** | **O(1), constant** | **O(m)** | **O(N log N)** | **Low** |
| Segmented flush *(not built)* | O(S × log N) | O(m log m) | O(m log m) | Medium |
| LSM-tree *(not built)* | O(L × log N) | O(1) amortized | O(m log m) | High |
