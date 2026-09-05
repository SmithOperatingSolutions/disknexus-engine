// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Smith Operating Solutions

package pipeline_test

import (
	"github.com/SmithOperatingSolutions/disknexus-engine/core/crypto"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/pipeline"
	"github.com/SmithOperatingSolutions/disknexus-engine/core/store"
)

// Shorthands for the three repo shapes this package's tests construct
// pipelines for. They exist so the #265 churn stayed mechanical: every
// pipeline.New(cfg, logger) became New(cfg, logger, noEnc()) and every
// New(cfg, logger, key, key) became New(cfg, logger, passEnc(key)).
//
// MustBind is used deliberately: in a test the repo config is a literal, so a
// Bind error is a bug in the test rather than a condition to handle.
// TestMustBindIsTestOnly forbids it outside _test.go files.

// noEnc binds an unencrypted repo with no normalizers.
func noEnc() pipeline.Binding { return pipeline.MustBind(store.RepoConfig{}, nil) }

// noEncNorm binds an unencrypted repo with the given normalizer names.
func noEncNorm(names ...string) pipeline.Binding {
	return pipeline.MustBind(store.RepoConfig{Normalizers: names}, nil)
}

// passEnc binds a passphrase-encrypted repo, where the index is encrypted
// under the same key as the chunks.
func passEnc(k *crypto.MasterKey) pipeline.Binding {
	return pipeline.MustBind(store.RepoConfig{EncryptionMode: store.EncryptPassphrase}, k)
}

// managedEnc binds a managed-encryption repo, where the index stays plaintext.
func managedEnc(k *crypto.MasterKey) pipeline.Binding {
	return pipeline.MustBind(store.RepoConfig{EncryptionMode: store.EncryptManaged}, k)
}
