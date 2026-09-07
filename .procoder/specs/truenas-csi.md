# truenas-csi

Status: complete

## Problem

The `kw` k3s cluster (8 arm64 nodes, Kubernetes 1.34) stores all persistent data on
Longhorn, which replicates across the nodes' own disks. Meanwhile a TrueNAS SCALE 25.10.6
appliance sits on the same network with 72 TB of ZFS storage, 44.9 TB free, doing nothing
for Kubernetes beyond receiving Longhorn's backups over NFS. Workloads that want large,
snapshot-capable, pool-backed volumes have no way to ask for them: node-local replication
is the only option, so capacity is bounded by what fits in the nodes, and ZFS's snapshots,
clones and quotas are unreachable from a PersistentVolumeClaim.

The existing third-party option, democratic-csi, is a general-purpose driver spanning many
storage backends over a REST API that TrueNAS has since deprecated. This project builds a
driver aimed at exactly one backend, speaking the API TrueNAS actually supports today, and
treating the fact that the target pool holds ~20 TiB of irreplaceable data as a primary
design constraint rather than an operational footnote.

## Users

- **Cluster operator (Pascal)** — installs and upgrades the driver, supplies TrueNAS
  credentials, defines StorageClasses, and needs to trust that a driver bug cannot destroy
  the pool's existing datasets. Needs least-privilege credentials, clear failure messages,
  and an upgrade path that does not rip mounts out from under running pods.
- **Application workloads** — consume PVCs. Need volumes that honour their requested size,
  support the access modes they declare, survive node restarts, and can be snapshotted and
  restored.
- **Platform/SRE reader** — reads metrics and logs to answer "is storage healthy, which
  volume is failing, and is the pool filling up". Needs per-volume attribution in logs and
  usable Prometheus metrics.
- **Future contributor** — extends the driver to SMB and NVMe-oF. Needs the backend
  abstraction to make a new protocol an addition, not a rewrite.

## In scope

- [S-1] **TrueNAS middleware client** — JSON-RPC 2.0 over `wss://` only, API-key auth via
  auth.login_ex, one reader goroutine demultiplexing responses by id, bounded in-flight
  concurrency, reconnect with backoff, and job polling for filesystem.setperm.
- [S-2] **CSI Identity service** — plugin name/version, capabilities, probe.
- [S-3] **NFS volume provisioning** — dataset + sharing.nfs share + `refquota` +
  permissions, with delete.
- [S-4] **iSCSI volume provisioning** — zvol + extent + target + targetextent, with delete.
- [S-5] **Volume identity and delete safety** — structured volume IDs, a parent-dataset
  confinement check, and a ZFS ownership property verified (`source == "LOCAL"`) before any
  destructive call.
- [S-6] **Snapshots, clones and restore** — CreateSnapshot/DeleteSnapshot/ListSnapshots,
  and CreateVolume from a snapshot source, with the clone explicitly stamped and quota'd.
- [S-7] **Volume expansion** — ControllerExpandVolume (grow-only, guarded per backend) and
  NodeExpandVolume (device rescan + filesystem grow).
- [S-8] **Node NFS stage/publish** — mount/unmount with the configured NFS version.
- [S-9] **Node iSCSI stage/publish** — login, deterministic device resolution by NAA,
  mkfs/mount for filesystem volumes and bind-mount for raw block volumes, logout on unstage.
- [S-10] **Node capability preflight** — probe host binaries and kernel modules at startup,
  `modprobe` what is needed, advertise only deliverable capabilities, fail clearly otherwise.
- [S-11] **NodeGetVolumeStats** — capacity and inode usage per volume for kubelet.
- [S-12] **Capacity reporting** — GetCapacity / CSIStorageCapacity from real pool free space.
- [S-13] **Topology** — constrain scheduling to nodes that can reach the NAS and possess
  the capability a StorageClass requires.
- [S-14] **Observability** — structured logs carrying the volume ID, Prometheus metrics for
  CSI and middleware calls, health/liveness endpoints.
- [S-15] **Concurrency and idempotency** — per-volume locking and query-then-act semantics
  so retried CSI calls converge instead of duplicating or orphaning objects.
- [S-16] **Security** — minimal TrueNAS role set, minimal Kubernetes RBAC, non-root
  controller, credentials never logged, TLS trust configuration.
- [S-17] **Helm chart** — controller Deployment with sidecars, node DaemonSet, RBAC,
  CSIDriver object, StorageClass examples, snapshot-CRD prerequisite handling.
- [S-18] **Supply chain** — multi-arch images (arm64 primary), cosign signatures, SBOMs.
- [S-19] **Orphan reconciler** — periodically compare TrueNAS objects against PVs and
  report (never delete) strays.
- [S-20] **iSCSI CHAP and multipath** — CHAP authentication, and multipath support that
  degrades to single-path when the node lacks multipath tooling.
- [S-21] **Test suite** — unit tests, csi-sanity, and integration tests against a live
  TrueNAS box.
- [S-22] **Multiple TrueNAS backends** — a named set of appliances in driver config,
  selected per StorageClass, with per-backend connections, capacity reporting and
  credentials; the backend name is part of every volume ID.

## Out of scope

- **SMB and NVMe-oF volumes.** Both are validated at the API and node level (see the
  findings notes) and the backend abstraction is designed to accept them, but neither ships
  in v1.
- **NVMe over RDMA/RoCE.** nvmet.global.rdma = false on the target appliance and the
  RK3588 nodes have no RDMA-capable NICs, so it cannot be validated here at all.
- **TrueNAS CORE.** SCALE only; no FreeBSD, no legacy REST.
- **The lifecycle operator.** A `TrueNASCSIDriver` operator wrapping the chart is planned
  immediately after v1 and gets its own spec; this one covers the driver and chart.
- **Migrating existing Longhorn volumes.** The driver coexists with Longhorn; it does not
  import, convert, or move data from it.
- **Managing the pool itself** — pool creation, scrubs, disk replacement, snapshot
  retention policies, replication tasks. The driver manages only datasets it creates.
- **Multi-cluster or multi-tenant credential isolation.** One driver install serves one
  Kubernetes cluster.
- **OpenShift / OLM packaging.** Upstream Kubernetes only.

## Constraints

- **Transport: `wss://` only, enforced at config validation.** TrueNAS 25.10 *revokes* an
  API key presented over plaintext ("API key revoked due to insecure transport") — three
  keys were destroyed this way during research. A plaintext URL must be rejected before any
  connection is attempted, and this must have a test.
- **Authentication failure is terminal.** Never retry a failed login; log and fail fatally.
  Only connection failures get backoff. A naive reconnect loop could revoke the driver's own
  credentials and break every volume operation in the cluster.
- **Concurrency ceiling: 20 in-flight calls per connection** (measured). Excess calls fail
  with JSON-RPC `-32000` carrying no error data. Client caps in-flight work below this.
- **Kubernetes 1.31 minimum**; target cluster runs k3s 1.34.
- **arm64 is the primary architecture** (RK3588, Armbian noble, kernel 6.12.58). amd64 is
  built but not the validation target.
- **The pool holds ~20 TiB of live, irreplaceable data** (`Home`, `old_homes`, `Multimedia`,
  `Backup/timemachine`, `VMs`). Data-loss prevention outranks every other quality attribute.
- **Longhorn already owns the nodes' iSCSI stack.** The driver shares `iscsid`,
  `/etc/iscsi` and `/var/lib/iscsi` with it. Every `iscsiadm` invocation must be scoped to
  our specific target and portal — no `--logoutall`, no unscoped `-o delete`, no global
  session rescan, no rewriting iscsid.conf.
- **TrueNAS TLS is self-signed** with `CN=localhost` and `SAN=DNS:localhost`, so it cannot
  be verified against a real address. Trust must be explicitly configured.
- **Least privilege**: the driver's TrueNAS account uses a documented 14-role set, not
  `FULL_ADMIN`. system.info is deliberately not called because it requires `READONLY_ADMIN`.
- **Language: Go**; licence Apache 2.0.
- **Driver name `csi.truenas.watteel.com`**, Go module `github.com/pwatteel/truenas-csi`.
  The driver name is immutable once PersistentVolumes exist — changing it orphans every PV.
- **Accepted risk: a single shared iSCSI target exposes every LUN to every logged-in node.**
  Initiator ACLs are a property of the target, not of individual LUNs, so per-volume
  isolation is not achievable in this model. All cluster nodes are treated as equally
  trusted. The initiator ACL therefore defends the cluster boundary — keeping
  non-cluster machines off the target — not one node from another. This was chosen
  deliberately over per-node or per-volume targets, and must be stated in the README so no
  operator assumes RWO is enforced below Kubernetes.

## Interfaces

### CSI gRPC services
Identity, Controller and Node, over UNIX domain sockets. Controller capabilities:
`CREATE_DELETE_VOLUME`, `CREATE_DELETE_SNAPSHOT`, `LIST_VOLUMES`, `LIST_SNAPSHOTS`,
`EXPAND_VOLUME`, `CLONE_VOLUME`, `GET_CAPACITY`. Node capabilities:
`STAGE_UNSTAGE_VOLUME`, `EXPAND_VOLUME`, `GET_VOLUME_STATS`.

### Driver configuration (flags + Secret)
A map of **named backends**, each with: endpoint URL (wss enforced), API key, username,
optional CA bundle, `insecureSkipVerify` (default false, warns loudly), pool and parent
dataset. Driver-level: node id, log level, metrics and health ports. Each backend holds
its own connection, concurrency budget and capacity figures.

### StorageClass parameters
`backend` (which named appliance), `protocol` (`nfs`|`iscsi`), `pool`, `parentDataset`,
`fsType` (`ext4`|`xfs`), `sparse`, `volblocksize`; NFS options (`nfsVersion`, `networks`,
`maproot`, `mode`, `uid`, `gid`); iSCSI options (`portalID` to reuse an existing portal
instead of letting the driver create one, `chap` to disable the default-on CHAP,
`initiatorACL` to opt out of restricting the target to cluster node IQNs, `multipath`).

### iSCSI object model
One **shared target per backend**, created by the driver on first use, with one LUN per
volume. The driver creates the portal on demand unless `portalID` names an existing one.
The target's initiator group lists the cluster's node IQNs by default. CHAP is enabled by
default with credentials generated by the driver and stored as an iscsi.auth entry on
TrueNAS, which is also where the node plugin reads them from — no Kubernetes Secret is
needed for CHAP.

### TrueNAS middleware methods
auth.login_ex, pool.query, pool.dataset.*, pool.snapshot.*, iscsi.*,
sharing.nfs.*, filesystem.setperm / filesystem.stat, core.get_jobs.

### Operational surfaces
Prometheus `/metrics`, health `/healthz`, gRPC probe for the liveness sidecar.

## Data

- **Volume ID** — `<backend>/<protocol>/<pool>/<dataset path>/<name>`, e.g.
  `nas1/iscsi/Pool0/k8s/pvc-<uuid>`. Structured and stateless: every TrueNAS object name
  derives from it, so the driver keeps no mapping state and DeleteVolume is idempotent by
  construction. The backend name is included because a volume cannot be resolved without
  knowing which appliance holds it; backend, pool and parent are fixed for the volume's life.
- **iSCSI LUN ids** — allocated per shared target. The driver derives the mapping by
  querying existing targetextents rather than storing it, so a controller restart cannot
  lose or duplicate an allocation.
- **Snapshot ID** — the ZFS snapshot id, `<dataset>@<snapshot name>`.
- **Ownership marker** — ZFS user property `io.truenas.csi:managed` stamped at creation and
  re-stamped on clones, verified with `source == "LOCAL"` before any delete. Properties are
  inherited by children, so presence alone is not sufficient evidence of ownership.
- **State ownership** — TrueNAS owns all volume state; Kubernetes owns PV/PVC objects. The
  driver stores nothing of its own and holds no database.
- **Secrets** — TrueNAS API key and any CHAP credentials live in Kubernetes Secrets, are
  read into memory, and must never appear in logs, errors, metric labels or process args.

## Edge cases

- Retried `CreateVolume` for a volume that already exists — must return the existing volume
  when parameters match, `ALREADY_EXISTS` when they do not.
- `DeleteVolume` for a volume already gone — must return success, not an error.
- `DeleteVolume` for a dataset that exists but lacks the ownership marker, or whose marker
  is inherited rather than `LOCAL` — must refuse and report, never delete.
- A volume ID that resolves outside the configured parent dataset (traversal, crafted name,
  a PV from another install) — must be rejected before any TrueNAS call.
- Volume restored from a snapshot: the clone inherits neither the ownership marker nor
  `refquota`; both must be set explicitly or the volume leaks and misreports its size.
- `DeleteSnapshot` while clones still depend on it — must return `FAILED_PRECONDITION`.
- Expansion requesting a smaller size — rejected by the driver; middleware refuses zvol
  shrink but silently permits `refquota` shrink, so the NFS path needs its own guard.
- Expansion while the volume is mounted — must grow live via rescan without unmount.
- Two concurrent CSI calls for the same volume — serialised per volume.
- More than 20 concurrent middleware calls — queued behind the semaphore, not failed.
- Node lacking the capability a StorageClass requires (no `mkfs.xfs`, no `mount.cifs`) —
  fail with a message naming the missing package.
- Node with the kernel module available but not loaded — `modprobe` and continue.
- Node with no multipath tooling — degrade to single path with a warning, do not fail.
- Longhorn sessions present on the node — never touched by our iscsiadm calls.
- iSCSI device index reuse and the node's own local NVMe/SCSI disks — resolve strictly by
  NAA under `/dev/disk/by-id/`, never by scanning or index.
- Raw block volumes — no mkfs, no filesystem mount.
- Thick zvol larger than 80% of pool free space — middleware refuses; surfaced as
  `RESOURCE_EXHAUSTED`.
- `NodeUnstage` when the device is already gone, or the mount already removed.
- Controller restart mid-operation — the next retry converges via query-then-act.
- Pool offline, or dataset locked/encrypted.
- Two volumes racing for the same LUN id on the shared target — allocation must be derived
  from a live query under the per-backend lock, never from cached state.
- The shared target reaching the maximum number of LUNs a target supports.
- A StorageClass naming a backend that is not configured, or a volume ID whose backend no
  longer exists in config — must fail clearly rather than acting on the wrong appliance.
- One backend unreachable while others are healthy — must not stall calls for the others.
- The driver's shared target or portal deleted out from under it by a human.

## Failure modes

- **TrueNAS unreachable** — connection retried with exponential backoff; CSI calls return
  `UNAVAILABLE` so sidecars retry; existing mounts are unaffected. Metric and log on each
  state change, not per attempt.
- **TrueNAS auth rejected** — fatal, no retry, loud log naming the likely cause; the
  container exits rather than risk revoking the key.
- **Plaintext URL configured** — refuse to start, with a message explaining that TrueNAS
  revokes keys presented over insecure transport.
- **TLS verification fails** — refuse to connect unless `insecureSkipVerify` is explicitly
  set; if it is, log a warning on every connect.
- **Middleware returns `-32000`** — treated as backpressure: retry with backoff, never
  surfaced as a volume failure.
- **Middleware error mapping** — `errname` is unreliable (reports `EINVAL` where the truth
  is `ENOENT`), so state is established by query rather than inferred from errors.
- **filesystem.setperm job fails or hangs** — bounded timeout; volume creation fails and
  the partially created dataset is cleaned up.
- **Snapshot controller CRDs absent** — snapshot capability is not advertised; the driver
  runs without it rather than crash-looping.
- **Node loses connectivity to the NAS mid-workload** — iSCSI/NFS handle it at the kernel
  level; the driver reports through NodeGetVolumeStats and does not force-unmount.
- **Pool full** — `RESOURCE_EXHAUSTED`, and capacity reporting should have prevented
  scheduling in the first place.
- **Partial creation** (dataset made, extent failed) — the next retry finds the partial
  state and completes or rolls it back; no orphan is left unmarked.

## Acceptance criteria

- [ ] [S-1] `TestRejectsPlaintextEndpoint` — an endpoint URL with scheme http or ws is
      rejected at config validation with an explanatory error and no socket is opened;
      fails if any non-wss scheme reaches a connection attempt.
- [ ] [S-1] `TestAuthFailureIsTerminal` and `TestConnectionFailureRetries` — a rejected
      login exits fatally without a second attempt, while a dropped connection reconnects
      with backoff; fails if a login is ever retried.
- [ ] [S-1] `TestConcurrencyCapUnderLoad` — 100 concurrent operations produce zero -32000
      responses and in-flight calls never exceed the configured cap; fails if the semaphore
      is removed or raised above 20.
- [ ] [S-2] `TestCSISanity` runs the csi-sanity suite against the driver via
      `make test-sanity` and passes every case; fails if any CSI RPC returns a
      non-conforming status code.
- [ ] [S-3] `TestE2ENFSProvision` — a PVC with protocol nfs binds and the pod's statfs
      reports the requested size; fails if refquota is not set, which makes the pod see the
      whole pool.
- [ ] [S-3] `TestE2ENFSNonRootWrite` — a pod running as uid 1000 writes successfully to a
      fresh NFS volume; fails if provisioning skips the permissions step.
- [ ] [S-4] `TestE2EISCSIProvision` — a PVC with protocol iscsi binds and a pod reads back
      what it wrote; fails if any of zvol, extent, target or targetextent is not created.
- [ ] [S-5] `TestDeleteRefusesUnmarkedDataset` — DeleteVolume against a dataset with no
      ownership property, and against one whose property is inherited rather than LOCAL,
      refuses and leaves it intact; fails if the guard checks presence instead of source.
- [ ] [S-5] `TestVolumeIDConfinement` — volume IDs escaping the configured parent dataset,
      including traversal sequences, are rejected before any middleware call; fails if any
      crafted ID resolves outside the parent.
- [ ] [S-6] `TestE2ESnapshotRestoreIntegrity` — data is written, snapshotted, the source
      overwritten, then restored; the restored checksum equals the original; fails if the
      restore reads post-snapshot content.
- [ ] [S-6] `TestRestoredCloneIsManaged` — a restored volume carries a LOCAL ownership
      marker and the requested quota and can then be deleted; fails if the clone is left
      unstamped, which leaks it permanently.
- [ ] [S-6] `TestDeleteSnapshotWithDependentClone` — returns FAILED_PRECONDITION; fails if
      it deletes the snapshot or returns OK.
- [ ] [S-7] `TestE2EExpandMountedISCSI` — a mounted iSCSI PVC grows without unmount and
      pre-expansion data stays readable; fails if the node skips the device rescan.
- [ ] [S-7] `TestExpandRejectsShrink` — shrink requests fail on both backends; fails if the
      NFS path relies on middleware, which silently permits a refquota shrink.
- [ ] [S-8] `TestE2ENFSMountLifecycle` — mounts with the configured NFS version and
      unmounts leaving no residue in /proc/mounts; fails if the version parameter is ignored.
- [ ] [S-9] `TestISCSIDeviceResolution` — the device is resolved from the NAA under
      /dev/disk/by-id with no directory scan, and a volumeMode Block PVC is presented raw;
      fails if the code scans /dev or runs mkfs on a block volume.
- [ ] [S-9] `TestE2EUnstageLeavesLonghornIntact` — after NodeUnstage no session or node
      record remains for our target while pre-existing Longhorn sessions survive; fails if
      any unscoped iscsiadm command is issued.
- [ ] [S-10] `TestPreflightMissingTool` — on a node without mkfs.xfs a PVC requesting xfs
      fails naming the missing package; fails if it surfaces as a generic mount error.
- [ ] [S-10] `TestPreflightLoadsModules` — an available-but-unloaded module is loaded at
      startup and reported; fails if the plugin treats unloaded as unavailable.
- [ ] [S-11] `TestE2EVolumeStats` — kubelet's stats endpoint reports non-zero used and
      capacity bytes for a mounted volume of each protocol; fails if NodeGetVolumeStats is
      unimplemented.
- [ ] [S-12] `TestCapacityMatchesPool` — reported capacity tracks real pool free space and
      an oversized PVC stays Pending instead of failing at attach; fails if capacity is
      hardcoded or omitted.
- [ ] [S-13] `TestTopologyExcludesIncapableNode` — a pod is not scheduled onto a node
      lacking the required capability; fails if the node publishes topology labels it
      cannot honour.
- [ ] [S-14] `TestMetricsAndLogAttribution` — every CSI and middleware call is counted and
      timed with an error label and every volume log line carries the volume ID; fails if
      an operation completes with no metric observation.
- [ ] [S-14] `TestNoSecretsInOutput` — captured logs, errors, metric labels and process
      arguments contain neither the API key nor a CHAP secret; fails if any credential
      appears in output.
- [ ] [S-15] `TestConcurrentCreateIsIdempotent` and `TestConcurrentCreateDelete` — fifty
      identical CreateVolume calls yield exactly one dataset, and interleaved create/delete
      leaves no orphaned TrueNAS objects; fails if per-volume locking is removed.
- [ ] [S-16] `TestLeastPrivilegeAccount` runs the integration suite against a TrueNAS
      account holding only the documented 14 roles and passes; fails if any operation
      needs a broader role.
- [ ] [S-17] `TestChartInstallAndUpgrade` installs the chart on a clean cluster, binds a
      PVC, then upgrades without disturbing mounted volumes; fails if the upgrade restarts
      the node plugin in a way that breaks an in-use mount.
- [ ] [S-17] `TestChartWithoutSnapshotCRDs` — with the external-snapshotter CRDs absent the
      driver starts, logs clearly, and omits snapshot capability; fails if it crash-loops.
- [ ] [S-18] `TestReleaseArtifacts` confirms published images run on arm64 and amd64,
      that cosign verifies each signature, and that an SBOM is attached; fails if any
      published tag is unsigned or single-arch.
- [ ] [S-19] `TestOrphanReconcilerReports` — a marked dataset with no corresponding PV is
      reported in logs and a metric and is not deleted; fails if the reconciler deletes
      anything.
- [ ] [S-20] `TestE2ECHAPAttach` and `TestMultipathDegrades` — a CHAP-protected target
      attaches, and a node without multipath tooling still attaches single-path with a
      warning; fails if a missing multipathd makes attachment fail.
- [ ] [S-22] `TestBackendSelectionAndIsolation` — a StorageClass naming an unconfigured
      backend fails clearly, and one unreachable backend does not stall operations against
      a healthy one; fails if backends share a connection or a failure budget.
- [ ] [S-4] `TestLUNAllocationUnderConcurrency` — concurrent attaches to the shared target
      never assign a duplicate LUN id and allocation survives a controller restart; fails if
      the mapping is cached rather than derived from a live query.
- [ ] [S-20] `TestCHAPGeneratedPerTarget` — CHAP is configured by default, the credential
      is created on TrueNAS, and the node reads it back to log in; fails if CHAP is silently
      skipped or the credential is logged.
- [ ] [S-21] `TestIntegrationTeardownIsClean` asserts that after the full live-box
      integration run the NAS holds zero objects created by the suite; fails if a suite is
      skipped silently or any object survives teardown.

## Open questions
