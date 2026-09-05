# Disk-Level Backup Design

**Status:** Roadmap (Phase 4)

## Problem

The current `--volume` flag backs up a single volume via VSS. The backup
contains only the filesystem data starting from the NTFS boot sector. It does
NOT include:

- MBR or GPT partition table
- EFI System Partition (ESP)
- Recovery partition
- Inter-partition gaps and alignment padding
- Boot sector / bootloader code (before first partition)

This means a volume-level restore requires a pre-existing partition on the
target disk. You cannot clone a boot disk or do bare-metal recovery from a
volume-level backup alone.

## Goal

A `--disk` flag that reads an entire physical disk (`\\.\PhysicalDriveN`),
captures everything from sector 0 to the last sector, and can restore to a
different physical disk to produce an exact bootable clone.

## Disk Layout (What Gets Captured)

### GPT Disk (modern, most common)

```
Offset 0                                                      End of disk
├─────────┬──────┬──────────┬────────────┬────────────┬──────┤
│ Prot MBR│ GPT  │   ESP    │  Recovery  │  Windows   │ GPT  │
│ (LBA 0) │Header│ (FAT32)  │  (NTFS)   │  C: (NTFS) │Backup│
│ 512 B   │ ~32K │ ~100 MB  │  ~500 MB  │  remainder │ ~32K │
├─────────┴──────┴──────────┴────────────┴────────────┴──────┤
```

- **Protective MBR** (LBA 0, 512 bytes): Legacy boot compatibility
- **GPT Header + Partition Entries** (LBA 1–33, ~16 KB): Partition table
- **ESP** (EFI System Partition): Bootloader (bootmgfw.efi), BCD store
- **Recovery**: Windows RE image
- **Windows volume**: The main OS volume (what `--volume C:` captures)
- **Backup GPT** (last 33 sectors): Redundant copy of partition table

### MBR Disk (legacy)

```
Offset 0                                           End of disk
├──────┬──────────┬────────────┬────────────────────┤
│ MBR  │  (gap)   │  Recovery  │   Windows C:       │
│512 B │  ~1 MB   │            │                    │
├──────┴──────────┴────────────┴────────────────────┤
```

## How to Read It

Read directly from `\\.\PhysicalDriveN`:

```go
f, _ := openDevice(`\\.\PhysicalDrive0`)
size, _ := deviceSize(f)  // IOCTL_DISK_GET_LENGTH_INFO
// Read from offset 0 to size — this is the entire disk
```

No VSS needed for the raw read. However, for consistency of live
filesystems on the disk, the approach should be:

1. **Create VSS snapshots** of all mounted volumes on the disk
2. **Read from the physical disk** — the VSS snapshots ensure the
   filesystem data is crash-consistent even though we're reading the
   raw disk
3. **Release VSS snapshots** after backup completes

## Partition Discovery

Use `IOCTL_DISK_GET_DRIVE_LAYOUT_EX` to enumerate partitions:

```go
type PARTITION_INFORMATION_EX struct {
    PartitionStyle uint32    // GPT or MBR
    StartingOffset int64     // Byte offset from disk start
    PartitionLength int64    // Size in bytes
    PartitionNumber uint32
    // ... union of GPT/MBR specific fields
}
```

This gives us:
- Exact byte offset of each partition
- Size of each partition
- Partition type (ESP, Recovery, Data, etc.)
- For GPT: partition GUID, type GUID, attributes

### Why Partition Discovery Matters

1. **Smart exclusions**: Skip known-empty regions (gaps between partitions)
2. **Per-partition VSS**: Know which volumes to snapshot
3. **Restore to different-sized disk**: Remap partition offsets
4. **Manifest metadata**: Store partition layout so restore can recreate it

## Backup Manifest for Disk-Level

The current manifest maps: `chunk_index → (offset, length, hash)`

For disk-level backup, the manifest should additionally store:

```json
{
  "type": "disk",
  "disk_size": 256060514304,
  "sector_size": 512,
  "partition_table": "GPT",
  "partitions": [
    {
      "number": 1,
      "type": "EFI System",
      "offset": 1048576,
      "length": 104857600,
      "filesystem": "FAT32",
      "guid": "..."
    },
    {
      "number": 2,
      "type": "Microsoft Reserved",
      "offset": 105906176,
      "length": 16777216
    },
    {
      "number": 3,
      "type": "Basic Data",
      "offset": 122683392,
      "length": 255936446464,
      "filesystem": "NTFS",
      "guid": "...",
      "volume_label": "Windows"
    }
  ]
}
```

## Restore Flow

### Same-size disk (exact clone)

1. Open target `\\.\PhysicalDriveM` with `FSCTL_LOCK_VOLUME`
2. Write chunks from offset 0 — partition table, ESP, volumes, everything
3. Result: exact bootable clone

### Different-size disk (resize)

More complex — requires:

1. Write MBR/GPT structures, adjusting partition sizes
2. Write partition contents
3. Fix up GPT backup header (points to last LBA)
4. Extend/shrink the last partition to fill available space
5. Run `FSCTL_EXTEND_VOLUME` or filesystem-level resize

This is Phase 4+ complexity.

## Volume-Level vs Disk-Level Summary

| Aspect | `--volume E:` | `--disk \\.\PhysicalDrive0` |
|--------|--------------|----------------------------|
| Captures partition table | No | Yes |
| Captures ESP/bootloader | No | Yes |
| Captures all partitions | No (one at a time) | Yes |
| Requires VSS | Yes (for consistency) | Yes (for filesystem consistency) |
| Restore target | Volume (`\\.\E:`) | Physical disk (`\\.\PhysicalDrive1`) |
| Bare-metal recovery | No (need existing partition) | Yes |
| Size | One volume | Entire disk |

## Implementation Order

1. **Read path**: Open `\\.\PhysicalDriveN`, detect size via ioctl, chunk
   the entire disk (same pipeline as volume backup)
2. **Partition metadata**: Query `IOCTL_DISK_GET_DRIVE_LAYOUT_EX`, store in
   manifest
3. **VSS coordination**: Snapshot all volumes on the disk before reading
4. **Restore to same-size disk**: Write all chunks to target disk
5. **Restore to different-size disk**: Partition remapping (later)

## Open Questions

- Should disk-level backup skip known-zero regions (unallocated space after
  last partition)? Dedup handles zeros efficiently, but skipping saves I/O.
- Should we support backing up individual partitions by offset range?
  e.g., `--disk \\.\PhysicalDrive0 --partition 3`
- How to handle 4Kn (4K native sector) disks vs 512e (512-byte emulated)?
  `IOCTL_DISK_GET_DRIVE_GEOMETRY_EX` reports both physical and logical
  sector size.
