// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package forget

import (
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/config"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/index"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/prune"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/restore"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
	"github.com/SmithOperatingSolutions/disknexus-engine/volume"

	"log/slog"
)

func initRepo(t *testing.T) (string, config.Config) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	cfg := config.Default()
	if err := store.InitRepo(repo, store.RepoConfig{
		ChunkMinSize: cfg.ChunkMinSize, ChunkAvgSize: cfg.ChunkAvgSize, ChunkMaxSize: cfg.ChunkMaxSize,
		BuzhashMask: cfg.BuzhashMask, PackFileMaxSize: cfg.PackFileMaxSize, CompressionLevel: cfg.CompressionLevel,
	}); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}
	return repo, cfg
}

func saveMeta(t *testing.T, repo, id string, at time.Time, parent string, cat []manifest.FileEntry) {
	t.Helper()
	b := &manifest.Backup{BackupID: id, Timestamp: at, ParentBackupID: parent, BackupMode: "file", FileCatalog: cat}
	if err := b.Save(repo); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
}

// doVolumeBackup runs a real backup of data and returns its ID.
func doVolumeBackup(t *testing.T, repo string, cfg config.Config, data []byte) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "src.img")
	if err := os.WriteFile(src, data, 0644); err != nil {
		t.Fatal(err)
	}
	r, err := volume.NewReader(src, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	res, err := pipeline.New(cfg, quiet(), pipeline.MustBind(store.RepoConfig{}, nil)).Backup(context.Background(), r, src, r.Size(), repo)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return res.BackupID
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 15
func TestRun_RequireKeepRule_And_EmptyKeepSet_Refuse(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "b1", now.Add(-48*time.Hour), "", nil)
	saveMeta(t, repo, "b2", now.Add(-49*time.Hour), "", nil)

	// (a) no keep rule
	if _, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{}}); err == nil {
		t.Fatal("expected refusal with no keep rule")
	}
	// (b) within window that keeps nothing → empty keep set refused
	_, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{
		HasWithin: true, Within: CalDur{Hours: 1}, Now: now, Loc: time.UTC,
	}})
	if err == nil || !strings.Contains(err.Error(), "keep 0") {
		t.Fatalf("expected empty-keep-set refusal, got: %v", err)
	}
	// Nothing deleted either way.
	if ids, _ := manifest.ListIDs(repo); len(ids) != 2 {
		t.Fatalf("backups were deleted despite refusal: %v", ids)
	}
}

// 16
func TestRun_UnreadableManifest_RefusesUpFront(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "good", now, "", nil)
	// A corrupt .dnm: present (ListIDs sees it) but unparseable (List drops it).
	if err := os.WriteFile(filepath.Join(repo, "manifests", "corrupt.dnm"), []byte("not a dnm"), 0644); err != nil {
		t.Fatal(err)
	}
	var deleted int
	_, err := Run(context.Background(), Options{
		RepoPath: repo, Policy: Policy{Last: 1},
		DeleteFn: func(string) error { deleted++; return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("expected refusal on unreadable manifest, got: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("deleted %d despite refusal", deleted)
	}
}

// 17
func TestRun_DryRunReadOnly(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "old", now.Add(-72*time.Hour), "", nil)
	saveMeta(t, repo, "new", now, "", nil)

	before := snapshotDir(t, filepath.Join(repo, "manifests"))

	var deleteCalls, pruneCalls int
	plan, err := Run(context.Background(), Options{
		RepoPath: repo, Policy: Policy{Last: 1}, DryRun: true, Prune: true,
		DeleteFn: func(string) error { deleteCalls++; return nil },
		PruneFn:  func(context.Context, prune.Options) (*prune.Result, error) { pruneCalls++; return &prune.Result{}, nil },
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if len(plan.Remove) != 1 || plan.Remove[0] != "old" {
		t.Fatalf("plan.Remove = %v, want [old]", plan.Remove)
	}
	if deleteCalls != 0 || pruneCalls != 0 {
		t.Fatalf("dry run invoked delete=%d prune=%d, want 0/0", deleteCalls, pruneCalls)
	}
	after := snapshotDir(t, filepath.Join(repo, "manifests"))
	if before != after {
		t.Fatal("dry run mutated the manifests directory")
	}
}

// 18
func TestRun_DryRunPrune_NoPruneInvoked(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "old", now.Add(-72*time.Hour), "", nil)
	saveMeta(t, repo, "new", now, "", nil)
	pruneCalls := 0
	_, err := Run(context.Background(), Options{
		RepoPath: repo, Policy: Policy{Last: 1}, DryRun: true, Prune: true,
		PruneFn: func(context.Context, prune.Options) (*prune.Result, error) { pruneCalls++; return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruneCalls != 0 {
		t.Fatal("prune was invoked under --dry-run --prune")
	}
}

// 19
func TestRun_DeleteOrderNewestFirst(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	// A (old) is parent of B (new). Keep nothing extra: expire both by making a
	// third, newest backup the sole keep — but B references A, so promotion
	// keeps A,B; delete only the third if older... instead: three unrelated.
	saveMeta(t, repo, "aaold", now.Add(-72*time.Hour), "", nil)
	saveMeta(t, repo, "bbmid", now.Add(-48*time.Hour), "", nil)
	saveMeta(t, repo, "ccnew", now.Add(-1*time.Hour), "", nil)
	var order []string
	_, err := Run(context.Background(), Options{
		RepoPath: repo, Policy: Policy{Last: 1},
		DeleteFn: func(id string) error { order = append(order, id); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	// keep-last 1 keeps ccnew; delete bbmid then aaold (newest-first).
	want := []string{"bbmid", "aaold"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("delete order = %v, want %v (newest-first)", order, want)
	}
}

// 22
func TestRun_ProtectSupersetOfPolicy(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	// Chain: A <- B <- C. keep-last 1 keeps C; A,B must be promoted.
	saveMeta(t, repo, "A", now.Add(-72*time.Hour), "", nil)
	saveMeta(t, repo, "B", now.Add(-48*time.Hour), "A", nil)
	saveMeta(t, repo, "C", now.Add(-1*time.Hour), "B", nil)
	plan, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{Last: 1}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Remove) != 0 {
		t.Fatalf("chain must be fully protected, remove = %v", plan.Remove)
	}
	if len(plan.Promoted) != 2 {
		t.Fatalf("promoted = %v, want A and B", plan.Promoted)
	}
}

// 23
func TestRun_Strict_RefusesOnPromotion(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "A", now.Add(-72*time.Hour), "", nil)
	saveMeta(t, repo, "B", now.Add(-48*time.Hour), "A", nil)
	saveMeta(t, repo, "C", now.Add(-1*time.Hour), "B", nil)
	_, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{Last: 1}, Strict: true})
	if err == nil || !strings.Contains(err.Error(), "strict") {
		t.Fatalf("expected --strict refusal, got: %v", err)
	}
}

// 24
func TestRun_LegacyManifest_Supported(t *testing.T) {
	repo, _ := initRepo(t)
	now := time.Now()
	saveMeta(t, repo, "keepme", now, "", nil)
	saveMeta(t, repo, "expireme", now.Add(-72*time.Hour), "", nil)
	// Demote expireme to legacy .manifest form.
	full, err := manifest.Load(repo, "expireme")
	if err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, repo, "expireme", full)
	os.Remove(manifest.DNMPath(repo, "expireme"))

	plan, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{Last: 1}})
	if err != nil {
		t.Fatalf("forget with a legacy manifest: %v", err)
	}
	if len(plan.Deleted) != 1 || plan.Deleted[0] != "expireme" {
		t.Fatalf("legacy backup not deleted: %v", plan.Deleted)
	}
	if _, err := os.Stat(filepath.Join(repo, "manifests", "expireme.manifest")); err == nil {
		t.Fatal("legacy .manifest not removed")
	}
}

// 20 — the reference-chain trap, end to end.
func TestForgetThenPrune_PromotedAncestorRestores(t *testing.T) {
	repo, cfg := initRepo(t)

	data := make([]byte, 256*1024)
	rand.Read(data)
	aID := doVolumeBackup(t, repo, cfg, data) // A: real chunks

	aFull, err := manifest.Load(repo, aID)
	if err != nil {
		t.Fatal(err)
	}
	// B: a newer watcher-style file-mode backup whose unchanged file's data
	// lives in A (DataBackupID) and which is an incremental of A.
	bID := "0000000b-0000-0000-0000-00000000000b"
	b := &manifest.Backup{
		BackupID: bID, Timestamp: time.Now(), BackupMode: "file", BackupType: "incremental",
		ParentBackupID: aID, SourcePaths: []string{"src"},
		FileCatalog: []manifest.FileEntry{{
			Path: "f.dat", Size: int64(len(data)), StreamLength: int64(len(data)),
			Unchanged: true, DataBackupID: aID,
		}},
	}
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}

	// keep-last 1 keeps B (newest) and would expire A — but A is referenced.
	plan, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{Last: 1}, Prune: true, Key: nil})
	if err != nil {
		t.Fatalf("forget --prune: %v", err)
	}
	if len(plan.Deleted) != 0 {
		t.Fatalf("deleted a referenced ancestor: %v", plan.Deleted)
	}
	// A's manifest must survive and every one of A's chunks must still resolve.
	if _, err := manifest.Load(repo, aID); err != nil {
		t.Fatalf("promoted ancestor A gone after forget --prune: %v", err)
	}
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 0, cfg.BloomFPRate, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	for _, e := range aFull.Entries {
		if e.IsExcluded {
			continue
		}
		if _, found, err := idx.LookupDirect(e.ChunkHash); err != nil || !found {
			t.Fatalf("chunk %x from promoted ancestor A missing after prune (B would be unrestorable)", e.ChunkHash[:8])
		}
	}
}

// 21 — an isolated expired backup's chunks are actually reclaimed.
func TestForgetThenPrune_IsolatedBackup_Reclaims(t *testing.T) {
	repo, cfg := initRepo(t)

	dOld := make([]byte, 256*1024)
	rand.Read(dOld)
	dNew := make([]byte, 256*1024)
	rand.Read(dNew) // unrelated content → no shared chunks

	oldID := doVolumeBackup(t, repo, cfg, dOld)
	newID := doVolumeBackup(t, repo, cfg, dNew)
	// Make old actually older so keep-last 1 expires it.
	backdate(t, repo, oldID, time.Now().Add(-72*time.Hour))

	newFull, err := manifest.Load(repo, newID)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := Run(context.Background(), Options{RepoPath: repo, Policy: Policy{Last: 1}, Prune: true})
	if err != nil {
		t.Fatalf("forget --prune: %v", err)
	}
	if len(plan.Deleted) != 1 || plan.Deleted[0] != oldID {
		t.Fatalf("expected to delete only old backup, got %v", plan.Deleted)
	}
	if plan.Pruned == nil || plan.Pruned.BytesReclaimed <= 0 {
		t.Fatalf("expected reclaimed bytes, got %+v", plan.Pruned)
	}
	// The survivor still restores byte-for-byte.
	idx, err := index.NewDedupIndex(filepath.Join(repo, "index"), 0, cfg.BloomFPRate, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.CloseDiscard()
	cs, err := store.NewChunkStore(repo, cfg.PackFileMaxSize, cfg.CompressionLevel)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	outPath := filepath.Join(t.TempDir(), "out.img")
	wr, err := volume.NewWriter(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restore.NewRestorer(idx, cs, quiet()).Restore(context.Background(), newFull, wr); err != nil {
		wr.Close()
		t.Fatalf("restore survivor after prune: %v", err)
	}
	wr.Close()
	got, _ := os.ReadFile(outPath)
	if len(got) < len(dNew) || string(got[:len(dNew)]) != string(dNew) {
		t.Fatal("survivor restore not byte-identical after forget --prune")
	}
}

// --- helpers ---

func snapshotDir(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		lines = append(lines, e.Name()+":"+info.ModTime().String()+":"+itoa64(info.Size()))
	}
	sort.Strings(lines)
	return strings.Join(lines, "|")
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func backdate(t *testing.T, repo, id string, at time.Time) {
	t.Helper()
	b, err := manifest.Load(repo, id)
	if err != nil {
		t.Fatal(err)
	}
	b.Timestamp = at
	if err := b.Save(repo); err != nil {
		t.Fatal(err)
	}
}

func writeLegacy(t *testing.T, repo, id string, b *manifest.Backup) {
	t.Helper()
	// Minimal legacy .manifest JSON (metadata only is enough for forget policy).
	data := `{"backup_id":"` + id + `","timestamp":"` + b.Timestamp.Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(repo, "manifests", id+".manifest"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
