# Task 22: Release pipeline, signing and SBOM

Status: open
Created: 2026-09-07

## Description

Implements plan task 22 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 22: Release pipeline, signing and SBOM" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `.github/workflows/ci.yaml` — build, lint, unit and sanity tests.
- `.github/workflows/release.yaml` — multi-arch build, sign, SBOM.
- `test/release/release_test.go` — artifact verification.

Interfaces it produces for later tasks:

- Published image `ghcr.io/pwatteel/truenas-csi:<tag>` for `linux/arm64` and `linux/amd64`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestReleaseArtifacts.

## Acceptance criteria

- [ ] Write `TestReleaseArtifacts`: for the tag under test, assert `docker manifest inspect` lists both `linux/arm64` and `linux/amd64`, that `cosign verify` succeeds against the keyless identity of the release workflow, and that an SBOM attestation is present. Run `go test ./test/release/` — expect FAIL with "no such image".
- [ ] Write the CI workflow running `go vet`, the linter, unit tests and csi-sanity on every push, with arm64 jobs on a native arm64 runner.
- [ ] Write the release workflow building both architectures with buildx, pushing a manifest list, signing with cosign keyless, generating an SBOM with syft and attaching it.
- [ ] Pin every GitHub Action to a commit SHA, set explicit `permissions:` blocks, and give each job a timeout.
- [ ] Run `go test ./test/release/` against a published pre-release tag — expect PASS.
- [ ] Commit: "ci: multi-arch release with cosign signatures and SBOM".

## Evidence

