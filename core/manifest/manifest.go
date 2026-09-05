// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// VolumeExtent maps a region of a file to its physical location on the volume.
type VolumeExtent struct {
	FileOffset   int64 `json:"file_offset"`   // byte offset within the file's logical data
	VolumeOffset int64 `json:"volume_offset"` // byte offset on the raw volume
	Length       int64 `json:"length"`        // bytes in this extent
}

// FileEntry describes a single file or directory in a file-mode backup.
type FileEntry struct {
	Path          string         `json:"path"`                     // forward-slash relative path
	SourceIndex   int            `json:"source_index"`             // index into Backup.SourcePaths
	Size          int64          `json:"size"`                     // file size in bytes
	Mode          uint32         `json:"mode"`                     // os.FileMode as uint32
	ModTime       time.Time      `json:"mod_time"`                 // last modification time
	IsDir         bool           `json:"is_dir,omitempty"`         // directory
	IsSymlink     bool           `json:"is_symlink,omitempty"`     // symbolic link
	LinkTarget    string         `json:"link_target,omitempty"`    // symlink target
	StreamOffset  int64          `json:"stream_offset"`            // byte offset in concatenated stream
	StreamLength  int64          `json:"stream_length"`            // bytes in stream (0 for dirs/symlinks)
	ContentHash   [32]byte       `json:"content_hash,omitempty"`   // SHA-256 of covering chunk hashes
	Unchanged     bool           `json:"unchanged,omitempty"`      // true = file not re-backed-up (watcher mode)
	DataBackupID  string         `json:"data_backup_id,omitempty"` // backup containing this file's chunk data
	VolumeExtents []VolumeExtent `json:"volume_extents,omitempty"` // physical extents for volume file catalog
	InlineData    []byte         `json:"inline_data,omitempty"`    // content of NTFS resident files (no cluster extents)
	// IsExcluded marks a file whose blocks were deliberately zeroed in the
	// block capture (volatile files, repo/temp dirs) — cataloged for
	// completeness but not restorable (#94).
	IsExcluded bool `json:"is_excluded,omitempty"`
}

// MarshalJSON handles the [32]byte ContentHash as hex in JSON.
func (f FileEntry) MarshalJSON() ([]byte, error) {
	type Alias struct {
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
		ContentHash   string         `json:"content_hash,omitempty"`
		Unchanged     bool           `json:"unchanged,omitempty"`
		DataBackupID  string         `json:"data_backup_id,omitempty"`
		VolumeExtents []VolumeExtent `json:"volume_extents,omitempty"`
		InlineData    []byte         `json:"inline_data,omitempty"`
		IsExcluded    bool           `json:"is_excluded,omitempty"`
	}
	hashStr := ""
	var zero [32]byte
	if f.ContentHash != zero {
		hashStr = fmt.Sprintf("%x", f.ContentHash)
	}
	return json.Marshal(Alias{
		Path:          f.Path,
		SourceIndex:   f.SourceIndex,
		Size:          f.Size,
		Mode:          f.Mode,
		ModTime:       f.ModTime,
		IsDir:         f.IsDir,
		IsSymlink:     f.IsSymlink,
		LinkTarget:    f.LinkTarget,
		StreamOffset:  f.StreamOffset,
		StreamLength:  f.StreamLength,
		ContentHash:   hashStr,
		Unchanged:     f.Unchanged,
		DataBackupID:  f.DataBackupID,
		VolumeExtents: f.VolumeExtents,
		InlineData:    f.InlineData,
		IsExcluded:    f.IsExcluded,
	})
}

// UnmarshalJSON handles hex-encoded ContentHash from JSON.
func (f *FileEntry) UnmarshalJSON(data []byte) error {
	type Alias struct {
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
		ContentHash   string         `json:"content_hash,omitempty"`
		Unchanged     bool           `json:"unchanged,omitempty"`
		DataBackupID  string         `json:"data_backup_id,omitempty"`
		VolumeExtents []VolumeExtent `json:"volume_extents,omitempty"`
		InlineData    []byte         `json:"inline_data,omitempty"`
		IsExcluded    bool           `json:"is_excluded,omitempty"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	f.Path = a.Path
	f.SourceIndex = a.SourceIndex
	f.Size = a.Size
	f.Mode = a.Mode
	f.ModTime = a.ModTime
	f.IsDir = a.IsDir
	f.IsSymlink = a.IsSymlink
	f.LinkTarget = a.LinkTarget
	f.StreamOffset = a.StreamOffset
	f.StreamLength = a.StreamLength
	f.Unchanged = a.Unchanged
	f.DataBackupID = a.DataBackupID
	f.VolumeExtents = a.VolumeExtents
	f.InlineData = a.InlineData
	f.IsExcluded = a.IsExcluded

	if len(a.ContentHash) == 64 {
		decoded, err := hex.DecodeString(a.ContentHash)
		if err != nil {
			return fmt.Errorf("parsing content hash: %w", err)
		}
		copy(f.ContentHash[:], decoded)
	}
	return nil
}

// Entry maps a volume offset to a chunk in the store.
type Entry struct {
	VolumeOffset int64    `json:"volume_offset"`
	ChunkHash    [32]byte `json:"chunk_hash"`
	ChunkLength  int      `json:"chunk_length"`
	IsExcluded   bool     `json:"is_excluded,omitempty"`
}

// EntryRecordSize is the fixed on-disk size of one binary entry record.
// [0:8] VolumeOffset int64 LE | [8:40] ChunkHash [32]byte | [40:44] ChunkLength int32 LE | [44] IsExcluded byte
const EntryRecordSize = 45

// EntriesPath returns the path to the binary entries sidecar for a backup.
func EntriesPath(repoPath, backupID string) string {
	return filepath.Join(repoPath, "manifests", backupID+".entries")
}

// EntryWriter streams entry records to the binary sidecar file one at a time,
// using a 1 MiB write buffer to amortize syscall overhead over millions of entries.
type EntryWriter struct {
	f  *os.File
	bw *bufio.Writer
}

// OpenEntryWriter creates (or overwrites) the binary entries sidecar for backupID.
func OpenEntryWriter(repoPath, backupID string) (*EntryWriter, error) {
	path := EntriesPath(repoPath, backupID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("creating manifests dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating entries file: %w", err)
	}
	return &EntryWriter{f: f, bw: bufio.NewWriterSize(f, 1<<20)}, nil
}

// WriteEntry appends one entry to the sidecar.
func (w *EntryWriter) WriteEntry(e Entry) error {
	var buf [EntryRecordSize]byte
	binary.LittleEndian.PutUint64(buf[0:8], uint64(e.VolumeOffset))
	copy(buf[8:40], e.ChunkHash[:])
	binary.LittleEndian.PutUint32(buf[40:44], uint32(e.ChunkLength))
	if e.IsExcluded {
		buf[44] = 1
	}
	_, err := w.bw.Write(buf[:])
	return err
}

// Close flushes buffered data and closes the sidecar file.
func (w *EntryWriter) Close() error {
	if err := w.bw.Flush(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}

// Sync flushes the write buffer and fsyncs the sidecar so every entry written
// so far is durable on disk. A resume checkpoint (#42) records the sidecar
// length only after Sync, so the recorded length never counts buffered entries.
func (w *EntryWriter) Sync() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

// ReadAt reads previously written (and flushed) sidecar bytes at off. Used by
// resume checkpointing to extract the delta since the prior checkpoint (#55).
func (w *EntryWriter) ReadAt(p []byte, off int64) (int, error) {
	return w.f.ReadAt(p, off)
}

// Len flushes the buffer and returns the authoritative on-disk byte length of
// the sidecar (a multiple of EntryRecordSize). This is the value recorded as a
// resume checkpoint's EntriesLen — never a buffered logical count.
func (w *EntryWriter) Len() (int64, error) {
	if err := w.bw.Flush(); err != nil {
		return 0, err
	}
	info, err := w.f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// OpenEntryWriterResume reopens an existing sidecar to append during a resume
// (#42). It truncates the file down to exactly wantLen — dropping any
// un-checkpointed tail records that belonged to now-discarded packs — and
// appends from there. It NEVER grows the file: if the on-disk sidecar is
// shorter than wantLen the checkpoint is inconsistent with the sidecar, so it
// refuses rather than zero-extending (which would fabricate empty records that
// restore would read as bogus entries).
func OpenEntryWriterResume(repoPath, backupID string, wantLen int64) (*EntryWriter, error) {
	if wantLen < 0 || wantLen%EntryRecordSize != 0 {
		return nil, fmt.Errorf("resume entries length %d is not a multiple of %d", wantLen, EntryRecordSize)
	}
	path := EntriesPath(repoPath, backupID)
	f, err := os.OpenFile(path, os.O_RDWR, 0644) // never O_CREATE/O_TRUNC: must not clobber
	if err != nil {
		return nil, fmt.Errorf("reopening entries file for resume: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() < wantLen {
		f.Close()
		return nil, fmt.Errorf("entries sidecar is %d bytes but checkpoint expects >= %d; refusing to zero-extend", info.Size(), wantLen)
	}
	if err := f.Truncate(wantLen); err != nil {
		f.Close()
		return nil, fmt.Errorf("truncating entries to %d: %w", wantLen, err)
	}
	if _, err := f.Seek(wantLen, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("seeking entries to %d: %w", wantLen, err)
	}
	return &EntryWriter{f: f, bw: bufio.NewWriterSize(f, 1<<20)}, nil
}

// WriteEntries writes all entries to the binary sidecar in one call.
func WriteEntries(repoPath, backupID string, entries []Entry) error {
	w, err := OpenEntryWriter(repoPath, backupID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := w.WriteEntry(e); err != nil {
			w.Close()
			return fmt.Errorf("writing entry: %w", err)
		}
	}
	return w.Close()
}

// ReadEntries loads all entries from the binary sidecar.
// Returns nil, nil when no sidecar exists (old-format manifest or empty backup).
func ReadEntries(repoPath, backupID string) ([]Entry, error) {
	path := EntriesPath(repoPath, backupID)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening entries file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat entries file: %w", err)
	}
	entries := make([]Entry, 0, info.Size()/EntryRecordSize)

	br := bufio.NewReaderSize(f, 1<<20)
	var buf [EntryRecordSize]byte
	for {
		if _, err := io.ReadFull(br, buf[:]); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("reading entry record: %w", err)
		}
		var e Entry
		e.VolumeOffset = int64(binary.LittleEndian.Uint64(buf[0:8]))
		copy(e.ChunkHash[:], buf[8:40])
		e.ChunkLength = int(binary.LittleEndian.Uint32(buf[40:44]))
		e.IsExcluded = buf[44] != 0
		entries = append(entries, e)
	}
	return entries, nil
}

// Backup describes a complete backup operation.
// DigestCoversSourceStreamV1 names what a #455 content digest is folded
// over. Any change to the folding is a NEW covers value, not an edit.
const DigestCoversSourceStreamV1 = "source-stream/v1"

type Backup struct {
	BackupID     string    `json:"backup_id"`
	Timestamp    time.Time `json:"timestamp"`
	SourceVolume string    `json:"source_volume"`
	SectorSize   int       `json:"sector_size"`
	ClusterSize  int       `json:"cluster_size"`
	TotalBytes   int64     `json:"total_bytes"`
	Entries      []Entry   `json:"entries,omitempty"`

	// Incremental backup fields
	ParentBackupID string `json:"parent_backup_id,omitempty"`
	BackupType     string `json:"backup_type,omitempty"` // "full" or "incremental"

	// Stats
	TotalChunks  int64   `json:"total_chunks"`
	UniqueChunks int64   `json:"unique_chunks"`
	DedupChunks  int64   `json:"dedup_chunks"`
	RawBytes     int64   `json:"raw_bytes"`
	StoredBytes  int64   `json:"stored_bytes"`
	DedupRatio   float64 `json:"dedup_ratio"`
	CompRatio    float64 `json:"compression_ratio"`
	Duration     string  `json:"duration"`

	// Content digest (#455): hex SHA-256 over the captured source stream —
	// original bytes, post exclusion-zeroing, pre compression/encryption, in
	// offset order — which is byte-identical to what a full restore writes.
	// Covers records the definition, so a future change to what is folded
	// cannot silently invalidate stored digests. Empty on backups captured
	// before #455: verification must report those NOT VERIFIABLE, never
	// failed, and never passed.
	ContentDigest       string `json:"content_digest,omitempty"`
	ContentDigestCovers string `json:"content_digest_covers,omitempty"`

	// ExcludePaths (#468): the operator-configured exclusions this capture
	// was given, in their canonical form (`C:\Users\x\VMs`), whether or not
	// each resolved. A file whose blocks were zeroed can then be explained
	// at restore as "your exclusion", not just "excluded". Empty on backups
	// captured with none, and on every manifest written before #468.
	ExcludePaths []string `json:"exclude_paths,omitempty"`
	// ExcludeWarnings (#468): one line per configured exclusion that did
	// NOT apply to this capture — not found on the volume, non-NTFS, walk
	// failed — so "its data is in this backup" is recorded where the backup
	// is, not only in a log that scrolled away. Empty when every exclusion
	// applied (or none was configured).
	ExcludeWarnings []string `json:"exclude_warnings,omitempty"`

	// Incremental stats
	ChangedChunks   int64 `json:"changed_chunks,omitempty"`
	UnchangedChunks int64 `json:"unchanged_chunks,omitempty"`

	// File-mode backup fields (omitempty for backward compatibility)
	BackupMode  string      `json:"backup_mode,omitempty"`  // "volume" or "file"
	SourcePaths []string    `json:"source_paths,omitempty"` // root directories backed up
	FileCatalog []FileEntry `json:"file_catalog,omitempty"` // file metadata catalog

	// CatalogSidecarPath is a transient field set by the pipeline when the
	// catalog has been pre-serialized to a temp file to avoid holding the full
	// []FileEntry in memory during the chunk phase. saveDNM streams from this
	// file instead of encoding FileCatalog. Never persisted to disk.
	CatalogSidecarPath string `json:"-"`

	// CatalogCount is a transient field set by List from the .dnm catalog
	// section header, so metadata-only listings (list/stats) can tell whether a
	// backup carries a file catalog without loading every FileEntry. It is not
	// encoded into the .dnm itself; readMetadata leaves it zero, and Load
	// populates the full FileCatalog instead.
	CatalogCount int64 `json:"-"`

	// Managed encryption: ECIES-wrapped AES-256 DEK (92 bytes)
	WrappedDEK []byte `json:"wrapped_dek,omitempty"`
}

// NewBackupID generates a new unique backup identifier.
func NewBackupID() string {
	return uuid.New().String()
}

// Save writes the manifest to the manifests directory as a .dnm binary file.
// If b.Entries is non-nil (e.g. in tests), they are embedded directly.
// If b.Entries is nil, saveDNM copies the .entries sidecar written by the
// pipeline's OpenEntryWriter, then removes it (it has no further purpose).
func (b *Backup) Save(repoPath string) error {
	dir := filepath.Join(repoPath, "manifests")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating manifests dir: %w", err)
	}

	if err := saveDNM(repoPath, b); err != nil {
		return fmt.Errorf("writing dnm manifest: %w", err)
	}

	// The pipeline streams entries to the .entries sidecar before calling Save().
	// saveDNM() has already embedded that data into the .dnm file; delete the sidecar.
	os.Remove(EntriesPath(repoPath, b.BackupID))

	return nil
}

// Load reads a manifest from disk, populating Entries and FileCatalog.
//
// Preference order:
//  1. .dnm binary format (if present) — fastest, enables future streaming
//  2. .manifest JSON + .entries sidecar — legacy fallback
//  3. .manifest JSON with embedded entries — old format without sidecar
func Load(repoPath, backupID string) (*Backup, error) {
	// Prefer the .dnm binary format when available.
	if _, err := os.Stat(DNMPath(repoPath, backupID)); err == nil {
		return loadDNM(repoPath, backupID)
	}

	// Legacy path: JSON manifest + binary sidecar.
	path := filepath.Join(repoPath, "manifests", backupID+".manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	if len(b.Entries) == 0 {
		entries, err := ReadEntries(repoPath, backupID)
		if err != nil {
			return nil, fmt.Errorf("reading entries sidecar: %w", err)
		}
		b.Entries = entries
	}

	return &b, nil
}

// LoadCatalog reads a manifest's metadata and file catalog WITHOUT loading its
// chunk entries. For .dnm backups this skips the (potentially GB-scale) ENTRIES
// section entirely; callers that only need FileCatalog/metadata (e.g. prune's
// DataBackupID cross-reference walk) should use this instead of Load. Legacy
// JSON manifests fall back to a plain parse without the entries sidecar.
func LoadCatalog(repoPath, backupID string) (*Backup, error) {
	dnmPath := DNMPath(repoPath, backupID)
	if _, err := os.Stat(dnmPath); err == nil {
		r, err := OpenDNMReader(dnmPath)
		if err != nil {
			return nil, fmt.Errorf("opening dnm file: %w", err)
		}
		defer r.Close()

		b, err := r.readMetadata()
		if err != nil {
			return nil, fmt.Errorf("reading dnm metadata: %w", err)
		}
		if r.catalog.count > 0 {
			b.FileCatalog, err = r.readAllCatalog()
			if err != nil {
				return nil, fmt.Errorf("reading dnm catalog: %w", err)
			}
		}
		return &b, nil
	}

	// Legacy JSON: the catalog (if any) is embedded in the JSON itself; the
	// .entries sidecar is deliberately not read, and any entries embedded in
	// the JSON are dropped so the "no entries" contract holds for callers.
	path := filepath.Join(repoPath, "manifests", backupID+".manifest")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	var b Backup
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	b.Entries = nil
	return &b, nil
}

// nopCloser is a no-op io.Closer used when no resource cleanup is needed.
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// NewEntryAccessor opens an accessor for the entry records of the given backup.
// The returned io.Closer must be called when the accessor is no longer needed.
//
// If a .dnm file is present, the accessor reads directly from it using O(log n)
// seeks per lookup — no entries are loaded into memory. Otherwise it falls back
// to the binary sidecar (.entries file), loading all records into a slice.
func NewEntryAccessor(repoPath, backupID string) (EntryAccessor, io.Closer, error) {
	dnmPath := DNMPath(repoPath, backupID)
	if _, err := os.Stat(dnmPath); err == nil {
		r, err := OpenDNMReader(dnmPath)
		if err != nil {
			return nil, nil, fmt.Errorf("opening dnm reader for %s: %w", backupID, err)
		}
		return NewDNMEntryAccessor(r), r, nil
	}
	entries, err := ReadEntries(repoPath, backupID)
	if err != nil {
		return nil, nil, fmt.Errorf("reading entries for %s: %w", backupID, err)
	}
	return NewSliceEntryAccessor(entries), nopCloser{}, nil
}

// loadDNM reads a backup from its .dnm file, loading all sections.
// LoadForBlockRestore loads a backup WITHOUT decoding the file catalog
// (#153): disk/image restores consume only metadata + entries, and a
// 580k-file catalog (147 MB in the field) was being materialized — and
// held — for nothing. Also tolerant of catalog-section corruption, since
// the section is never read.
func LoadForBlockRestore(repoPath, backupID string) (*Backup, error) {
	if _, err := os.Stat(DNMPath(repoPath, backupID)); err != nil {
		return Load(repoPath, backupID) // legacy JSON path: no section skipping
	}
	r, err := OpenDNMReader(DNMPath(repoPath, backupID))
	if err != nil {
		return nil, fmt.Errorf("opening dnm file: %w", err)
	}
	defer r.Close()
	b, err := r.readMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading dnm metadata: %w", err)
	}
	b.Entries, err = r.readAllEntries()
	if err != nil {
		return nil, fmt.Errorf("reading dnm entries: %w", err)
	}
	return &b, nil
}

func loadDNM(repoPath, backupID string) (*Backup, error) {
	r, err := OpenDNMReader(DNMPath(repoPath, backupID))
	if err != nil {
		return nil, fmt.Errorf("opening dnm file: %w", err)
	}
	defer r.Close()

	b, err := r.readMetadata()
	if err != nil {
		return nil, fmt.Errorf("reading dnm metadata: %w", err)
	}

	if r.catalog.count > 0 {
		b.FileCatalog, err = r.readAllCatalog()
		if err != nil {
			return nil, fmt.Errorf("reading dnm catalog: %w", err)
		}
	}

	b.Entries, err = r.readAllEntries()
	if err != nil {
		return nil, fmt.Errorf("reading dnm entries: %w", err)
	}

	return &b, nil
}

// List returns all backup manifests in the repository, sorted by timestamp.
// Prefers .dnm files (metadata-only seek, no JSON parse); falls back to
// .manifest JSON for legacy backups that have not been migrated.
func List(repoPath string) ([]Backup, error) {
	dir := filepath.Join(repoPath, "manifests")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading manifests dir: %w", err)
	}

	// Collect unique backupIDs from both .dnm and .manifest files.
	seen := make(map[string]bool)
	var ids []string
	for _, e := range dirEntries {
		name := e.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ".dnm"):
			id = strings.TrimSuffix(name, ".dnm")
		case filepath.Ext(name) == ".manifest" && !strings.HasSuffix(name, ".manifest.bak"):
			id = strings.TrimSuffix(name, ".manifest")
		default:
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	var manifests []Backup
	for _, id := range ids {
		dnmPath := DNMPath(repoPath, id)
		if _, statErr := os.Stat(dnmPath); statErr == nil {
			// Fast path: read only the metadata section from .dnm.
			r, openErr := OpenDNMReader(dnmPath)
			if openErr != nil {
				continue
			}
			b, readErr := r.readMetadata()
			// Record the catalog size cheaply from the section header so
			// list/stats can report file counts without loading every entry.
			b.CatalogCount = r.CatalogCount()
			r.Close()
			if readErr != nil {
				continue
			}
			manifests = append(manifests, b)
		} else {
			// Legacy path: read .manifest JSON.
			path := filepath.Join(dir, id+".manifest")
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			var b Backup
			if err := json.Unmarshal(data, &b); err != nil {
				continue
			}
			b.CatalogCount = int64(len(b.FileCatalog))
			manifests = append(manifests, b)
		}
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].Timestamp.Before(manifests[j].Timestamp)
	})

	return manifests, nil
}

// ResolveID resolves a partial backup ID prefix to a full backup ID.
// Returns an error if the prefix is ambiguous (matches multiple backups).
// Scans both .dnm and .manifest files, deduplicating by backupID.
func ResolveID(repoPath, partialID string) (string, error) {
	dir := filepath.Join(repoPath, "manifests")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading manifests dir: %w", err)
	}

	seen := make(map[string]bool)
	var matches []string
	for _, e := range dirEntries {
		name := e.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ".dnm"):
			id = strings.TrimSuffix(name, ".dnm")
		case filepath.Ext(name) == ".manifest" && !strings.HasSuffix(name, ".manifest.bak"):
			id = strings.TrimSuffix(name, ".manifest")
		default:
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		if strings.HasPrefix(id, partialID) {
			matches = append(matches, id)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no backup found matching prefix %q", partialID)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d backups", partialID, len(matches))
	}
}

// Delete removes a manifest and all associated files from the repository.
// Note: chunks are not reclaimed until GC is implemented.
func Delete(repoPath, backupID string) error {
	dnmPath := DNMPath(repoPath, backupID)
	manifestPath := filepath.Join(repoPath, "manifests", backupID+".manifest")

	dnmErr := os.Remove(dnmPath)
	manifestErr := os.Remove(manifestPath)

	// Require at least one of .dnm or .manifest to exist.
	if dnmErr != nil && !os.IsNotExist(dnmErr) {
		return fmt.Errorf("deleting dnm manifest: %w", dnmErr)
	}
	if manifestErr != nil && !os.IsNotExist(manifestErr) {
		return fmt.Errorf("deleting manifest: %w", manifestErr)
	}
	if os.IsNotExist(dnmErr) && os.IsNotExist(manifestErr) {
		return fmt.Errorf("no manifest found for backup %s", backupID)
	}

	// Remove sidecar files; ignore errors since they may not exist.
	os.Remove(EntriesPath(repoPath, backupID))
	return nil
}

// Get resolves a partial ID and loads the manifest.
func Get(repoPath, partialID string) (*Backup, error) {
	fullID, err := ResolveID(repoPath, partialID)
	if err != nil {
		return nil, err
	}
	return Load(repoPath, fullID)
}

// MarshalJSON handles the [32]byte chunk hash as hex in JSON.
func (e Entry) MarshalJSON() ([]byte, error) {
	type Alias struct {
		VolumeOffset int64  `json:"volume_offset"`
		ChunkHash    string `json:"chunk_hash"`
		ChunkLength  int    `json:"chunk_length"`
		IsExcluded   bool   `json:"is_excluded,omitempty"`
	}
	return json.Marshal(Alias{
		VolumeOffset: e.VolumeOffset,
		ChunkHash:    fmt.Sprintf("%x", e.ChunkHash),
		ChunkLength:  e.ChunkLength,
		IsExcluded:   e.IsExcluded,
	})
}

// UnmarshalJSON handles hex-encoded chunk hash from JSON.
func (e *Entry) UnmarshalJSON(data []byte) error {
	type Alias struct {
		VolumeOffset int64  `json:"volume_offset"`
		ChunkHash    string `json:"chunk_hash"`
		ChunkLength  int    `json:"chunk_length"`
		IsExcluded   bool   `json:"is_excluded,omitempty"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	e.VolumeOffset = a.VolumeOffset
	e.ChunkLength = a.ChunkLength
	e.IsExcluded = a.IsExcluded

	if len(a.ChunkHash) == 64 {
		decoded, err := hex.DecodeString(a.ChunkHash)
		if err != nil {
			return fmt.Errorf("parsing chunk hash: %w", err)
		}
		copy(e.ChunkHash[:], decoded)
	}
	return nil
}

// ListIDs returns every backup ID that has a .dnm or .manifest file in the
// repository, by a raw directory scan — it never parses a manifest, so unlike
// List it cannot silently drop a corrupt-but-present backup. Callers that must
// see EVERY backup (prune, forget) source their ID set from here and cross-check
// against List to detect unreadable manifests.
func ListIDs(repoPath string) ([]string, error) {
	dir := filepath.Join(repoPath, "manifests")
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading manifests dir: %w", err)
	}

	seen := make(map[string]bool)
	var ids []string
	for _, e := range dirEntries {
		name := e.Name()
		var id string
		switch {
		case strings.HasSuffix(name, ".dnm"):
			id = strings.TrimSuffix(name, ".dnm")
		case strings.HasSuffix(name, ".manifest") && !strings.HasSuffix(name, ".manifest.bak"):
			id = strings.TrimSuffix(name, ".manifest")
		default:
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}
