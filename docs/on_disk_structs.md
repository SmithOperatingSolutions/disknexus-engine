# On-Disk Struct Types

<!-- The struct types the engine writes. The repository-level objects the
     product layers on top (index deltas, pack lists, reader leases, tombstones,
     trash, streamed manifest parts) are documented with that product. -->


Reference for the serialization structs used by DiskNexus when persisting data
to disk, and for the objects a repository holds that are not structs at all.

`docs/manifest_binary_format.md` is the authority for the `.dnm` manifest — its
header, section index, and per-section field layouts, in more detail than this
page carries, pinned to the code by a test. What is here is the cross-format
index: what exists, where its key is, and how big it is.

---

## Hash Index — `IndexEntry`

**File:** `internal/core/index/hashindex.go`
**Size:** 48 bytes, binary little-endian
**Sorted by:** First 8 bytes of `StrongHash` (big-endian uint64 for binary search)

```go
type IndexEntry struct {
    StrongHash  [32]byte  // SHA-256 of chunk data
    PackNumber  uint32    // Pack file identifier
    StoreOffset uint64    // Byte offset within pack file
    ChunkLength uint32    // Length of chunk data
}
```

Binary layout:

| Bytes   | Field         | Encoding        |
|---------|---------------|-----------------|
| 0–31    | StrongHash    | raw bytes        |
| 32–35   | PackNumber    | uint32 LE        |
| 36–43   | StoreOffset   | uint64 LE        |
| 44–47   | ChunkLength   | uint32 LE        |

The sorted file (`hash-index.db`) is the durable form. An ephemeral hash table
(`hash-index.htab`) is built from it at backup start, rebuilt after each
`Flush()`, and deleted at `Close()`; see below.

---

## Hash Index — `hash-index.htab` (ephemeral)

**File:** `internal/core/index/hashtable.go`
**Lifetime:** Created at `NewHashIndex`, rebuilt after each `Flush`, deleted at
`Close()`. Never present between backup runs.
**Purpose:** O(1) dedup lookups during a backup session via open-addressed
hashing with linear probing. Supersedes the old delta/session-index LSM approach.

**File layout:**

| Bytes          | Field      | Encoding   | Notes                              |
|----------------|------------|------------|------------------------------------|
| 0–7            | Magic      | `"DNHTAB\x01\x00"` | Format identifier         |
| 8–15           | numSlots   | uint64 LE  | Total slot count                   |
| 16–23          | count      | uint64 LE  | Occupied slots (written at Close)  |
| 24 + i×48 …    | slot[i]    | `IndexEntry` (48 bytes) | Zero `StrongHash` = empty |

`numSlots = max(existingEntries × 2, 1024)` — initial load factor ≤ 50%.

**Slot addressing:** `slotIdx = hashPrefix8(StrongHash) % numSlots`, then linear
probe forward (wrapping). An all-zero `StrongHash` is the empty-slot sentinel
(SHA-256 can never be all-zeros for real data).

**Load threshold:** `Insert` returns `errTableFull` when `count > numSlots × 85%`,
triggering `growHashTable()` which doubles `numSlots` via an atomic
`.htab.tmp` → `.htab` rename.

**Total file size:** `24 + numSlots × 48` bytes. At 20M entries: `24 + 40M × 48 ≈ 1.9 GB`.

---

## Bloom Filter

**File:** `internal/core/index/bloom.go`
**Format:** Binary little-endian

**Header (16 bytes):**

| Bytes | Field    | Encoding   |
|-------|----------|------------|
| 0–7   | numBits  | uint64 LE  |
| 8–15  | count    | uint64 LE  |

**Data:** Bit array stored as consecutive uint64 words (8 bytes each, LE).
Uses 7 hash functions.

---

## Manifest Entries — `Entry`

**File:** `internal/core/manifest/manifest.go`
**Size:** 45 bytes, binary little-endian
**Storage:** ENTRIES section of `.dnm` manifest files (see below)

```go
type Entry struct {
    VolumeOffset int64
    ChunkHash    [32]byte
    ChunkLength  int
    IsExcluded   bool
}
```

Binary layout:

| Bytes | Field        | Encoding            |
|-------|--------------|---------------------|
| 0–7   | VolumeOffset | int64 LE            |
| 8–39  | ChunkHash    | raw bytes (SHA-256) |
| 40–43 | ChunkLength  | uint32 LE           |
| 44    | IsExcluded   | 1 byte (0 or 1)     |

Records are written in VolumeOffset order (already sorted), enabling O(log n)
binary search during restore. During backup a transient `.entries` sidecar is
used as a streaming write buffer; `Save()` embeds it into the `.dnm` file and
deletes it — the sidecar does not persist at rest.

---

## Backup Manifest — `Backup`

**File:** `internal/core/manifest/manifest.go`, `internal/core/manifest/dnm.go`
**Format:** Binary `.dnm` file — sectioned format with file header + section index
**S3 path:** `{repoPrefix}manifests/{backupID}.dnm`

The `.dnm` file is **not encrypted** in any mode (the server needs it for file
browsing and dedup coordination). It contains three sections seekable via a
32-byte file header:

| Section | Type | Content |
|---------|------|---------|
| METADATA (`0x01`) | Binary, length-prefixed strings + LE integers | Backup statistics and config (see field list below) |
| CATALOG (`0x02`) | Length-prefixed `FileEntry` records | Per-file metadata (file-mode backups only) |
| ENTRIES (`0x03`) | Fixed 45-byte `Entry` records | Chunk map, sorted by VolumeOffset |

**File header (32 bytes):**

| Bytes | Field | Encoding |
|-------|-------|----------|
| 0–7   | Magic: `"DNMANIF\x00"` | raw bytes |
| 8–9   | Version | uint16 LE (current: 1) |
| 10–11 | Flags | uint16 LE (reserved, 0) |
| 12–19 | SectionIndexOffset | uint64 LE — byte offset of section index from file start |
| 20–23 | SectionIndexCount | uint32 LE (3) |
| 24–27 | FileCRC32 | uint32 LE (0 = not computed) |
| 28–31 | Reserved | `[4]byte` |

**Section index entry (36 bytes each):**

| Bytes | Field | Encoding |
|-------|-------|----------|
| 0     | SectionType | uint8 |
| 1–7   | Reserved | `[7]byte` |
| 8–15  | SectionOffset | uint64 LE |
| 16–23 | SectionLength | uint64 LE |
| 24–31 | RecordCount | uint64 LE |
| 32–35 | SectionCRC32 | uint32 LE (0 = not computed) |

Both CRC fields are **never computed** — not "computed on close". Nothing in the
manifest package imports `hash/crc32`, and no reader validates them.

**Two layouts, and the sentinel that separates them.** Cloud volume backups
stream their manifest as part objects and cannot know the section index offset
when the first part ships, so their header carries `SectionIndexOffset = 0` and
the real offset sits in an 8-byte LE trailer at end of file. A reader that
believes the zero misparses most cloud manifests. Full description, including
the section ordering of each variant, in `docs/manifest_binary_format.md`.

**`Backup` struct (Go):**

```go
type Backup struct {
    BackupID        string
    Timestamp       time.Time
    SourceVolume    string
    SectorSize      int
    ClusterSize     int
    TotalBytes      int64
    Entries         []Entry     // loaded from ENTRIES section on demand
    ParentBackupID  string
    BackupType      string      // "full" or "incremental"
    TotalChunks     int64
    UniqueChunks    int64
    DedupChunks     int64
    RawBytes        int64
    StoredBytes     int64
    DedupRatio      float64
    CompRatio       float64
    Duration        string
    ChangedChunks   int64
    UnchangedChunks int64
    BackupMode      string      // "volume" or "file"
    SourcePaths     []string
    FileCatalog     []FileEntry // loaded from CATALOG section on demand
    WrappedDEK      []byte      // 92 bytes ECIES-wrapped AES-256 DEK (managed mode only)
}
```

Two further fields on the Go struct — `CatalogSidecarPath` and `CatalogCount` —
are transient (`json:"-"`) and never encoded into the file. The METADATA section
holds exactly the fields listed above, in the order given in
`docs/manifest_binary_format.md`.

Legacy `.manifest` JSON files and `.entries` sidecars from pre-DNM repos are
still readable via `manifest.Load()` for backward compatibility.

---

## File Catalog Entry — `FileEntry`

**File:** `internal/core/manifest/manifest.go` (struct),
`internal/core/manifest/dnm.go` (`encodeFileEntry`/`decodeFileEntry`)
**Format at rest:** **binary** — one length-prefixed record per entry in the
`.dnm` CATALOG section. Field-by-field layout in
`docs/manifest_binary_format.md`.
**Note:** the JSON tags below are *not* the on-disk form. They serve the
controller's HTTP surface and the legacy pre-DNM `.manifest` files, where
`ContentHash` uses custom `MarshalJSON`/`UnmarshalJSON` for hex encoding.

```go
type FileEntry struct {
    Path          string         `json:"path"`
    SourceIndex   int            `json:"source_index"`
    Size          int64          `json:"size"`
    Mode          uint32         `json:"mode"`
    ModTime       time.Time      `json:"mod_time"`
    IsDir         bool           `json:"is_dir,omitempty"`
    IsSymlink     bool           `json:"is_symlink,omitempty"`
    LinkTarget    string         `json:"link_target,omitempty"`
    StreamOffset  int64          `json:"stream_offset"`
    StreamLength  int64          `json:"stream_length"`
    ContentHash   [32]byte       `json:"content_hash,omitempty"`  // hex-encoded in JSON
    Unchanged     bool           `json:"unchanged,omitempty"`
    DataBackupID  string         `json:"data_backup_id,omitempty"`
    VolumeExtents []VolumeExtent `json:"volume_extents,omitempty"`
    InlineData    []byte         `json:"inline_data,omitempty"`   // NTFS resident file content
    IsExcluded    bool           `json:"is_excluded,omitempty"`   // blocks deliberately zeroed (#94)
}
```

`InlineData` carries the whole content of an NTFS **resident** file — one small
enough to live inside its MFT record, with no cluster extents to point at. Those
bytes are in the catalog record and nowhere else in the backup, so a reader that
stops after `VolumeExtents` restores every such file as empty. It is encoded
last in the record precisely so that older records, which end after the extents,
stay decodable.

`IsExcluded` is the catalog twin of `Entry.IsExcluded`: the file was walked and
described, but its blocks were zeroed during capture (pagefile, hiberfil, the
repo's own temp dirs), so it is present in the listing and not restorable.

---

## Volume Extent — `VolumeExtent`

**File:** `internal/core/manifest/manifest.go`
**Format at rest:** binary, 3 × int64 LE (24 bytes) inside the enclosing
`FileEntry` record in the `.dnm` CATALOG section. The JSON tags below are for
the controller's API surface.

```go
type VolumeExtent struct {
    FileOffset   int64 `json:"file_offset"`
    VolumeOffset int64 `json:"volume_offset"`
    Length       int64 `json:"length"`
}
```

---

## Hasher — `ChunkID` (in-memory only)

**File:** `internal/core/hasher/hasher.go`
**Note:** Not serialized to disk directly; feeds into the hash index and bloom filter.

```go
type ChunkID struct {
    WeakHash   uint64    // xxHash → used for bloom filter Add/MayContain
    StrongHash [32]byte  // SHA-256 → used as key in HashIndex
}
```

---

---

## Pack Names — `pack_names.bin`

**File:** `internal/cloudsync/packnames.go`
**S3 path:** `{repoPrefix}index/pack_names.bin` (e.g. `repos/{uuid}/index/pack_names.bin`)
**Format:** Binary little-endian, magic `"DNPN"`

Maps global pack numbers to S3 object keys. The prefix (everything up to and
including `chunks/`) is stored once in the header; each entry is only a 16-byte
raw UUID. The full S3 key for pack N is reconstructed as:

```
prefix + uuid(entry[N]).String() + fmt.Sprintf("-%04d.pack", N)
```

The array is indexed by global pack number (`entry[N]` = pack N), which is why
no explicit pack number field is needed. **The array is dense; the pack numbers
in it are not.** See the gap sentinel under Entries below — reconstructing a key
for every index in `[0, Count)` builds keys for packs that were never written.

**Header (16 bytes):**

| Bytes   | Field      | Encoding    | Notes                              |
|---------|------------|-------------|------------------------------------|
| 0–3     | Magic      | `"DNPN"`    | Format identifier                  |
| 4–5     | Version    | uint16 LE   | Current: 1                         |
| 6–9     | Count      | uint32 LE   | Number of entries (= max pack + 1) |
| 10–11   | PrefixLen  | uint16 LE   | Byte length of prefix string       |
| 12–15   | Reserved   | `[4]byte`   | Zero                               |

**Prefix (`PrefixLen` bytes):**

Variable-length UTF-8 string immediately after the header, e.g.
`"repos/3f4a7b2c-.../chunks/"` or `"pools/a1b2c3-.../chunks/"`.

**Entries (16 bytes each, `entry[N]` = global pack N):**

| Bytes   | Field    | Encoding  | Notes                               |
|---------|----------|-----------|-------------------------------------|
| 0–15    | BackupID | `[16]byte` | UUID raw bytes (RFC 4122, big-endian nibbles) |

A zero UUID (`[16]byte{}`) is a gap sentinel. **Skipping it is mandatory, not an
edge case.** `Count` is `max(packNumber) + 1`, so the array spans every number
from 0 to the highest one ever used and writes a zero UUID for each number
inside that span that no pack occupies (`Marshal`, `internal/cloudsync/packnames.go`).

Gaps are **normal and permanent**, from two independent causes:

- **Pack-number leasing** (`internal/s3client/packlease.go`, `PackLeaseBlock`).
  The controller hands each backup a half-open range of pack numbers — 64 by
  default — and the run uses as many as it needs. Every number it leased and did
  not fill is a permanent hole. A typical backup ends mid-block, so **almost
  every repository written by a leasing agent has gaps**, and it acquires up to
  a block's worth more on each run.
- **Retention.** Deleting a pack removes its slot and leaves the numbering
  around it untouched.

A reader that assumes contiguity synthesizes
`{prefix}{uuid}-{NNNN}.pack` keys for packs that were never written, and gets a
404 partway through a restore — after the restore has started, which is the
worst moment to discover it. Decode index `N` only when its 16 bytes are
non-zero; a zero entry means "pack N does not exist", not "pack N is missing".

**Legacy fallback:** The original JSON sidecar (`pack_names.json` at the repo
root) is still parsed by `LoadPackNames` for backward compatibility. New code
never writes it; detection is by magic header (`"DNPN"` → binary, else JSON).

**Size comparison (per 10,000 packs, 36-char UUIDs, ~50-byte prefix):**

| Format       | Size     | Notes                                    |
|--------------|----------|------------------------------------------|
| Binary       | ~160 KB  | Header + prefix + 16 bytes × count      |
| Legacy JSON  | ~1.1 MB  | Full S3 key string per entry             |

---
