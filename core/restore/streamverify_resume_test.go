// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package restore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/hasher"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// streamWorld stores n random chunks and describes them as one backup with
// a stream digest — what a server-side digest verify (#465) walks.
func streamWorld(t *testing.T, n int) (*manifest.Backup, *index.DedupIndex, *store.ChunkStore) {
	t.Helper()
	repo := t.TempDir()
	if err := store.InitRepo(repo, store.RepoConfig{}); err != nil {
		t.Fatal(err)
	}
	cs, err := store.NewChunkStore(repo, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 1000, 0.01, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { idx.CloseDiscard() })
	digest := sha256.New()
	b := &manifest.Backup{BackupID: "stream-verify"}
	var off int64
	for i := 0; i < n; i++ {
		data := make([]byte, 3000+i)
		rand.Read(data)
		pack, poff, _, err := cs.Store(data)
		if err != nil {
			t.Fatal(err)
		}
		idx.Insert(hasher.Sum(data), pack, uint64(poff), uint32(len(data)))
		digest.Write(data)
		b.Entries = append(b.Entries, manifest.Entry{VolumeOffset: off, ChunkHash: sha256.Sum256(data), ChunkLength: len(data)})
		off += int64(len(data))
	}
	b.TotalBytes = off
	b.ContentDigest = hex.EncodeToString(digest.Sum(nil))
	b.ContentDigestCovers = manifest.DigestCoversSourceStreamV1
	return b, idx, cs
}

// A server-side verify checkpoints mid-stream and resumes in another
// process (#465): the resumed walk continues the digest fold from the
// checkpoint and reaches a MATCH; a resume that starts anywhere but the
// checkpoint's offset is refused, because folding a gap or a repeat is a
// verdict about a different stream.
func TestStreamVerifyCheckpointResumesToTheSameVerdict(t *testing.T) {
	b, idx, cs := streamWorld(t, 40)
	ea := manifest.NewSliceEntryAccessor(b.Entries)
	total := int64(len(b.Entries))
	ctx := context.Background()

	sv, err := NewStreamVerify(b, total)
	if err != nil {
		t.Fatal(err)
	}
	if err := sv.Range(ctx, ea, 0, 17, idx, cs, nil, nil); err != nil {
		t.Fatal(err)
	}
	cp, err := sv.Checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if sv.Next() != 17 {
		t.Fatalf("Next = %d after ranging [0,17)", sv.Next())
	}

	// Positive control: an uninterrupted walk matches.
	whole, _ := NewStreamVerify(b, total)
	if err := whole.Range(ctx, ea, 0, total, idx, cs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if res := whole.Finish(); res.DigestVerdict != DigestMatch {
		t.Fatalf("uninterrupted verify: %q, want match — the fixture's digest is wrong", res.DigestVerdict)
	}

	resumed, err := ResumeStreamVerify(b, total, cp)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Next() != 17 {
		t.Fatalf("resumed Next = %d, want 17", resumed.Next())
	}
	if err := resumed.Range(ctx, ea, 18, total, idx, cs, nil, nil); err == nil {
		t.Fatal("a Range starting past the checkpoint was accepted — the fold would skip a chunk and still claim a verdict")
	}
	if err := resumed.Range(ctx, ea, 17, total, idx, cs, nil, nil); err != nil {
		t.Fatal(err)
	}
	res := resumed.Finish()
	if res.DigestVerdict != DigestMatch || res.VerifiedChunks != total || len(res.Errors) != 0 {
		t.Fatalf("resumed verify: verdict %q verified %d errors %d, want match/%d/0", res.DigestVerdict, res.VerifiedChunks, len(res.Errors), total)
	}

	// A partial walk that never resumed makes no claim.
	partial, _ := NewStreamVerify(b, total)
	partial.Range(ctx, ea, 0, 5, idx, cs, nil, nil)
	if res := partial.Finish(); res.DigestVerdict != "" {
		t.Fatalf("a partial walk claimed %q", res.DigestVerdict)
	}
	// Garbage is not a checkpoint.
	if _, err := ResumeStreamVerify(b, total, []byte("{not json")); err == nil {
		t.Fatal("a corrupt checkpoint resumed")
	}
}

// A walk that met a chunk error has aborted its fold; a checkpoint of it
// would resume hours of work toward a verdict Finish will refuse.
func TestStreamVerifyRefusesToCheckpointAfterAnError(t *testing.T) {
	b, idx, cs := streamWorld(t, 6)
	b.Entries[2].ChunkHash[0] ^= 0xff // this chunk will not verify
	ea := manifest.NewSliceEntryAccessor(b.Entries)
	sv, _ := NewStreamVerify(b, int64(len(b.Entries)))
	if err := sv.Range(context.Background(), ea, 0, 4, idx, cs, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := sv.Checkpoint(); err == nil {
		t.Fatal("a walk with a chunk error handed out a checkpoint")
	}
	if res := sv.Finish(); len(res.Errors) == 0 || res.DigestVerdict == DigestMatch {
		t.Fatalf("errors=%d verdict=%q — the corrupt chunk went unnoticed", len(res.Errors), res.DigestVerdict)
	}
}
