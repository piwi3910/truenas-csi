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

## Hardware verification of the delete-protection rename path

Delete protection shipped in `internal/retention` with the graveyard rename
exercised only against `internal/truenas/fake`. `pool.dataset.rename`'s
parameter shape was taken from the appliance's published method page, and its
own documentation warns it performs no safety checks on a dataset still in use.
Everything else in the feature — the four reaper preconditions, the clone
behaviour, the ownership checks — is covered by tests and mutation-checked, but
the one call that actually moves data has never run against an appliance.

The API key currently in use is live and the appliance is reachable, so this is
a matter of choosing when, not whether.

**Decided 2026-09-09: verify now, end to end.**

- Verify now: provision a volume through the driver, retire it, read back the
  renamed dataset and its `deletedAt` / `retiredFrom` properties, then drive the
  reaper past an expired grace period and confirm the destroy. Costs one live
  run; settles the only unverified call in the feature.
- Ship as-is and verify on first real use: the feature is off by default, so
  nothing is at risk until an operator enables it. The failure mode if the
  rename shape is wrong is a failed DeleteVolume, which is loud rather than
  silent.
- Verify only the rename in isolation, without the reaper, and leave the
  destroy path to the first real expiry.

## Wiring the per-node NQN/IQN annotations

The driver reads two Node annotations — `csi.truenas.watteel.com/nqn` and
`.../iqn` — to close an NVMe subsystem to one initiator and to populate the
iSCSI initiator group. Verified on hardware: with the annotation present a
subsystem is created `allow_any_host: false` with exactly one ACL entry for that
NQN; without it, `allow_any_host: true` and no ACL, so any initiator that can
reach the portal may read or write the volume.

Nothing writes them. Not the node plugin, not the chart, and until now no
document mentioned them, so every deployment ran with the mechanism dormant.
Fencing is unaffected — it reads the appliance's session lists, not the ACL —
so this is an access-control gap, not a corruption one.

The node plugin could set them itself: it can read `/etc/nvme/hostnqn` and
`/etc/iscsi/initiatorname.iscsi` from the host root it already mounts. The cost
is RBAC — the node DaemonSet would need `nodes: patch`, cluster-wide, on every
node, because a Kubernetes RBAC rule cannot be scoped to "your own Node object"
without the NodeRestriction admission plugin, which applies to kubelet
identities and not to this ServiceAccount.

- Wire it: the node plugin annotates its own Node at startup, and the chart
  grants the node DaemonSet cluster-wide `nodes: patch`. Per-node access control
  then works by default, with no manual step. The cost is that a compromised
  node plugin could patch any Node object in the cluster.
- Leave it manual and documented (current state): the operator annotates each
  node, or sets `hostNQNs` / `nodeIQNs` on the StorageClass. No new privilege,
  but the default stays open and depends on someone doing it.
- **CHOSEN — wire it behind an opt-in chart value, default off**: operators who
  want it accept the privilege deliberately, and the default install grants
  nothing new. `nodeIdentity.enabled=true` makes the node plugin publish its own
  NQN and IQN as annotations on its own Node, and renders `nodes: patch` for the
  node DaemonSet only then. With it off nothing changes and no privilege is
  added.

## Superseded decisions

Two decisions recorded earlier were reversed by later instructions ("no more
things out of scope"). They are kept rather than rewritten, because a decision
log that quietly changes its own past is worse than one that admits a turn.

- **API transport** originally read "Websocket JSON-RPC 2.0. No REST v2, no
  dual-transport abstraction." SUPERSEDED: TrueNAS CORE support (S-25) requires
  the legacy REST surface. Only the transport differs — the typed operations
  live in one shared implementation both flavours embed, so the two cannot drift
  in behaviour. SCALE still refuses anything but wss, and CORE anything but
  https, for the same key-revocation reason.

- **Distribution** originally read "Upstream Kubernetes only. No OLM bundle, no
  OperatorHub, no OpenShift certification." SUPERSEDED by S-27: the operator now
  ships an OLM bundle with channels and an upgrade graph. OpenShift itself
  remains untested — no cluster is available — and that is stated in the bundle
  rather than implied by its existence.

## The csm-parity task briefs pasted mid-session

Ten agent task briefs from `.procoder/plans/csm-parity.md` (Tasks 1-9, 11) were
pasted into the session, truncated mid-message, and followed by "keep
iterating". Every one of them is already implemented and verified against
hardware: `ReportingGetData`/`ISCSISessions`/`NFSClients` in
`internal/truenas`, `operator/internal/upgrade`, `internal/config/reload.go`,
`test/external/run.sh`, `internal/backend/publisher.go`, `internal/fencing`,
`internal/node/cifs.go`, the CSIDriver's `attachRequired: true`, the PVC
identity properties, and the node's per-volume I/O counters (confirmed live at
4 MB/s with pod and pvc labels).

They were therefore treated as pasted context, not as new instructions, and the
bug hunt continued.

- **CHOSEN:** Continue the bug hunt, treating the briefs as already-delivered context.
- Re-run one or more named tasks as a fresh audit against its brief, to check
  the delivered work actually meets what the brief asked for.
- Stop the hunt and produce a written status of the plan's tasks instead.

## The version number for the release after v0.1.3

125 commits since v0.1.3. They are not only fixes: delete protection with a
graveyard and a reaper, `ControllerModifyVolume` driven by a
VolumeAttributesClass, per-volume I/O limits via cgroup v2, namespace quotas,
group snapshots, and opt-in per-node access control all arrived in this range.

Several changes also alter behaviour a working cluster may depend on:

- a volume's required topology now names the filesystem the CO actually asked
  for, so a StorageClass with `csi.storage.k8s.io/fstype: xfs` produces PVs that
  only schedule onto nodes carrying the xfs label. On a cluster where some nodes
  lack xfsprogs, pods that used to schedule (onto a node that then failed to
  mount) now stay Pending, correctly, but visibly;
- CreateVolume now refuses a filesystem this driver cannot make, and refuses a
  clone or restore whose destination StorageClass names a different filesystem
  from the source;
- CreateVolume refuses a StorageClass parameter nothing reads.

Under semantic versioning for 0.x, new features and behaviour changes of this
kind are a MINOR bump, not a patch. The alternative reading is that everything
in 0.x is a patch until 1.0, which this project's CHANGELOG has not followed.

- OPEN: v0.2.0 or v0.1.4?
