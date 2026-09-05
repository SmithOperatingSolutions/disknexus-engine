# disknexus-engine

A backup storage engine in Go: content-defined chunking, deduplication,
encryption, packed storage, a dedup index, manifests, restore, verify, and
retention — plus the readers that turn a disk, a volume, or a file tree into
bytes the engine can chunk, and the planners that put them back.

It is the engine inside [disknexus](https://github.com/ccsrvs/disknexus). It
is published on its own for one reason: **a backup is only worth what you can
prove about it**, and the proof should not depend on the vendor. A repository
written by this module restores with this module alone — no server, no agent,
no account — and the suite that demonstrates that ships here.

## What it does

| Package | What it is |
|---|---|
| `core/chunker` | Content-defined chunking (Buzhash), the geometry that makes dedup stable across machines |
| `core/hasher` | Dual hashing in one pass: a weak hash for the bloom filter, SHA-256 for identity |
| `core/index` | The dedup index: an on-disk hash index, a bloom filter, and index deltas that are folded, never rewritten per backup |
| `core/store` | Pack files: chunks framed and appended into bounded packs, sealed with a hook the caller uses to ship them |
| `core/manifest` | Manifests (`.dnm`): what a backup is, entry by entry, streamable so a 700 MB manifest never has to fit in memory |
| `core/pipeline` | The backup: read → chunk → hash → dedup → pack → manifest, with checkpoints so an interrupted run resumes |
| `core/restore` | Restore, byte-exact, pack-major so every pack is fetched once; `StreamVerify` for verifying against a digest in spans |
| `core/prune`, `core/forget`, `core/retention` | What to keep, what to delete, and reclaiming packs safely |
| `core/crypto` | AES-256-GCM at rest, keys wrapped by passphrase (Argon2) or by a managed keypair |
| `core/checkpoint`, `core/resume` | The resume protocol: sealed packs, sidecar entries, a checkpoint record |
| `core/disklayout` | GPT/MBR partition tables |
| `volume`, `volumefs` | Reading a block device or image; NTFS/ext4/exFAT/FAT32 catalogs and volatile-region exclusion |
| `vss` | Windows Volume Shadow Copy snapshots (via [go-vss](https://github.com/SmithOperatingSolutions/go-vss)) |
| `filemode` | File-tree capture |
| `bmr` | Bare-metal restore: putting a disk back onto hardware |
| `diskplan`, `restoreplan` | Planning a multi-partition capture; planning how a restore fetches its packs |
| `exportimport` | Exporting a backup set as a portable archive, and importing one |

## The proof

`test/e2e` drives the public API only, against a local repository, and judges
every outcome against something the engine did not produce — the SHA-256 of a
source the test generated, a tree the test wrote, a byte the test flipped:

- a volume backup restores byte-identical, full and incremental, and the parent
  still restores after a child is written;
- every file of a tree extracts byte-identical, including an empty file, one
  larger than a pack, and duplicates that must dedup;
- a real NTFS image gets a catalog, restores whole, and yields a single file
  by path;
- keep-last-1 retention deletes two of three generations, prune reclaims
  disk, and the survivor still restores;
- a backup interrupted after a checkpoint resumes and restores byte-identical;
- verify passes a clean backup and condemns bytes flipped mid-pack;
- an encrypted repository restores with its key and refuses another at open.

Each scenario was mutation-proven against the production code: a single
flipped byte in every decoded chunk turns all seven red.

```
go test ./...            # everything, ~30 s
go test ./test/e2e/      # the proof, ~1 s
```

## Formats

The bytes this engine writes are documented in `docs/` — the manifest binary
format, the index and pack structures, the hash-index sizing model, and the
disk-level layout. They are a compatibility contract: a repository written by
one release is readable by every later one.

## Testing standard

`docs/TESTING.md`. The short version: every test fails without its change,
asserts against an authority rather than the absence of an error, and every
load-bearing guard is mutation-proven. `CONTRIBUTING.md` has the rest.

## Versioning

Pre-1.0. The API is stable in practice but not yet promised: minor versions
may change signatures, and the changelog says so when they do. The on-disk
formats are the thing that does not change under you.

Comments in the source cite issue numbers (`#NNN`). Those refer to the
originating private tracker; they are kept because they name the incident or
decision behind a line of code, which is worth more than a clean-looking
comment.

## License

Apache License 2.0 — see `LICENSE` and `NOTICE`.
