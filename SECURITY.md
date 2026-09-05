# Security policy

disknexus-engine is a storage engine: it chunks, deduplicates, encrypts, packs,
indexes, and restores backup data. A defect here can mean data that cannot be
restored or data that leaks. We want to hear about either.

## Reporting a vulnerability

Email **security@smithoperatingsolutions.com** with:

- what you found and the version or commit it applies to;
- a reproduction, or as close to one as you can get;
- whether you believe it affects data confidentiality, integrity, or
  availability (a restore that produces wrong bytes is an integrity issue and
  is treated as severe).

Do not open a public issue for a vulnerability. You will get an acknowledgement
within three business days and a fix or a mitigation plan within thirty. We
credit reporters in the release notes unless asked not to.

## Scope

In scope: everything in this module — the on-disk formats it writes, the
crypto it applies (`core/crypto`), the restore and verify paths, and the
platform readers (`volume`, `volumefs`, `vss`, `bmr`).

Out of scope: the products that embed this engine (their servers, agents, and
network protocols are not in this repository), and dependencies, which should
be reported upstream.

## What we consider a vulnerability here

- A restore or verify that reports success for bytes that differ from what was
  backed up.
- Plaintext reaching disk or the network from an encrypted repository without
  the key.
- A crafted repository (packs, index, manifest, deltas) that makes the engine
  read or write outside the repository, exhaust memory unboundedly, or panic in
  a way that leaves a repository unrecoverable.
- A key-derivation or wrapping weakness in `core/crypto`.

## Supported versions

Pre-1.0: only the latest tagged release receives fixes. See the README for
the versioning policy.
