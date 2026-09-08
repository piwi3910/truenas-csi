# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [semantic versioning](https://semver.org/).

## [0.1.3] - 2026-09-08

### Fixed

- **Replication now actually runs** (#10). `StorageProtectionGroup` could be
  applied and nothing would ever reconcile it: no binary constructed the
  reconciler, and the chart never installed the CRD. The reconciler starts in
  the driver's controller pod behind `replication.enabled`, leader-elected,
  because the replication manager needs appliance clients that exist only in
  that pod. `Appliances.Client` also returned `*truenas.Client` while the
  registry hands out a `truenas.API`, so the interface its own comment claimed
  the registry satisfied could not be implemented by it — part of why this was
  never wired.
- **Pool administration and volume migration are reachable** (#11), as
  `truenas-csi pool status|disks|alerts` and `truenas-csi migrate`. Both were
  implemented, tested and documented with no binary exposing them. Migration
  plans by default and needs `-apply` plus a `-confirm` repeating the target
  claim's name.

### Added

- A CI check that fails when any `internal/` package has no non-test importer
  (#12). Three features shipped unreachable this week, one of which made every
  PVC unschedulable; `golangci-lint`'s `unused` cannot see this class, because
  it reports only unexported symbols.

### Changed

- `go.work` no longer claims controller-runtime is kept out of the driver's
  build. It is in it, for the replication reconciler, and the comment now says
  so along with the cost and the alternative.

## [0.1.2] - 2026-09-08

### Fixed

- **The node reachability probe was never wired, so every dynamically
  provisioned volume was unschedulable.** The controller requires a
  `csi.truenas.watteel.com/backend-<name>` segment for each volume, and the node
  code that publishes it — `ProbeReachability` / `SetReachability` — had no
  caller. Volumes were created on the appliance, PVs bound, and then every pod
  failed with `node(s) didn't match PersistentVolume's node affinity`. Found by
  installing on a real cluster; no unit test could see it, because both halves
  were correct and only the call site was missing.
- **A pool-prefixed `parentDataset` is now refused at startup.** Every install
  example said `tank/k8s`, which the driver reads as `tank/tank/k8s`; it started
  healthy and failed the first PVC with a message naming a dataset component
  rather than the setting. The examples are corrected.

### Known

Three subsystems remain implemented but unreachable from any binary, now
tracked rather than implied to work: replication (#10), pool administration and
volume migration (#11). See #12 for the CI check that would have caught all of
them, and #9 for the reachability bug above.

## [0.1.1] - 2026-09-08

### Changed

- Every module, image and workflow now builds with **Go 1.27**, the current
  stable release. The `go` directive is `1.27.0` rather than `1.27.1` on
  purpose: it is a minimum, and golangci-lint refuses a module targeting a Go
  newer than the one it was itself built with, so `1.27.1` would fail lint on
  every run while changing nothing about the language version.

No functional change. Rebuilt so the published binary is the one the source
tree is tested against.

## [0.1.0] - 2026-09-08

First release. Validated end to end against a live TrueNAS SCALE 25.10.6
appliance and an 8-node arm64 k3s 1.34 cluster.

### Added

**Protocols.** NFS, iSCSI, SMB and NVMe/TCP from one binary, selected per
StorageClass. Each is a backend behind one interface, so a fifth is an
implementation rather than a change.

**CSI surface.** Dynamic and static provisioning, snapshots, restore,
clone-from-volume, online and offline expansion, `GET_CAPACITY`,
`LIST_VOLUMES` with published nodes, `ControllerGetVolume` and
`ControllerGetVolumeHealth`, node volume stats and health, and the upstream
`GroupController` service for crash-consistent group snapshots. csi-sanity
conformance runs in CI.

**Appliance-side fencing.** `ControllerUnpublishVolume` revokes access on the
appliance: iSCSI by unmapping the LUN, NFS and SMB by narrowing the share's host
access list. An opt-in, leader-elected controller consumes a connectivity
service that answers from the appliance's own session and NFSv4 lease state, and
force-deletes a pod only once every volume's revoke has succeeded.

**Replication.** `StorageProtectionGroup` over ZFS replication, with failover,
failback, suspend, resume, and a rehearsal failover that clones the last
replicated snapshot without touching production.

**Operator.** A `TrueNASCSIDriver` CRD with an OLM bundle, which refuses an
upgrade or downgrade that skips further than the declared window rather than
applying it.

**Observability.** Array metrics (pool, dataset, iSCSI sessions, NFS clients,
scrub and disk health), per-volume I/O counters read from the node's own kernel
counters, and a Grafana dashboard. Metrics collection is leader-elected so extra
replicas do not multiply load against the appliance's concurrency ceiling.

**Operations.** Credential hot-reload without a restart, log level changed live
through a watched ConfigMap, a rate limiter and circuit breaker on appliance
calls, topology labels that keep the scheduler off nodes that cannot serve a
volume, an orphan reconciler, volume migration, pool administration, and
optional per-namespace ZFS quotas.

### Security

- The driver **refuses a plaintext endpoint**. TrueNAS permanently revokes an
  API key presented over one, so this is enforced at configuration validation
  rather than discovered at first use.
- Every destructive path checks an ownership property with `source == LOCAL`
  before acting, so a dataset the driver did not create is never destroyed.
- An NFS export's host list is never written empty. On TrueNAS an export with
  both `hosts` and `networks` empty is exported to **everyone**, so revoking the
  last node naively would open the volume rather than close it; a single choke
  point makes that state unreachable, and four tests fail if it is removed.
- SMB credentials never reach a process argument list, where `/proc` would make
  them readable node-wide. They are written to a `0600` file on tmpfs and
  removed on every path, including failures.

### Known limitations

- **SMB cannot be fenced.** TrueNAS 25.10 publishes no SMB session or status
  call, so SMB connectivity is unobservable and an SMB-only pod is never
  force-deleted. This is the safe direction, but it is a real gap.
- **The appliance cannot report per-volume I/O.** `reporting.get_data` exposes 40
  graphs and none is per-dataset or per-zvol, so per-volume performance is
  measured node-side. Only _mounted_ volumes are visible, and a volume's series
  moves between node exporters when its pod reschedules.
- **Replication is single-cluster.** It moves data and flips primacy; it does not
  create the target-side PersistentVolume in a second cluster.
- **Ephemeral inline volumes are deliberately unsupported.** They are provisioned
  in `NodePublishVolume` with no controller involvement, so the fence — which
  lives in `ControllerUnpublishVolume` — would never run for them.
- **TrueNAS CORE and NVMe over RDMA/RoCE are implemented but unverified** against
  hardware, and are marked as such in the source.
- Populated `iscsi.global.sessions` and `nvmet.global.sessions` element shapes
  are still unverified; both carry `UNVERIFIED:` markers naming the check.

### Upgrading

Not applicable — this is the first release. Note for later: `attachRequired` on
the CSIDriver object is immutable, so any future change to it requires the
object to be recreated.

[0.1.3]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.3
[0.1.2]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.2
[0.1.1]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.1
[0.1.0]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.0
