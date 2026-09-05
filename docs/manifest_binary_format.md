# Manifest Binary Format — `.dnm`

> **Normative from "The `.dnm` Format As Written" onwards.** A third-party
> reader or recovery tool can be built from those tables. They are pinned to the
> code by `TestSpecMatchesDNMLayout`
> (`internal/core/manifest/dnm_spec_drift_test.go`), which encodes a real
> manifest, walks the bytes it produced, and compares them against the offsets
> and widths written here. If this file and `on_disk_structs.md` disagree,
> that test is the tiebreaker.
>
> **Everything above that section is the design evaluation from before the
> format shipped.** It is kept for its rationale, and it describes three options
> of which one was built. Where its sketches conflict with the normative
> section — they do, on section order and on a "footer" that does not exist —
> the sketches are wrong.

## Problem Statement

The current `.manifest` JSON format causes:

- **High RAM** — `FileCatalog []FileEntry` and `Entries []Entry` are both loaded entirely into memory on every `Load()` call. At 1M files + 1TB of data this can reach 2.5–2.7 GB of peak memory.
- **Long load times** — `json.Unmarshal` on a 250–300 MB JSON string is slow. Reading + parsing a large manifest can take several seconds.
- **No streaming** — The only streaming path today is the `.entries` write path during backup. All read paths are all-at-once.
- **No partial access** — Loading backup metadata (e.g., for `list`) must skip the `Entries` field but still parses the full JSON blob including `FileCatalog`.

---

## Root Causes

| Source | Worst Case | Issue |
|---|---|---|
| `FileCatalog []FileEntry` in JSON | 1M files × ~300 B = ~300 MB | All loaded + decoded at once |
| `Entries []Entry` from `.entries` sidecar | 16M chunks × 45 B = ~720 MB | All loaded into slice at once |
| `parentHashes` map in incremental pipeline | 32 B × unique chunk count | Full set held in memory |
| `refEntries` cache in FileRestorer | Full parent Entries × N parents | Unbounded |
| `json.Unmarshal` of large JSON | N/A | CPU + memory spike during parse |

---

## Format Options

### Option A — Simple Length-Prefixed Record Stream

One flat binary file. Every record is:

```
[type:  1 byte ]
[length: 4 bytes LE]
[data:  length bytes]
```

Record types:
- `0x01` HEADER — backup metadata (replaces JSON fields)
- `0x02` FILE_ENTRY — one FileEntry (variable)
- `0x03` CHUNK_ENTRY — one Entry (45 bytes, same layout as today)
- `0xFF` END

**Pros:**
- True streaming read and write in a single pass
- Forward-compatible: unknown types can be skipped by any reader
- Simplest to implement

**Cons:**
- No random access — must scan from the start to reach any section
- Cannot seek directly to FileCatalog without scanning past all CHUNK_ENTRYs first
- Cannot efficiently load metadata-only (must scan until first non-HEADER record)

---

### Option B — Sectioned Binary with Header Index

One binary file, structured as:

```
[File Header — 32 bytes]           magic, version, flags, section index offset
[Section Index — variable]         array of (sectionType, offset, length, count, crc32)
[METADATA section]                 fixed-size header fields
[CATALOG section]                  length-prefixed FileEntry records
[ENTRIES section]                  fixed 45-byte Entry records (same as today's sidecar)
[Footer — 8 bytes]                 file crc32, magic
```

> This sketch is **not** what shipped. The section index went to the end of the
> file (the writer cannot know the section sizes up front — see the Cons below,
> which is exactly why), the footer was never built, and no CRC is ever
> computed. See "The `.dnm` Format As Written".

The header contains the byte offset of the section index. A reader can:
1. Read the 32-byte file header → get section index offset
2. Seek to section index → get offsets for METADATA, CATALOG, ENTRIES
3. Seek directly to any section without scanning the file

Within CATALOG and ENTRIES, records are still sequential and can be streamed.

**Pros:**
- Seek to any section in O(1) — load only what you need
- Stream records within a section (no need to load entire section at once)
- Single file, single checksum, one atomic replace on write
- Best for the common case: metadata-only load for `list`, full load for restore

**Cons:**
- Writer must know section sizes before writing the header (two-pass or deferred flush)
- Slightly more complex implementation

---

### Option C — Three Separate Files (least disruptive)

Keep the existing split, add a third file:

```
<backupID>.manifest   JSON with metadata only (no FileCatalog, no Entries)
<backupID>.entries    Existing binary chunk entries — unchanged
<backupID>.catalog    New binary file catalog (length-prefixed FileEntry records)
```

**Pros:**
- Smallest diff — existing `.entries` format and reader unchanged
- Metadata JSON remains human-readable
- Can be implemented incrementally without touching restore logic

**Cons:**
- Three files per backup — more fragile, more to track/copy/delete
- JSON still has a parse cost even if small
- Does not solve the `.entries` all-at-once load problem
- Diverges further from a clean unified format

---

## Recommendation: Option B — Sectioned Binary with Header Index

This gives the best balance of streaming capability, selective loading, and implementation complexity. The section index costs 32–64 bytes and unlocks O(1) section seeking, which directly solves the metadata-only load problem and enables per-record streaming in all sections.

Option A is simpler but gives up seek capability. Option C is easiest to ship but leaves the `.entries` all-at-once load unsolved and adds a third file.

---

## The `.dnm` Format As Written

One file per backup, at `{repoPrefix}manifests/{backupID}.dnm`. It is **not
encrypted and not compressed in any repo mode** — the controller reads it for
file browsing and dedup coordination — so the bytes below are what is at rest.

Source of truth: `internal/core/manifest/dnm.go` (constants and codecs),
`dnm_writer.go` (file writer), `dnm_stream.go` (streamed writer),
`dnm_reader.go` and `dnm_ranged.go` (readers).

### File layout — two variants, one reader

Section order is **not** part of the contract: every reader locates sections
through the section index alone. Two writers exist and they order the sections
differently. What a third-party reader must handle is not the order but the way
the second variant reports where the index is.

**File writer** (`saveDNM`) — every local repository, and any manifest uploaded
as a single object:

```
[File Header — 32 bytes]         SectionIndexOffset patched after the sections
[METADATA section]
[CATALOG section]
[ENTRIES section]
[Section Index — 3 × 36 bytes]   at end of file
```

**Streamed writer** (`DNMStreamer`) — the norm for cloud volume backups, whose
manifest ships as ordered part objects
(`{repoPrefix}manifests/{backupID}.dnm.part-%05d`) that the controller composes
into the final key:

```
[File Header — 32 bytes]         SectionIndexOffset = 0  ← sentinel, not an offset
[ENTRIES section]
[METADATA section]
[CATALOG section]
[Section Index — 3 × 36 bytes]
[Trailer — 8 bytes]              uint64 LE: the real section index offset
```

The first part is uploaded long before the file's total size is known, so the
header written into it cannot carry the offset. It carries zero, and the offset
rides in an 8-byte little-endian trailer at end of file.

**A reader that takes a zero `SectionIndexOffset` at face value seeks to byte 0
and decodes the file's own magic as a section index — so it fails, or worse
succeeds, on most cloud manifests.** On zero, read the last 8 bytes instead, and
verify what they say: the index must begin exactly at
`size − 8 − SectionIndexCount×36`, which is the check `OpenDNMReader` makes
before trusting it.

There is no footer in the single-object variant, and no whole-file checksum in
either — see the note under the header table.

### File Header (32 bytes, fixed)

```
Offset  Size  Field
0       8     Magic: "DNMANIF\x00"
8       2     Version: uint16 LE  (current: 1; a reader must refuse anything else)
10      2     Flags: uint16 LE    (reserved; never set by either writer)
12      8     SectionIndexOffset: uint64 LE  (byte offset from file start; 0 = see trailer)
20      4     SectionIndexCount: uint32 LE   (number of section index entries; always 3)
24      4     FileCRC32: uint32 LE           (reserved; never computed)
28      4     Reserved: [4]byte
```

`SectionIndexOffset` is 8 bytes, not 4. A reader that treats it as a `uint32`
agrees with the writer on every repository under 4 GiB of manifest and then
silently reads garbage on the first one that is larger — the failure appears
years into a deployment, on the biggest customer.

**The CRC fields are never computed.** `encodeHeader` writes `FileCRC32` as zero
and `encodeSectionIndex` writes `SectionCRC32` as zero; the package does not
import `hash/crc32` at all, and no reader validates either field. Treat both as
reserved-zero rather than as an integrity check you can rely on: a `.dnm` is
protected by the durability of the object store, not by this format. (Index
deltas and pack lists *do* carry a SHA-256 that their readers verify and refuse
on — see `on_disk_structs.md`.)

### Section Index Entry (36 bytes each)

```
Offset  Size  Field
0       1     SectionType: uint8
1       7     Reserved: [7]byte  (alignment)
8       8     SectionOffset: uint64 LE  (byte offset from file start)
16      8     SectionLength: uint64 LE  (bytes)
24      8     RecordCount: uint64 LE
32      4     SectionCRC32: uint32 LE   (reserved; never computed)
```

Section types:
- `0x01` METADATA — `RecordCount` is always 1
- `0x02` CATALOG — `RecordCount` is the number of `FileEntry` records
- `0x03` ENTRIES — `RecordCount` is the number of 45-byte `Entry` records

Entries appear in the index in the order the writer emitted the sections, which
differs between the two variants; dispatch on `SectionType`, never on position.

### Shared field encodings

Four primitives are used throughout METADATA and CATALOG. Getting `count16`
wrong is the one that loses data silently.

| Name | Encoding |
|---|---|
| `str8` | `uint8` byte length, then the bytes. The writer **truncates** at 255 bytes. |
| `str16` | `uint16 LE` byte length, then the bytes. The writer truncates at 65535 bytes. |
| `count16` | `uint16 LE` when the value is **< 0xFFFF**. Otherwise the literal `0xFFFF` **sentinel**, followed by the real count as `uint32 LE`. |
| `timeNano` | `int64 LE` Unix nanoseconds. `-9223372036854775808` (`MinInt64`) means the **zero time**; `0` means the Unix epoch, which is a real mtime on files extracted from archives and must round-trip as one. |

`count16` is an escape, not a plain `uint16`. A reader that reads two bytes and
stops will, on a file with 65535 or more source paths or extents, consume the
sentinel as the count and then walk four bytes out of phase for the rest of the
record — the whole section decodes into rubbish rather than failing. The escape
exists because the pre-escape encoder wrapped such counts mod 65536 while still
writing every item, so decoders dropped data without an error.

### METADATA Section

One flat record, fields in exactly this order. There is no length prefix; the
section's extent comes from the section index.

```
BackupID:        str8
SourceVolume:    str16
BackupType:      str8   ("full" / "incremental")
BackupMode:      str8   ("volume" / "file")
ParentBackupID:  str8
Timestamp:       timeNano
SectorSize:      uint32 LE
ClusterSize:     uint32 LE
TotalBytes:      int64 LE
TotalChunks:     int64 LE
UniqueChunks:    int64 LE
DedupChunks:     int64 LE
RawBytes:        int64 LE
StoredBytes:     int64 LE
DedupRatio:      float64 LE   (IEEE-754 bits, little-endian)
CompRatio:       float64 LE
Duration:        str8         (a Go duration STRING, e.g. "1m12.4s" — not a number)
ChangedChunks:   int64 LE
UnchangedChunks: int64 LE
SourcePathCount: count16
SourcePaths:     [count × str16]
WrappedDEKLen:   count16
WrappedDEK:      [len]byte     (92 bytes, ECIES-wrapped AES-256 DEK; managed mode only)
```

Two fields are not what an earlier version of this document claimed, and both
throw a reader out of alignment for the rest of the section:

- **`SourceVolume` is `str16`, not `str8`.** It is the only string in METADATA
  that is not `str8`.
- **`Duration` is a `str8` string, not an `int64` nanosecond count.** The
  `Backup` struct carries `Duration string` and the encoder writes it with
  `writeStr8`. There has never been a `DurationNs` field.

### CATALOG Section (FileEntry records)

Zero or more records, each length-prefixed. Populated for file-mode backups;
volume-mode backups may still carry entries for the files they mapped.

```
RecordLen:     uint32 LE    (bytes that follow — NOT count16)
  Path:          str16
  SourceIndex:   int32 LE
  Size:          int64 LE
  Mode:          uint32 LE
  ModTime:       timeNano
  Flags:         uint8       (bit 0: IsDir, bit 1: IsSymlink, bit 2: Unchanged,
                              bit 3: IsExcluded)
  LinkTarget:    str16
  StreamOffset:  int64 LE
  StreamLength:  int64 LE
  ContentHash:   [32]byte
  DataBackupID:  str8
  ExtentCount:   count16
  Extents:       [ExtentCount × 24 bytes]
    FileOffset:    int64 LE
    VolumeOffset:  int64 LE
    Length:        int64 LE
  InlineDataLen: count16
  InlineData:    [InlineDataLen]byte
```

**`Flags` bit 3 is `IsExcluded`** — the file's blocks were deliberately zeroed by
the capture exclusion map (pagefile, hiberfil and friends). A reader that masks
off only bits 0–2 will present a deliberately-emptied file as ordinary content.

**`InlineData` is the record's last field and it holds real file content.** It
is the resident data of small NTFS files, which have no cluster extents at all
and therefore cannot be reconstructed from the volume: their bytes exist
*nowhere else in the backup*. A reader built from the older version of this
document stopped after `Extents` and silently produced empty files for every
small resident file on the volume.

It was appended after `Extents` for compatibility in both directions: records
written before it existed simply end after the extents, so a decoder must treat
"fewer than 2 bytes remain in this record" as "no inline data" rather than as a
truncation error. Bound the read by `RecordLen`; do not read past it.

### ENTRIES Section

Fixed 45-byte records in sequence, no length prefix (the count is in the section
index). Byte-identical to the transient `.entries` sidecar written during a
backup, which is why the writer can copy that file into the section verbatim.

```
VolumeOffset:  int64 LE      (8 bytes)   [0:8]
ChunkHash:     [32]byte      (32 bytes)  [8:40]
ChunkLength:   uint32 LE     (4 bytes)   [40:44]
IsExcluded:    uint8         (1 byte)    [44]   (0 or 1)
─────────────────────────────────────
Total:         45 bytes
```

Records are in ascending `VolumeOffset` order, which restore relies on for
binary search. There is **no pack number and no offset here**: a manifest names
its chunks by hash alone and every one of them is resolved through the dedup
index. See `index_file_recovery.md` for what follows from that.

---

## Access Patterns Enabled

| Use case | Today | With .dnm |
|---|---|---|
| `list` (metadata only) | Parse full JSON (skips entries sidecar) | Read header → seek to METADATA, read only |
| Restore metadata check | Full JSON + full entries sidecar load | Seek to METADATA section |
| Volume restore | All entries loaded into `[]Entry` | Stream ENTRIES section, process per-record |
| File restore | All entries + all FileCatalog loaded | Stream CATALOG; load ENTRIES on demand |
| Incremental parent comparison | Load all parent entries into hash map | Stream parent ENTRIES, build rolling bloom filter |
| Content hash computation | All entries sorted in memory | Stream ENTRIES in VolumeOffset order (already sorted) |
| Backup write | Already streaming for ENTRIES | Stream all three sections; write index last |

---

## Migration Plan

> **Status: shipped in 2025, and then this document stopped tracking it.** All
> six phases below are in the tree, and `.dnm` is the sole manifest format
> written by new code. But the checklist is a record of what was done then, not
> a statement about the tree now: the format has since gained a fourth flag bit,
> an `InlineData` field carrying NTFS resident content, escaped counts, and an
> entire second (streamed) layout — and the last checklist item, "update
> `on_disk_structs.md` with new format", was ticked against a header and
> section index this document went on to describe **wrongly** for the rest of
> the format's life. Anyone who built a reader from the tables here before this
> correction misparsed every manifest in existence.
>
> Read the checklist as history. The normative section above is the current
> format, and it is pinned by a test so that this cannot recur.

### Phase 1 — Binary FileEntry Serialization ✓

**Goal:** Define and test the binary encoding for `FileEntry`. No file format changes yet.

- [x] Add `MarshalBinary() ([]byte, error)` and `UnmarshalBinary([]byte) error` to `FileEntry`
- [x] Add `MarshalBinary()` / `UnmarshalBinary()` to `Backup` metadata fields (no Entries, no FileCatalog)
- [x] Unit tests covering round-trip fidelity, edge cases (empty path, no extents, nil hash)
- [x] Benchmark binary vs. JSON encoding for a 100K FileEntry slice

### Phase 2 — CatalogWriter / CatalogReader ✓

**Goal:** Streaming writer and reader for the CATALOG section. Parallels existing EntryWriter.

- [x] `CatalogWriter` — writes length-prefixed FileEntry records to an `io.Writer`
  - `Write(fe FileEntry) error`
  - `Close() error`
  - Internal 1 MiB buffer
- [x] `CatalogReader` — reads records one at a time from an `io.Reader`
  - `Next() (FileEntry, error)` — returns `io.EOF` when done
  - `ReadAll() ([]FileEntry, error)` — convenience method for code that needs full slice
- [x] Unit tests for writer/reader round-trip, large payloads, partial reads

### Phase 3 — `.dnm` Writer ✓

**Goal:** Write complete `.dnm` files. Keep `.manifest` + `.entries` write path as fallback during transition.

- [x] Implement `ManifestWriter` that writes header + three sections in order
  - Write file header (placeholder CRC)
  - Stream METADATA section
  - Stream CATALOG section via `CatalogWriter`
  - Stream ENTRIES section via `EntryWriter` (reuse existing)
  - Seek back to write section index and finalize file CRC
- [x] Integrate into `Backup.Save()` — write `.dnm` in addition to existing files (dual-write during transition)
- [x] Tests: file layout, section offsets, CRC correctness, round-trip

### Phase 4 — `.dnm` Reader ✓

**Goal:** Read `.dnm` files with selective section loading.

- [x] Implement `ManifestReader`:
  - `Open(path string) (*ManifestReader, error)` — reads header + section index only
  - `ReadMetadata() (Backup, error)` — seeks to METADATA, returns populated Backup (no Entries/FileCatalog)
  - `StreamCatalog() (*CatalogReader, error)` — returns streaming reader positioned at CATALOG section
  - `StreamEntries() (*EntryReader, error)` — returns streaming reader positioned at ENTRIES section
  - `ReadAllEntries() ([]Entry, error)` — convenience for callers that need full slice
  - `ReadAllCatalog() ([]FileEntry, error)` — convenience for callers that need full slice
- [x] Update `manifest.Load()` to prefer `.dnm` when present, fall back to `.manifest` + `.entries`
- [x] Update `manifest.List()` to use `ReadMetadata()` only

### Phase 5 — Streaming Restore ✓

**Goal:** Use the streaming APIs in restore and pipeline code to reduce peak memory.

- [x] Volume restore: replace `for _, entry := range backup.Entries` with `StreamEntries()` iterator
- [x] File restore: replace `backup.FileCatalog` slice with `StreamCatalog()` iterator where possible
- [x] Incremental comparison in pipeline: stream parent entries into a bloom filter instead of loading into a `map[[32]byte]struct{}`
- [x] `ComputeContentHashes`: stream ENTRIES in sorted order (ENTRIES section is already VolumeOffset-sorted) instead of loading + sorting in memory

### Phase 6 — Cutover & Cleanup ✓

- [x] Remove dual-write (write only `.dnm`)
- [x] Write a one-shot migration tool: `disknexus migrate-manifest <repo>` — reads old `.manifest` + `.entries`, writes `.dnm`, verifies round-trip, then renames originals to `.manifest.bak`
- [x] Update `manifest.Load()` to error on missing `.dnm` (no fallback)
- [x] Remove old `ReadEntries`, `WriteEntries`, JSON manifest save path
- [x] Update `on_disk_structs.md` with new format

---

## Compatibility Notes

- Old backups: `.manifest` + `.entries` remain valid until migrated. The reader falls back automatically during Phase 4/5.
- New backups written by Phase 3+ readers will have both `.dnm` and legacy files until Phase 6 removes dual-write.
- The `.dnm` format version field allows future format changes without a full rewrite.

---

## Expected Impact

| Metric | Today (1M files, 1TB) | After Phase 4–5 |
|---|---|---|
| Peak RAM (restore) | ~2.5–2.7 GB | ~50–100 MB (streaming) |
| Manifest load time | Several seconds (JSON parse) | <100 ms (seek + header read) |
| `list` command speed | Parse full JSON per backup | Read 32-byte header + METADATA section |
| Write throughput | Already streaming (entries) | Same (all sections streamed) |
| Single-file restore | Load entire catalog | Stream to matching FileEntry, stop |
