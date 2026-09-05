# Index File Recovery

> **The index is not a cache. It is the only map from a backup's contents to
> the bytes on disk.** A manifest names its chunks by hash and nothing else, so
> a repository whose index is gone holds every byte it always held and cannot
> restore any of them. Read the next section before deleting anything in
> `index/`.

The dedup index lives in `<repo>/index/` and consists of **three** things:

- **`hash-index.db`** — sorted binary file mapping SHA-256 chunk hashes to their
  location (pack number + byte offset) in the pack store. Grows with every
  unique chunk ever backed up.
- **`bloom.bin`** (or `bloom.bin.enc` for encrypted repos) — probabilistic
  filter loaded entirely into RAM. Provides fast "definitely not present"
  answers so most chunks never need a disk lookup on the hash index.
- **`index/deltas/*.delta`** (or `.delta.enc`) — per-backup index changes
  published by each writer since #364. **These are authoritative, not
  scratch.** Every index open merges them (`internal/core/index/dedup.go`,
  `applyPendingDeltas`). A chunk written by a recent backup lives *only* in a
  delta until a compaction folds it into `hash-index.db`. Deleting
  `index/deltas/` silently drops the locations of every chunk stored since the
  last compaction.

---

## Why index loss means data loss

This is the invariant the rest of this document depends on, so it is stated
first and precisely.

A manifest entry is 45 bytes and contains **no pack number and no offset**
(`internal/core/manifest/manifest.go`, `EntryRecordSize`):

```
[0:8] VolumeOffset int64 LE | [8:40] ChunkHash [32]byte | [40:44] ChunkLength int32 LE | [44] IsExcluded byte
```

Restore resolves every chunk through the index and **hard-fails on a miss**
(`internal/core/restore/restore.go`):

```go
idxEntry, found, err := r.index.LookupDirect(entry.ChunkHash)
...
if !found {
    return nil, fmt.Errorf("chunk %d not found in index (hash %x)", i, entry.ChunkHash[:8])
}
```

So the pack files being untouched is irrelevant on its own: the data is present
and unaddressable. An index that has forgotten the repository's chunks is, as
`internal/core/index/dedup.go` puts it, "indistinguishable from a fresh one".

**The one nuance that matters, stated exactly.** Chunk identity is the hash of
normalized bytes, so if a later backup re-stores *byte-identical* content, that
content's hash re-enters the index and the old manifests referencing it resolve
again. Recovery by re-backup is therefore *partial and content-dependent*:

- Content still present on the source at re-backup time → re-stored → resolves.
- Content unique to history — a file deleted or modified since, an older
  incremental's superseded blocks — is never re-read, never re-stored, and its
  chunks stay unaddressable **permanently**.

"Run another backup and it heals" is true only for data you still have. It is
exactly false for the data you keep backups in order to still have.

---

## What happens if you delete `hash-index.db` only

`NewHashIndex` opens with `O_RDWR|O_CREATE`, so a new empty file is silently
created. No error is returned. The bloom filter loads from the existing
`bloom.bin` and still holds all hashes from previous runs.

During the next backup:

1. Bloom says chunk *may exist* → triggers hash index lookup
2. Hash index is empty → not found → treated as a bloom false positive
3. Chunk is considered **new** and written to a new pack file

This happens for **every chunk from every previous backup**, so stored data
roughly doubles. The bloom and index are also now permanently out of sync: the
bloom claims millions of chunks exist that the index knows nothing about, and
every future backup pays a disk lookup per chunk before learning it is "new".

**Restorability: broken immediately, then partially self-healing.** Until the
re-store completes, nothing restores. Afterwards, only re-stored content
resolves — see the nuance above.

---

## What happens if you delete `bloom.bin` only

**The next backup refuses to run.** This changed with #370–#372; older versions
of this document described a silent degradation that no longer happens.

`internal/core/pipeline/pipeline.go`:

```go
if !analyzeOnly && dedupIdx.BloomSuspect() {
    dedupIdx.CloseDiscard()
    return nil, fmt.Errorf("bloom filter is missing but the hash index is populated in %s; run 'disknexus index --rebuild-all' before backing up (continuing would bypass dedup and re-store everything)", indexDir)
}
```

`BloomSuspect()` is `bloomMissing && index.Count() > 0` — a missing filter over
a populated index. Refusing is the point: continuing would treat every chunk as
new, because an empty bloom answers "definitely not present" and the pipeline
never consults the hash index behind it.

**Restorability: unaffected.** `hash-index.db` is intact, and restore, verify
and export use `LookupDirect`, which bypasses the bloom entirely — deliberately,
so that a damaged filter never blocks recovery of intact data. Fix the bloom at
your leisure; your backups are fine.

---

## What happens if you delete both

`BloomSuspect()` is false (the index is empty too), so the next backup proceeds
against a clean slate and re-stores everything.

The result is self-consistent and it is **not** the least bad outcome. It is the
worst one: at the moment of deletion every backup in the repository becomes
unrestorable, and only content re-read by a subsequent backup ever becomes
addressable again. A "healthy index" that has forgotten where the last three
years live is not a recovery.

---

## Summary

| Deleted | Next backup | Index state after | Restorability |
|---|---|---|---|
| `hash-index.db` only | All chunks re-stored | Permanently inconsistent | **Broken**, then partial — only re-stored content resolves |
| `bloom.bin` only | **Refused** until rebuilt | Intact | **Unaffected** — restore bypasses the bloom |
| `index/deltas/*` | Proceeds | Silently missing recent chunks | **Broken** for every backup since the last compaction |
| Both index files | All chunks re-stored | Self-consistent but amnesiac | **Broken**, then partial |

---

## Recovery recommendation

**First: do not delete the other file.** The "delete both for a clean slate"
advice that used to live here was wrong and is the fastest way to turn a
recoverable problem into permanent loss.

If a file is missing, rebuild:

```bash
disknexus index --repo /backup/repo --rebuild-all
# For encrypted repos, add --passphrase or set DISKNEXUS_PASSPHRASE:
disknexus index --repo /backup/repo --passphrase mysecret --rebuild-all
```

The rebuild reads every pack file sequentially, decompresses each chunk,
recomputes its hashes, and repopulates the bloom filter and hash index. It
reconstructs the repo's normalizer from the repo config first, so the rebuilt
index is keyed the same way the pipeline wrote the manifest entries. RAM usage
is bounded (~45 MB for the hash index regardless of total chunks; bloom filter
sized to fit the chunk count).

### Two limits on `--rebuild-all` you must know before relying on it

1. **It is refused for managed-encryption repositories.**
   `internal/core/index/rebuild.go`:
   ```go
   if repoCfg.EffectiveEncryptionMode() == store.EncryptManaged {
       return RebuildResult{}, fmt.Errorf("index rebuild is not supported for managed-encryption repositories")
   }
   ```
   Rebuilding needs the chunk key, which for a managed repo the controller
   holds. For these repos index loss is **not locally recoverable**.

2. **It takes a local repository path only.** The command resolves `--repo` to
   an absolute filesystem path and requires `config.json` beside it; there is no
   S3 code path. A cloud repository cannot be rebuilt with this command.

For a cloud or managed repository, treat the index as the irreplaceable object
it is: it is protected by the same durability the packs are, and recovery from
its loss is a support operation, not a CLI flag.

### Encrypted repos

The relevant files are `bloom.bin.enc`, `hash-index.db.enc`, and
`index/deltas/*.delta.enc`. The plaintext `bloom.bin` and `hash-index.db` are
transient working copies, decrypted at startup and removed immediately after
loading — so their *absence* is normal and is not the failure this document is
about. Opening an encrypted index without a key is a hard refusal rather than a
silent empty read (#370), precisely so that a keyless open can never be mistaken
for an empty repository.
