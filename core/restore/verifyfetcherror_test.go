package restore

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/SmithOperatingSolutions/disknexus-engine/core/manifest"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// A pack that cannot be FETCHED is not a corrupt chunk. A walk whose pack
// download fails (the network died, the run was cancelled) must return the
// fetch error — no chunk error recorded, the digest fold intact, the
// checkpoint still writable — so the caller keeps its checkpoint and the
// item is re-queued rather than filed as "N chunk errors". Positive
// control: a pack that IS present but corrupt still yields a chunk error.
func TestAFetchFailureIsNotAChunkError(t *testing.T) {
	b, idx, cs := streamWorldPacked(t, 40, 8192) // ~2 chunks per pack: pack 0 is sealed, no writer holds it
	ea := manifest.NewSliceEntryAccessor(b.Entries)
	total := int64(len(b.Entries))
	ctx := context.Background()

	// Control: corrupt a pack in place; the walk reports chunk errors.
	corrupt, _ := NewStreamVerify(b, total)
	packPath := cs.PackPath(0)
	orig, err := os.ReadFile(packPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), orig...)
	for i := 16; i < len(bad); i++ {
		bad[i] ^= 0xff
	}
	if err := os.WriteFile(packPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Range(ctx, ea, 0, total, idx, cs, nil, nil); err != nil {
		t.Fatalf("control: a corrupt pack ended the walk with an error instead of chunk errors: %v", err)
	}
	if res := corrupt.Finish(); len(res.Errors) == 0 {
		t.Fatal("control: a corrupt pack produced no chunk error")
	}
	if err := os.WriteFile(packPath, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	// The pack is gone and its download fails: a transport failure.
	errNet := errors.New("dial tcp: network is unreachable")
	if err := os.Remove(packPath); err != nil {
		t.Fatal(err)
	}
	cs.OnPackMissing = func(uint32) error { return errNet }
	sv, _ := NewStreamVerify(b, total)
	err = sv.Range(ctx, ea, 0, total, idx, cs, nil, nil)
	if err == nil {
		res := sv.Finish()
		t.Fatalf("a failed pack download was reported as %d chunk errors (verdict %q) — a dead network reads as corruption", len(res.Errors), res.DigestVerdict)
	}
	if !errors.Is(err, errNet) {
		t.Fatalf("the walk's error does not carry the fetch failure: %v", err)
	}
	var fe *store.FetchError
	if !errors.As(err, &fe) || fe.Pack != 0 {
		t.Fatalf("the walk's error is not a store.FetchError naming pack 0: %v", err)
	}
	if _, cerr := sv.Checkpoint(); cerr != nil {
		t.Fatalf("after a fetch failure the walk refuses to checkpoint: %v — the caller loses its resume point", cerr)
	}
}

// The sampled path is the same rule: a pack that cannot be fetched ends
// VerifySelected with the fetch error, not a chunk-error verdict.
func TestASampledVerifyReturnsAFetchFailureAsAnError(t *testing.T) {
	b, idx, cs := streamWorldPacked(t, 40, 8192)
	errNet := errors.New("dial tcp: i/o timeout")
	if err := os.Remove(cs.PackPath(0)); err != nil {
		t.Fatal(err)
	}
	cs.OnPackMissing = func(uint32) error { return errNet }
	res, err := VerifySelectedWithNormalizer(context.Background(), b, idx, cs, nil, []int{0, 1, 2})
	if err == nil {
		t.Fatalf("a sampled verify filed a failed download as a verdict: %+v", res)
	}
	if !errors.Is(err, errNet) {
		t.Fatalf("the sampled verify's error does not carry the fetch failure: %v", err)
	}
}
