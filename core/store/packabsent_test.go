package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The ranged-chunk hook follows the same rule as the pack hook: an error
// wrapping ErrPackAbsent is loss (a plain error the verify judges per
// chunk), anything else is a FetchError that stops the walk.
func TestTheChunkFetchHookTellsAbsentFromUnreachable(t *testing.T) {
	dir := t.TempDir()
	if err := InitRepo(dir, RepoConfig{}); err != nil {
		t.Fatal(err)
	}
	cs, err := NewChunkStore(dir, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	_ = os.MkdirAll(filepath.Join(dir, "chunks"), 0o755)

	cs.OnChunkFetch = func(uint32, int64) ([]byte, error) { return nil, fmt.Errorf("404: %w", ErrPackAbsent) }
	_, err = cs.Retrieve(7, 0)
	var fe *FetchError
	if err == nil || errors.As(err, &fe) || !errors.Is(err, ErrPackAbsent) {
		t.Fatalf("an absent pack through the chunk hook came back as %v — want a plain loss, not a FetchError", err)
	}
	cs.OnChunkFetch = func(uint32, int64) ([]byte, error) { return nil, errors.New("dial tcp: i/o timeout") }
	_, err = cs.Retrieve(7, 0)
	if err == nil || !errors.As(err, &fe) || fe.Pack != 7 {
		t.Fatalf("an unreachable pack through the chunk hook came back as %v — want a FetchError naming pack 7", err)
	}
}
