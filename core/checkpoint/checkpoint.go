// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

// Package checkpoint persists and validates the durable resume checkpoint for
// an in-progress backup (issue #42). Exactly one checkpoint file per backup
// lives at manifests/<backupID>.checkpoint. It records the last durably-sealed
// pack, the source byte offset to resume from (a content-defined chunk
// boundary), the durable entries-sidecar length, and enough source/config
// identity to refuse resuming against divergent bytes.
//
// The file is written atomically (temp + fsync + rename + dir-sync) so its mere
// presence means validity, and a trailing CRC32 rejects any media tear: a bad
// CRC is treated as "no valid checkpoint".
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Version is the checkpoint schema version.
const Version = 1

// Suffix is the checkpoint filename suffix within manifests/.
const Suffix = ".checkpoint"

// Progress is the pipeline-produced portion of a checkpoint, handed to the
// pipeline's CheckpointFn hook at each pack seal. The CLI merges the identity
// fields and writes the full record. Every field here is knowable from the
// store loop alone (storage-agnostic).
type Progress struct {
	LastSealedPack uint32 `json:"last_sealed_pack"`
	// CheckpointSeq is the 0-based sequence of this checkpoint within the
	// backup — equal to the Seq of the Segment written just before it (#55).
	// A resumed run continues numbering from CheckpointSeq+1.
	CheckpointSeq       uint32   `json:"checkpoint_seq"`
	ResumeOffset        int64    `json:"resume_offset"`
	EntriesLen          int64    `json:"entries_len"`
	EntriesCount        int64    `json:"entries_count"`
	BoundaryChunkHash   [32]byte `json:"boundary_chunk_hash"`
	BoundaryChunkOffset int64    `json:"boundary_chunk_offset"`
	BoundaryChunkLength int      `json:"boundary_chunk_length"`

	// Running manifest totals so the final .dnm covers prefix+suffix.
	TotalChunks  int64 `json:"total_chunks"`
	RawBytes     int64 `json:"raw_bytes"`
	UniqueChunks int64 `json:"unique_chunks"`
	DedupChunks  int64 `json:"dedup_chunks"`
	StoredBytes  int64 `json:"stored_bytes"`

	// DigestState is the marshaled SHA-256 state of the content digest
	// (#455) folded over the stream BEFORE the resume-point chunk — the
	// resumed run cannot re-read the prefix, so the fold travels here.
	DigestState []byte `json:"digest_state,omitempty"`
}

// Checkpoint is the full durable record.
type Checkpoint struct {
	Version  int    `json:"version"`
	Mode     string `json:"mode"` // "volume" (reserved: "file")
	BackupID string `json:"backup_id"`

	Progress `json:"progress"`

	// Source identity — all must match on resume or the resume is refused.
	SourceKind          string `json:"source_kind"` // "input" | "no-vss" | "vss"
	SourcePath          string `json:"source_path"`
	TotalSize           int64  `json:"total_size"`
	SourceMTimeUnixNano int64  `json:"source_mtime_unix_nano,omitempty"`
	VSSSnapshotID       string `json:"vss_snapshot_id,omitempty"`
	VSSDevicePath       string `json:"vss_device_path,omitempty"`
	Volume              string `json:"volume,omitempty"`

	// Chunker/normalizer/encryption identity.
	ConfigHash [32]byte `json:"config_hash"`

	// PacksGeneration is the repo's pack-layout generation when this backup
	// started. Prune bumps the generation after renumbering packs, so a
	// mismatch means this checkpoint's segments reference stale pack numbers
	// and resume must rebuild instead of replaying them (#55/#56).
	PacksGeneration string `json:"packs_generation,omitempty"`

	// CatalogHash fingerprints a file-mode backup's walk result (#51): the
	// concatenated stream is only reproducible if a fresh walk yields the exact
	// same catalog, so any mismatch refuses the resume. Zero for volume mode.
	CatalogHash [32]byte `json:"catalog_hash,omitempty"`

	// BasePackCount is the repo's pack count when a CLOUD backup started (#50):
	// the pack-number base the session's packs were numbered from. On resume it
	// must equal the freshly-downloaded pack count, or another backup completed
	// meanwhile and the shared numbering advanced — resume must refuse.
	BasePackCount uint32 `json:"base_pack_count,omitempty"`

	// PackLeaseStart/PackLeaseCount record the controller-issued pack range
	// this backup owns (#357 phase 1). A LEASED backup's numbers were reserved
	// for it, so what the rest of the repo did to its own slots while this one
	// was suspended cannot invalidate them — the BasePackCount refusal above is
	// retired for lease holders and kept for everyone else. Zero = no lease
	// (an old agent, or an older controller), and the refusal applies.
	PackLeaseStart uint32 `json:"pack_lease_start,omitempty"`
	PackLeaseCount uint32 `json:"pack_lease_count,omitempty"`

	// ParentBackupID is set for a resumable incremental backup (#54); the
	// resumed run stamps this lineage onto the completed manifest. It does not
	// affect chunking (dedup is parent-independent).
	ParentBackupID string `json:"parent_backup_id,omitempty"`

	CreatedUnixNano int64 `json:"created_unix_nano"`
}

// envelope wraps the serialized checkpoint body with a CRC over exactly those
// bytes, so validation is independent of JSON field ordering.
type envelope struct {
	Body  json.RawMessage `json:"body"`
	CRC32 uint32          `json:"crc32"`
}

// ErrBadCRC indicates a checkpoint whose stored CRC does not match its body;
// callers treat it as no valid checkpoint.
var ErrBadCRC = fmt.Errorf("checkpoint CRC mismatch")

// Path returns the checkpoint file path for a backup.
func Path(repoPath, backupID string) string {
	return filepath.Join(repoPath, "manifests", backupID+Suffix)
}

// Write atomically persists a checkpoint. It lags the durable pack seal by
// contract (the caller invokes it only after the seal is fsynced), so a crash
// between seal and this write re-does one pack on resume rather than losing data.
func Write(repoPath string, c *Checkpoint) error {
	body, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	data, err := json.Marshal(envelope{Body: body, CRC32: crc32.ChecksumIEEE(body)})
	if err != nil {
		return fmt.Errorf("marshal checkpoint envelope: %w", err)
	}
	return atomicWrite(Path(repoPath, c.BackupID), data)
}

// Load reads and validates one checkpoint by backup ID.
func Load(repoPath, backupID string) (*Checkpoint, error) {
	data, err := os.ReadFile(Path(repoPath, backupID))
	if err != nil {
		return nil, err
	}
	return parse(data)
}

// Find returns the single valid in-progress checkpoint in a repo, or (nil, nil)
// if there is none. Checkpoints with a bad CRC are ignored (treated as absent).
// v1 supports at most one in-progress backup per repo (enforced by the backup
// lock); if several files somehow exist, the lexicographically-first valid one
// is returned deterministically.
func Find(repoPath string) (*Checkpoint, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "manifests", "*"+Suffix))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		c, err := parse(data)
		if err != nil {
			continue // bad CRC / corrupt → not a valid checkpoint
		}
		return c, nil
	}
	return nil, nil
}

// Exists reports whether any checkpoint file (valid or not) is present. Used by
// prune to refuse when an in-progress backup may exist.
func Exists(repoPath string) (bool, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "manifests", "*"+Suffix))
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

// Remove deletes a backup's checkpoint (on successful completion or --restart).
func Remove(repoPath, backupID string) error {
	err := os.Remove(Path(repoPath, backupID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func parse(data []byte) (*Checkpoint, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint envelope: %w", err)
	}
	if crc32.ChecksumIEEE(env.Body) != env.CRC32 {
		return nil, ErrBadCRC
	}
	var c Checkpoint
	if err := json.Unmarshal(env.Body, &c); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint body: %w", err)
	}
	return &c, nil
}

// Identity is the freshly-observed source/config identity to check against a
// recorded checkpoint before resuming.
type Identity struct {
	SourceKind          string
	SourcePath          string
	Volume              string
	TotalSize           int64
	SourceMTimeUnixNano int64
	ConfigHash          [32]byte
	CatalogHash         [32]byte // file mode only (#51)
}

// VerifyIdentity compares a checkpoint's recorded identity against freshly
// observed values and returns a human-readable mismatch reason, or "" if the
// resume is safe to attempt. The boundary-byte probe and VSS-snapshot liveness
// are stronger checks performed separately by the caller.
func (c *Checkpoint) VerifyIdentity(cur Identity) string {
	if c.SourceKind != cur.SourceKind {
		return fmt.Sprintf("source kind changed (%s -> %s)", c.SourceKind, cur.SourceKind)
	}
	if c.SourcePath != cur.SourcePath {
		return fmt.Sprintf("source path changed (%s -> %s)", c.SourcePath, cur.SourcePath)
	}
	if c.Volume != cur.Volume {
		return fmt.Sprintf("volume changed (%s -> %s)", c.Volume, cur.Volume)
	}
	if c.TotalSize != cur.TotalSize {
		return fmt.Sprintf("source size changed (%d -> %d bytes)", c.TotalSize, cur.TotalSize)
	}
	if c.ConfigHash != cur.ConfigHash {
		return "repository chunker/normalizer/encryption config changed"
	}
	// mtime is a hard check for regular-file inputs (see #42 §8-G).
	if c.SourceKind == "input" && c.SourceMTimeUnixNano != cur.SourceMTimeUnixNano {
		return fmt.Sprintf("input file mtime changed (%d -> %d)", c.SourceMTimeUnixNano, cur.SourceMTimeUnixNano)
	}
	// File mode: the fresh walk must reproduce the exact catalog (#51).
	if c.Mode == "file" && c.CatalogHash != cur.CatalogHash {
		return "source files changed since the backup was suspended (files added, removed, resized, or modified)"
	}
	return ""
}

// ConfigHash derives the chunker/normalizer/encryption identity hash from a
// repo config. Any change that would alter chunk boundaries or chunk identity
// changes this hash, so a resume against a re-configured repo is refused.
//
// The stored config is resolved first (#259). Callers hand this either the
// config as stored (the CLI passes store.LoadRepoConfig / the controller's
// repo record straight in) or a config rebuilt from the effective
// config.Config a reader already resolved (the agent does). Hashing the
// resolved form makes those two the same hash; hashing the raw form made a
// repo with unset fields produce one hash on the CLI and another on the
// agent, so a checkpoint written by one path was refused by the other.
// Effective is idempotent, so an already-resolved config hashes unchanged.
func ConfigHash(rc store.RepoConfig) [32]byte {
	rc = rc.Effective()
	h := sha256.New()
	fmt.Fprintf(h, "v1;min=%d;avg=%d;max=%d;mask=%d;pack=%d;zstd=%d;enc=%s;norm=%s",
		rc.ChunkMinSize, rc.ChunkAvgSize, rc.ChunkMaxSize, rc.BuzhashMask,
		rc.PackFileMaxSize, rc.CompressionLevel, rc.EffectiveEncryptionMode(),
		strings.Join(rc.Normalizers, ","))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// atomicWrite mirrors internal/storage/local.go's atomic WriteFile: temp file
// in the same dir, fsync, rename, then fsync the directory so the new dirent is
// durable. Presence of the final file == validity.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
