# Changelog

Tags on this repository. Signature changes are called out; on-disk formats
never change incompatibly.

## v0.2.6 (unreleased)
- `crypto.WrapSecretAsymmetric` / `crypto.UnwrapSecretAsymmetric`: X25519 ECIES for a secret of any length (HKDF info `disknexus-secret-wrap-v1`, a domain distinct from master-key wrapping); the layout a browser reproduces with WebCrypto. `crypto.SecretWrapOverhead`.
- `crypto.KeyFile.KeyID` (`key_id`, optional; absent in earlier files, readers treat absence as unknown): a domain-separated hash naming the master key without carrying it. `(*MasterKey).ID()`, `crypto.RewrapKeyFile(mk, passphrase)` (the rotation primitive: same key, fresh salt and nonce), `(*KeyFile).SameKey(other)`.

## v0.2.5
- deps: `github.com/ulikunitz/xz` v0.5.15 (indirect, via go-diskfs).

## v0.2.4
- `bmr.CloneResult.ReadBack`: how many partitions were read back and compared (every one with `ReadAt` set, none otherwise).

## v0.2.3
- `volumefs.Shrinker` / `ExternalShrinker`: the seam that shrinks a staged NTFS or ext4 filesystem with `ntfsresize` or `e2fsck` + `resize2fs`, naming a missing tool before anything runs.

## v0.2.2
- `bmr.RestoreDiskFit`, `RestoreMemberTo`, `StagedMember`: restore a machine snapshot's disk into a fit plan, shrunk members supplied by the caller from staging.
- `bmr.CloneDisk`: partition-by-partition drive-to-drive clone with per-partition SHA-256 read-back; `CheckBootStructures` and `BootReport`.
- `volumefs.ScanFAT32Partition`.

## v0.2.1
- `volumefs.MinimumSize`: how small an NTFS (from `$Bitmap`) or ext4 (superblock) filesystem can go; `FSMinimum` with the exact `LastUsedEnd`.
- `volumefs.Identity` / `IdentityAuto`: a filesystem's serial (NTFS/FAT/exFAT volume serial, ext4 UUID) and label.

## v0.2.0
- `disklayout.PlanFit` / `ApplyFit`, `TargetGeometry`, `FitOptions`, `FitPlan`: fit a captured GPT/MBR layout onto a drive of another size — grow the last data partition, move a trailing Recovery partition to the end, shrink to a filesystem's minimum, realign, refuse a logical-sector mismatch.

## v0.1.1
- Coverage tests for the seams the product's suite used to cover; CODEOWNERS.

## v0.1.0
- First public release: the engine split out of disknexus with its e2e proof suite.
