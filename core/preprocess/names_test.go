// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package preprocess

import (
	"bytes"
	"reflect"
	"testing"
)

// The normalizer names in a repo's config are how every read path rebuilds
// the exact normalizer the backup hashed with: chunk identity is the hash of
// NORMALIZED bytes while the stored bytes are the originals, so a name that
// round-trips to a different normalizer makes every chunk fail verification.
func TestNormalizerNamesRoundTripExactly(t *testing.T) {
	cases := []struct {
		n     Normalizer
		names []string
	}{
		{nil, nil},
		{&NoopNormalizer{}, nil},
		{&PENormalizer{}, []string{"pe"}},
		{&NTFSTimestampNormalizer{}, []string{"ntfs"}},
		{&CompositeNormalizer{Normalizers: []Normalizer{&NTFSTimestampNormalizer{}, &PENormalizer{}}}, []string{"ntfs", "pe"}},
	}
	for _, c := range cases {
		got := Names(c.n)
		if !reflect.DeepEqual(got, c.names) {
			t.Errorf("Names(%T) = %v, want %v", c.n, got, c.names)
		}
		back, err := FromNames(got)
		if err != nil {
			t.Fatalf("FromNames(%v): %v", got, err)
		}
		if !reflect.DeepEqual(Names(back), c.names) {
			t.Errorf("FromNames(Names(%T)) names as %v, want %v", c.n, Names(back), c.names)
		}
		if len(c.names) == 0 && back != nil {
			t.Errorf("no names reconstructed a %T, want nil", back)
		}
	}
	// Order is the composition order and is preserved.
	rev, _ := FromNames([]string{"pe", "ntfs"})
	if got := Names(rev); !reflect.DeepEqual(got, []string{"pe", "ntfs"}) {
		t.Fatalf("composite order not preserved: %v", got)
	}
	// The historical capitalized spelling is tolerated on read, and written
	// back in the canonical form.
	alt, err := FromNames([]string{"PE"})
	if err != nil || !reflect.DeepEqual(Names(alt), []string{"pe"}) {
		t.Fatalf("the PE alias: err=%v names=%v", err, Names(alt))
	}
	if _, err := FromNames([]string{"pe", "gzip"}); err == nil {
		t.Fatal("an unknown normalizer name was accepted — the read path would hash with the wrong normalizer")
	}
}

// IdentityHashInput is the one rule for "what bytes does the chunk hash
// cover": the normalized form when a normalizer is set, the data itself
// otherwise — never a copy that differs from Normalize.
func TestIdentityHashInputMatchesNormalize(t *testing.T) {
	data := append([]byte("MZ"), bytes.Repeat([]byte{0x11}, 4096)...)
	if got := IdentityHashInput(nil, data); !bytes.Equal(got, data) {
		t.Fatal("nil normalizer changed the identity input")
	}
	for _, n := range []Normalizer{&PENormalizer{}, &NTFSTimestampNormalizer{},
		&CompositeNormalizer{Normalizers: []Normalizer{&PENormalizer{}, &NTFSTimestampNormalizer{}}}} {
		if got := IdentityHashInput(n, data); !bytes.Equal(got, n.Normalize(data)) {
			t.Fatalf("%T: IdentityHashInput differs from Normalize", n)
		}
	}
}
