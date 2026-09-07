# Decisions

## Scope
Production-ready CSI driver for Kubernetes, from scratch. Go. TrueNAS SCALE only.

## Protocols (eventual)
iSCSI, NFS, SMB, NVMe-oF (transports: TCP + RDMA/RoCE; no Fibre Channel).

## API transport
Websocket JSON-RPC 2.0 (`/api/current`). No REST v2, no dual-transport abstraction.

## v1 scope
- Protocols: iSCSI + NFS only (SMB and NVMe-oF deferred)
- CSI features: snapshots + clones, volume expansion, raw block volumes

## Volume identity
Structured volume ID: `<protocol>/<pool>/<dataset path>/<name>`, e.g. `iscsi/tank/k8s/pvc-<uuid>`.
Stateless — all TrueNAS object names derive from it. No driver-side mapping state.

## Config split
- StorageClass parameters: pool, parent dataset, protocol, sparse/thick, portal group, export options
- Secret + flags: TrueNAS URL, API key, TLS verification (operator policy, not user-writable)

## Test environment
Live TrueNAS SCALE box available during development. Real integration tests.

## Packaging
Helm chart in-repo, versioned with the driver.

## Hardening
All of it is v1: per-volume locking, resumable middleware jobs, correct gRPC codes,
leader election, websocket reconnect with backoff, graceful shutdown, Prometheus metrics,
NodeGetVolumeStats, orphan reconciler (report-only), iSCSI multipath + CHAP,
max_volumes_per_node, topology, filesystem handling incl. online resize, CSIStorageCapacity,
minimal RBAC / non-root / seccomp, API key never logged, multi-arch images signed with
cosign plus SBOMs.

## Distribution
Upstream Kubernetes only. No OLM bundle, no OperatorHub, no OpenShift certification.

## Operator
Kubebuilder operator with one typed `TrueNASCSIDriver` CRD that renders the in-repo Helm
chart via the Helm Go SDK and reconciles the result — one manifest set, two install paths.
Operator owns drain-aware DaemonSet rollout, API key rotation, version-skew refusal, and
health/orphan reporting in status. Built after the driver reaches v1, not before.

## Kubernetes support
Minimum 1.31. Target cluster is k3s 1.34.4.

## Architecture target
Primary architecture is **arm64** (RK3588 / Armbian noble, kernel 6.12.58, k3s 1.34).
amd64 is built but not the validation target. CI needs arm64 build capability.

## Snapshot CRDs
external-snapshotter is a documented prerequisite, with an opt-in chart flag to install
the controller + CRDs for clusters that have none. Never installed by default — two drivers
installing the snapshot controller breaks clusters.

## NVMe-oF validation
NVMe/TCP is the implementable transport. RoCE stays in the design but is unvalidated —
the target cluster (RK3588, 2.5GbE) has no RDMA-capable hardware.

## License
Apache 2.0.

## TLS / transport (forced by live findings)
- `wss://` only. A plaintext URL is rejected at config validation — TrueNAS 25.10 REVOKES
  an API key presented over insecure transport, so plaintext destroys the credential.
- Auth failure is terminal and never retried; only connection failures get backoff.
- Trust model: optional CA bundle supplied in the driver Secret, verified properly by
  default. An explicit `insecureSkipVerify` exists, defaults to false, and logs a loud
  warning when enabled — needed because a stock TrueNAS cert is self-signed with
  `SAN=DNS:localhost` and cannot be verified against a real address.

# Open decisions

## Live verification
Awaiting a fresh API key, to be used over `wss://` only.

## Node prerequisites (to verify on the k3s cluster)
- `open-iscsi` + `iscsi_tcp` on Armbian rockchip64 kernel 6.12.58
- multipath tooling
- `nvme-tcp` (later milestone)

## Next step after the spec

Spec `.procoder/specs/truenas-csi.md` is COMPLETE (22 scope items, 35 criteria).
The procoder chain for architectural work goes spec -> plan -> todo/backlog -> build.

- Run `/procoder:plan` now — turn the spec into a gated implementation plan
- Seed the backlog first (`/procoder:backlog`) — milestones and stories before the plan
- Revisit the shared-iSCSI-target decision before planning around it
- Start building directly from the spec, skipping the plan
