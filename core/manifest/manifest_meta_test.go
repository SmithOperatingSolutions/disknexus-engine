// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package manifest

import (
	"runtime"
	"testing"
	"time"
)

func allocated() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.TotalAlloc
}

// TestLoadMetadata_ReadsTheHeaderNotTheEntries (#506): the parent-chain walk
// and the disk-member size check need a manifest's header. LoadMetadata
// answers from the DNM header and allocates nothing proportional to the
// entry count; the positive control, LoadForBlockRestore on the same file,
// allocates at least the entries — proving TotalAlloc sees what a full read
// costs.
func TestLoadMetadata_ReadsTheHeaderNotTheEntries(t *testing.T) {
	const n = 200_000
	repo := t.TempDir()
	b := &Backup{
		BackupID:       "cccccccc-0000-0000-0000-000000000001",
		ParentBackupID: "bbbbbbbb-0000-0000-0000-000000000001",
		SourceVolume:   `\\?\Volume{meta}`,
		Timestamp:      time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC),
		TotalBytes:     n * 4096,
		TotalChunks:    n,
		Entries:        make([]Entry, n),
	}
	for i := range b.Entries {
		b.Entries[i] = Entry{VolumeOffset: int64(i) * 4096, ChunkLength: 4096}
		b.Entries[i].ChunkHash[0] = byte(i)
	}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}

	before := allocated()
	meta, err := LoadMetadata(repo, b.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	metaAlloc := allocated() - before

	if meta.BackupID != b.BackupID || meta.ParentBackupID != b.ParentBackupID ||
		meta.SourceVolume != b.SourceVolume || meta.TotalBytes != b.TotalBytes ||
		meta.TotalChunks != b.TotalChunks || !meta.Timestamp.Equal(b.Timestamp) {
		t.Fatalf("header differs:\n got  %+v\n want %+v", headerOf(meta), headerOf(b))
	}
	if len(meta.Entries) != 0 {
		t.Fatalf("LoadMetadata returned %d entries; it must return none", len(meta.Entries))
	}

	before = allocated()
	full, err := LoadForBlockRestore(repo, b.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	fullAlloc := allocated() - before
	if len(full.Entries) != n {
		t.Fatalf("positive control loaded %d entries, want %d", len(full.Entries), n)
	}

	const entryBytes = 40 // below the 48-byte struct: the floor a full read cannot go under
	t.Logf("LoadMetadata allocated %d KB; LoadForBlockRestore allocated %d KB", metaAlloc>>10, fullAlloc>>10)
	if fullAlloc < n*entryBytes {
		t.Fatalf("positive control: a full load of %d entries allocated only %d KB — TotalAlloc is not seeing entry reads", n, fullAlloc>>10)
	}
	if metaAlloc > 1<<20 {
		t.Fatalf("LoadMetadata allocated %d KB for a %d-entry manifest — it is reading the entries", metaAlloc>>10, n)
	}
}

func headerOf(b *Backup) Backup {
	h := *b
	h.Entries = nil
	return h
}
