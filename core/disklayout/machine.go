// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package disklayout

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"time"
)

// MemberKind says how a partition's content was captured.
type MemberKind string

const (
	// MemberVolume: the partition holds a filesystem backed up through the
	// normal volume pipeline (VSS on Windows) — content addressed by BackupID.
	MemberVolume MemberKind = "volume"
	// MemberRaw: the partition (ESP, MSR, unrecognized) was captured as raw
	// ranged reads of the disk — content addressed by BackupID of a raw-range
	// backup whose stream is exactly the partition bytes.
	MemberRaw MemberKind = "raw"
	// MemberSkipped: deliberately not captured (e.g. swap); restored as zeros.
	MemberSkipped MemberKind = "skipped"
)

// PartitionMember maps one partition of a captured disk to its backing backup.
type PartitionMember struct {
	Index    int        `json:"index"` // GPT entry index (matches DiskLayout.Partitions)
	Kind     MemberKind `json:"kind"`
	BackupID string     `json:"backup_id,omitempty"` // empty for skipped
	Reason   string     `json:"reason,omitempty"`    // for skipped: why
}

// DiskCapture is one whole disk in a machine snapshot: its parsed layout, the
// verbatim GPT metadata regions, and the per-partition member backups.
type DiskCapture struct {
	// Source identifies the disk on the origin machine (informational):
	// "\\.\PhysicalDrive0", "/dev/sda", or an image path.
	Source string     `json:"source"`
	Layout DiskLayout `json:"layout"`
	// PrimaryGPT and BackupGPT are the verbatim bytes of the layout's
	// PrimaryRegion / BackupRegion, restored byte-exactly so disk signatures
	// and partition GUIDs (which BCD/fstab reference) are preserved.
	PrimaryGPT []byte `json:"primary_gpt"`
	BackupGPT  []byte `json:"backup_gpt"`
	// AuxBytes (#149): verbatim bytes of Layout.AuxRegions (the MBR EBR
	// chain lives in inter-partition gaps no member covers).
	AuxBytes [][]byte          `json:"aux_bytes,omitempty"`
	Members  []PartitionMember `json:"members"`
}

// MachineManifest groups one machine snapshot: every disk captured together,
// with identity for the recovery flow ("list machines → pick snapshot").
type MachineManifest struct {
	Version   int           `json:"version"`
	MachineID string        `json:"machine_id"` // stable per machine (agent/host identity)
	Hostname  string        `json:"hostname"`
	OS        string        `json:"os"`
	CreatedAt time.Time     `json:"created_at"`
	Disks     []DiskCapture `json:"disks"`
}

const machineManifestVersion = 1

// MachineManifestPath is the repo location of a machine snapshot manifest.
func MachineManifestPath(repoPath, snapshotID string) string {
	return filepath.Join(repoPath, "manifests", snapshotID+".machine.json")
}

// envelope mirrors the checkpoint pattern: body + CRC so a torn write is
// rejected rather than half-parsed.
type machineEnvelope struct {
	Body  json.RawMessage `json:"body"`
	CRC32 uint32          `json:"crc32"`
}

// MarshalMachineManifest validates and encodes a manifest in the CRC envelope
// used both for the local repo file and the S3 object (#70).
func MarshalMachineManifest(m *MachineManifest) ([]byte, error) {
	m.Version = machineManifestVersion
	if err := m.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal machine manifest: %w", err)
	}
	return json.Marshal(machineEnvelope{Body: body, CRC32: crc32.ChecksumIEEE(body)})
}

// UnmarshalMachineManifest decodes and validates an envelope-wrapped manifest.
func UnmarshalMachineManifest(data []byte) (*MachineManifest, error) {
	var env machineEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("unmarshal machine manifest envelope: %w", err)
	}
	if crc32.ChecksumIEEE(env.Body) != env.CRC32 {
		return nil, fmt.Errorf("machine manifest CRC mismatch")
	}
	var m MachineManifest
	if err := json.Unmarshal(env.Body, &m); err != nil {
		return nil, fmt.Errorf("unmarshal machine manifest: %w", err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// SaveMachineManifest atomically writes the manifest (temp + fsync + rename +
// dir sync — the repo-wide durability pattern).
func SaveMachineManifest(repoPath, snapshotID string, m *MachineManifest) error {
	data, err := MarshalMachineManifest(m)
	if err != nil {
		return err
	}
	path := MachineManifestPath(repoPath, snapshotID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
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

// LoadMachineManifest reads and validates a machine snapshot manifest.
func LoadMachineManifest(repoPath, snapshotID string) (*MachineManifest, error) {
	data, err := os.ReadFile(MachineManifestPath(repoPath, snapshotID))
	if err != nil {
		return nil, err
	}
	return UnmarshalMachineManifest(data)
}

// validate enforces the internal consistency a restore depends on: every
// member references an existing partition index, no partition is double-
// covered, non-skipped members carry a backup ID, and the verbatim GPT
// regions match the layout's declared lengths.
func (m *MachineManifest) validate() error {
	if len(m.Disks) == 0 {
		return fmt.Errorf("machine manifest has no disks")
	}
	for di, d := range m.Disks {
		byIndex := map[int]bool{}
		for _, p := range d.Layout.Partitions {
			byIndex[p.Index] = true
		}
		seen := map[int]bool{}
		for _, mem := range d.Members {
			if !byIndex[mem.Index] {
				return fmt.Errorf("disk %d: member references partition index %d not in layout", di, mem.Index)
			}
			if seen[mem.Index] {
				return fmt.Errorf("disk %d: partition index %d covered by multiple members", di, mem.Index)
			}
			seen[mem.Index] = true
			switch mem.Kind {
			case MemberVolume, MemberRaw:
				if mem.BackupID == "" {
					return fmt.Errorf("disk %d partition %d: %s member missing backup ID", di, mem.Index, mem.Kind)
				}
			case MemberSkipped:
			default:
				return fmt.Errorf("disk %d partition %d: unknown member kind %q", di, mem.Index, mem.Kind)
			}
		}
		// Every layout partition must be accounted for — a silently uncovered
		// partition would restore as garbage.
		for _, p := range d.Layout.Partitions {
			if !seen[p.Index] {
				return fmt.Errorf("disk %d: partition index %d (%s) has no member", di, p.Index, p.TypeName)
			}
		}
		if int64(len(d.PrimaryGPT)) != d.Layout.PrimaryRegion.Length {
			return fmt.Errorf("disk %d: primary GPT bytes %d != declared region %d", di, len(d.PrimaryGPT), d.Layout.PrimaryRegion.Length)
		}
		if int64(len(d.BackupGPT)) != d.Layout.BackupRegion.Length {
			return fmt.Errorf("disk %d: backup GPT bytes %d != declared region %d", di, len(d.BackupGPT), d.Layout.BackupRegion.Length)
		}
		if len(d.AuxBytes) != len(d.Layout.AuxRegions) {
			return fmt.Errorf("disk %d: aux byte blocks %d != declared aux regions %d", di, len(d.AuxBytes), len(d.Layout.AuxRegions))
		}
		for ai := range d.AuxBytes {
			if int64(len(d.AuxBytes[ai])) != d.Layout.AuxRegions[ai].Length {
				return fmt.Errorf("disk %d: aux region %d bytes %d != declared %d", di, ai, len(d.AuxBytes[ai]), d.Layout.AuxRegions[ai].Length)
			}
		}
	}
	return nil
}

// ListMachineManifests returns the snapshot IDs of all machine manifests in a
// repo (for the recovery flow's listing).
func ListMachineManifests(repoPath string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(repoPath, "manifests", "*.machine.json"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		base := filepath.Base(m)
		out = append(out, base[:len(base)-len(".machine.json")])
	}
	return out, nil
}
