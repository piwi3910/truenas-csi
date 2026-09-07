# Task 24: Operator documentation

Status: open
Created: 2026-09-07

## Description

Implements plan task 24 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 24: Operator documentation" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `README.md` — overview, installation, StorageClass reference.
- `docs/security.md` — least privilege and the shared-target exposure.
- `docs/troubleshooting.md` — failure modes and their signatures.

Interfaces it produces for later tasks:

none; this task documents what earlier tasks built.

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestDocsListAllStorageClassParameters.

## Acceptance criteria

- [ ] Write `TestDocsListAllStorageClassParameters` in `test/docs/docs_test.go`, parsing the parameter names out of the backend implementations and asserting each appears in `README.md`; fails when a parameter is added without documentation. Run `go test ./test/docs/` — expect FAIL with "README.md not found".
- [ ] Write `docs/security.md` containing the exact 14-role list — `DATASET_WRITE`, `DATASET_DELETE`, `POOL_READ`, `SNAPSHOT_WRITE`, `SNAPSHOT_DELETE`, `SHARING_ISCSI_EXTENT_WRITE`, `SHARING_ISCSI_TARGET_WRITE`, `SHARING_ISCSI_TARGETEXTENT_WRITE`, `SHARING_ISCSI_GLOBAL_READ`, `SHARING_ISCSI_PORTAL_READ`, `SHARING_ISCSI_INITIATOR_READ`, `SHARING_ISCSI_AUTH_READ`, `SHARING_NFS_WRITE`, `FILESYSTEM_ATTRS_WRITE` — with instructions for creating the TrueNAS account, and a prominent section stating that a single shared iSCSI target exposes every LUN to every logged-in node, so RWO is not enforced below Kubernetes.
- [ ] Write `docs/troubleshooting.md` covering: an API key revoked by plaintext connection, authentication failure being terminal by design, `-32000` backpressure, a StorageClass requesting a filesystem the node cannot create, and a PVC stuck Pending because pool capacity is exhausted.
- [ ] Write `README.md` covering installation via Helm, the node package prerequisites (`open-iscsi`, `xfsprogs`, `cifs-utils`, `nvme-cli`, `multipath-tools`), every StorageClass parameter with its default, and the supported protocol matrix.
- [ ] Run `go test ./test/docs/` — expect PASS.
- [ ] Commit: "docs: operator guide, security model and troubleshooting".

## Evidence

