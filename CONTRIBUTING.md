# Contributing

Thanks for looking. This engine is small enough to hold in your head and
dangerous enough to deserve care: a defect can produce a backup that will not
restore. The rules below exist for that reason, not for ceremony.

## Before you start

- Open an issue describing the problem or the change. For anything touching
  an on-disk format (`docs/`), say so — formats are a compatibility contract
  and change rarely.
- Build and test standalone: `go build ./... && go test ./...` from this
  directory. The end-to-end suite in `test/e2e` is the proof that a
  repository written by this module restores with this module alone; keep it
  green.

## How we test

Every change that adds or fixes behavior comes with a test that **fails
without the change**. The standard is in `docs/TESTING.md`; the short form:

1. The failure message says what an operator would experience, not what an
   internal variable held.
2. Interrogate the fixture: ask what it would look like if the bug were
   present. If the answer is "identical", the fixture is wrong.
3. Assert against an authority — a digest, a byte-exact restore, bytes on
   disk — never against the absence of an error.
4. Every refusal test has a positive control on the same fixture.
5. Mutation-prove load-bearing guards: change the production code so the
   property is false, confirm the test goes red, revert. Say in the PR which
   mutants you ran and whether any survived.
6. A `t.Skip` is a deleted test. If a host cannot run a test, that is a
   failure to look at, not a pass to scroll past.

## Boundaries the build enforces

- No package here imports anything outside this module and the standard
  library plus the dependencies in `go.mod`.
- No package reads a `DISKNEXUS_*` environment variable: every knob is a
  parameter, and the caller decides.
- Every package is imported by another package here or is a documented entry
  point; a leaf nobody uses does not belong in a library.

These are checked by the embedding product's architecture tests today and
will be checked by this repository's own CI after the split.

## Style

`gofmt`, `go vet`, and the Windows and macOS cross-compiles must be clean.
Comments explain *why*, and name the incident or issue that taught us, not
*what* the next line does.

## License

By contributing you agree your contribution is licensed under the Apache
License 2.0 (see `LICENSE`).
