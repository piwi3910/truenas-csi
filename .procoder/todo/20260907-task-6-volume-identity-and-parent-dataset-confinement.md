# Task 6: Volume identity and parent-dataset confinement

Status: closed
Created: 2026-09-07

## Description

Implements plan task 6 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 6: Volume identity and parent-dataset confinement" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `internal/volume/id.go` — encode and parse.
- `internal/volume/confine.go` — containment check.
- `internal/volume/id_test.go` — tests.

Interfaces it produces for later tasks:

- `type ID struct { Backend, Protocol, Pool, Parent, Name string }`
- `func (id ID) String() string` producing `<backend>/<protocol>/<pool>/<parent>/<name>`
- `func ParseID(s string) (ID, error)`
- `func (id ID) DatasetPath() string` producing `<pool>/<parent>/<name>`
- `func Confine(id ID, allowedPool, allowedParent string) error`
- `var ErrOutsideParent = errors.New("volume resolves outside the configured parent dataset")`

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestVolumeIDRoundTrip, TestVolumeIDConfinement, TestParseIDRejectsMalformed.

## Acceptance criteria

- [x] Write `TestVolumeIDRoundTrip` asserting `ParseID("nas1/iscsi/Pool0/k8s/pvc-abc").String()` equals the input and that `DatasetPath()` is `Pool0/k8s/pvc-abc`. Run `go test ./internal/volume/` — expect FAIL with "undefined: ParseID".
- [x] Write `TestVolumeIDConfinement`, table-driven, asserting `ErrOutsideParent` for each of: `"nas1/nfs/Pool0/k8s/../../Home"`, `"nas1/nfs/Pool0/../Home/x"`, `"nas1/nfs/Pool0/k8s/./../../old_homes"`, `"nas1/nfs/OtherPool/k8s/x"`, `"nas1/nfs/Pool0/notk8s/x"`, and a name containing `/`; and success for `"nas1/nfs/Pool0/k8s/pvc-1"`.
- [x] Write `TestParseIDRejectsMalformed` covering empty string, three segments, and an empty component between separators.
- [x] Implement `ParseID` splitting into exactly five components, rejecting any component that is empty, `.` or `..`, or that contains a path separator after unescaping.
- [x] Implement `Confine` comparing `filepath.Clean` of the resolved dataset path against the configured `<pool>/<parent>` prefix, requiring a separator at the boundary so `k8s-other` does not match `k8s`.
- [x] Run `go test ./internal/volume/` — expect PASS.
- [x] Commit: "volume: structured identity with parent-dataset confinement".

## Evidence

- Delivered by a parallel worktree; merged. Traversal cases rejected before any middleware call.
- `go test ./...` green across all 18 packages; `gofmt -l` and `go vet ./...` clean.
- procoder gate: 0 blocking findings. Committed on branch feat/foundation.
