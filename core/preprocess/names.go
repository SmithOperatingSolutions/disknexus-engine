// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package preprocess

import "fmt"

// Stable, persisted names for each normalizer. These are written into the
// repository config so that read paths (restore, verify, index rebuild,
// prune) can reconstruct exactly the normalizer the backup used — the chunk
// identity hash is computed over normalized bytes but the original bytes are
// stored, so verification must re-normalize with the same normalizer.
const (
	NameePE   = "pe"   // PENormalizer
	NameNTFS  = "ntfs" // NTFSTimestampNormalizer
	namePEAlt = "PE"   // tolerated on read
)

// Names returns the stable persisted name(s) for a normalizer, flattening a
// CompositeNormalizer into its parts. Returns nil for a nil or no-op
// normalizer.
func Names(n Normalizer) []string {
	switch v := n.(type) {
	case nil:
		return nil
	case *NoopNormalizer:
		return nil
	case *PENormalizer:
		return []string{NameePE}
	case *NTFSTimestampNormalizer:
		return []string{NameNTFS}
	case *CompositeNormalizer:
		var out []string
		for _, sub := range v.Normalizers {
			out = append(out, Names(sub)...)
		}
		return out
	default:
		return nil
	}
}

// FromNames reconstructs a Normalizer from persisted names. It returns nil
// when names is empty (no normalization). The order of names is preserved so
// composite normalization is reproduced exactly.
func FromNames(names []string) (Normalizer, error) {
	var normalizers []Normalizer
	for _, name := range names {
		switch name {
		case NameePE, namePEAlt:
			normalizers = append(normalizers, &PENormalizer{})
		case NameNTFS:
			normalizers = append(normalizers, &NTFSTimestampNormalizer{})
		default:
			return nil, fmt.Errorf("unknown normalizer %q", name)
		}
	}
	switch len(normalizers) {
	case 0:
		return nil, nil
	case 1:
		return normalizers[0], nil
	default:
		return &CompositeNormalizer{Normalizers: normalizers}, nil
	}
}

// IdentityHashInput returns the bytes whose SHA-256 is the chunk identity:
// the normalized form when a normalizer is set, otherwise data unchanged.
// Read paths use this to verify stored (original) chunk bytes against the
// normalized identity hash recorded in the manifest.
func IdentityHashInput(n Normalizer, data []byte) []byte {
	if n == nil {
		return data
	}
	return n.Normalize(data)
}
