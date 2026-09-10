# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [semantic versioning](https://semver.org/).

## [0.2.0] - 2026-09-10

125 commits since 0.1.3. Everything below was verified against a real TrueNAS
SCALE 25.10.6 appliance and an eight-node arm64 cluster, and the release passes
the upstream Kubernetes external-storage suite on all four protocols — iSCSI
46/46, NFS 27/27, NVMe-oF 46/46, SMB 27/27 — plus csi-sanity against the
appliance itself.

### Upgrade notes

Three changes alter behaviour a working cluster may depend on. None of them
touches an existing volume's data; all three change what the driver will accept
or require from here on.

- **A volume's required topology now names the filesystem the CO actually asked
  for.** Kubernetes conveys it through the reserved `csi.storage.k8s.io/fstype`
  parameter, which external-provisioner strips into the volume capability, so
  the driver never saw it and labelled every block volume `ext4`. A
  StorageClass asking for xfs now produces PVs that schedule only onto nodes
  advertising the xfs label. On a cluster where some nodes lack `xfsprogs`,
  pods that previously scheduled — onto a node that then failed to mount or
  grow the volume — will now stay Pending instead. Install `xfsprogs` on those
  nodes, or use a StorageClass whose fsType they can serve.
- **CreateVolume refuses a filesystem this driver cannot make**, by name, on the
  PVC. Previously such a class bound a PV whose topology no node could satisfy,
  and the only diagnostic was a scheduler message about an unknown label.
- **CreateVolume refuses a clone or restore whose destination StorageClass names
  a different filesystem from the source.** A ZFS clone copies bytes, so the
  filesystem comes from the source whatever the class says; the volume used to
  attach and then fail to mount for ever with a bad-superblock error naming
  neither snapshot nor filesystem.

### Added

- **Delete protection.** `DeleteVolume` can retire a volume into a graveyard
  dataset instead of destroying it, with a grace period and a reaper that
  destroys only driver-owned datasets inside the graveyard that carry a deletion
  timestamp and are past their grace. Off by default.
- **`ControllerModifyVolume`**, driven by a `VolumeAttributesClass`, over a
  closed allowlist of ZFS properties that can be changed on a mounted volume
  without affecting data already written: `sync`, `compression`, `atime` and
  `recordsize`. Every accepted value was checked against the appliance, in both
  directions — the allowlist is exactly what `pool.dataset.update` takes.
- **Per-volume I/O limits** for the block protocols, applied on the node with
  cgroup v2 `io.max`. TrueNAS has no per-dataset throttle, so this is a per-pod,
  per-node limit and says so; a limit on an NFS or SMB volume is refused rather
  than silently ignored.
- **Per-volume I/O metrics**, read from the node's own kernel counters and
  labelled with the pod, namespace and claim, so a volume's traffic joins to the
  workload using it. The reader only ever touches procfs, so a scrape cannot be
  blocked by a hung mount.
- **Opt-in per-node access control**: the node plugin publishes its own NQN and
  IQN as annotations, which is what lets the controller grant appliance-side
  access to one node rather than to the cluster.
- **Appliance diagnostics metrics**, which were collected but exported by
  nothing, and a `nightly` image channel for testing before a version tag.

### Fixed

The whole of this section was found by running against real hardware.

- **iSCSI LUN ids are recycled, and the node never released them.** Every volume
  is a LUN on one shared target, and the appliance hands out the lowest free id.
  Unstage deleted no device, so a node kept a disk at a LUN the appliance later
  gave to a different volume: the next volume to land there could never be
  staged on that node, failing for ever because its block device never appeared
  after the attach, and a retry could not help. Unstage now removes the device,
  and a stage that cannot find its own makes one recovery attempt scoped to its
  own LUN.
- **A staged device that has become a different volume is now reported.** A node
  fenced from a volume keeps a mounted device at a LUN the controller has since
  reassigned; it passes every reachability probe while the pod writes into
  another claim's data. The check re-reads the device's identity — a cached read
  agrees with itself no matter what is behind the LUN — and holds its verdict
  between checks.
- **Raw block volumes were invisible to the shared-session guard.** Their mount
  is recorded with the source `udev` and names no disk, so a node whose iSCSI
  volumes were block ones logged out of the shared target when any one of them
  was unstaged, taking its siblings' data paths down.
- **Block volume statistics described the host, not the volume.** `statfs` on a
  device node answers about devtmpfs, so a 1 GiB block PVC reported 16.4 GB —
  half the node's RAM, and the same figure for every block volume on it —
  through `kubelet_volume_stats_capacity_bytes` and every alert built on it.
- **Volume monitoring stopped at every node plugin restart.** The health
  monitor's targets, the I/O series and the single-writer reservation all live
  in memory and are registered by `NodeStageVolume`, and the kubelet does not
  re-issue it for a volume it already considers staged: measured, a restarted
  plugin received zero stage and publish calls. All three are now rebuilt at
  startup from the records this driver already keeps, under a deadline so that a
  stuck mount belonging to someone else cannot stop the plugin from starting.
- **StorageClass mount settings were lost between create and publish** for NFS
  and SMB, because those calls run on separate leader elections and a value
  cached in memory at create is not there at publish. A class asking for NFSv3
  mounted 4.2; a class asking for mode 0777 produced 0755 and a pod that could
  not write. Both are now recorded on the dataset.
- **A ZFS clone inherits nothing held locally**, which cost a family of bugs:
  cloned volumes lost their protocol marker, their thick provisioning, and the
  ACL type their SMB share needs. A clone abandoned by a controller crash is now
  resumed rather than stranded.
- **The middleware's display form is not its machine form.** `origin` is
  returned uppercased for display, and three guards that compared against it
  were dead. The parsed form is now the only one reachable from the type.
- **A disabled share, extent, NVMe port or namespace is refused at publish**
  rather than adopted, and the NVMe port shared by every volume is no longer
  torn down when one volume's create fails.
- **The orphan report named every namespace's parent dataset**, for ever, on any
  cluster with namespace quotas enabled — a permanent false positive is worse
  than no report. Its own comment already said to skip it.
- **Capacity is reported in bytes the pool can actually store.** `pool.query`
  returns raw bytes including RAIDZ parity: 41.05 TiB raw against 30.33 TiB
  writable on a 12-disk RAIDZ2, so the scheduler was told there was a third more
  room than existed.
- Volumes smaller than the appliance's 1 GiB refquota floor are rounded up and
  reported honestly instead of failing with a schema error naming neither the
  limit nor the field; a refused shrink no longer tells the resizer to retry; a
  device is never formatted when `blkid` could not answer for it; expansion
  waits for the size that was asked for rather than for any change; and a
  StorageClass parameter nothing reads is refused instead of silently ignored.

### Security

- The least-privilege role definitions were incomplete — SMB needed a role of
  its own, and the test that would have caught it skipped. CHAP secrets are kept
  out of the PersistentVolume from both of the sources that carried them.
- Disabled TLS verification is now warned about on every connection, and the
  documentation workflow is pinned.

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

[0.2.0]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.2.0
[0.1.3]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.3
[0.1.2]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.2
[0.1.1]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.1
[0.1.0]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.0
