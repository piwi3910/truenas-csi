# Changelog

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project uses [semantic versioning](https://semver.org/).

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

[0.1.0]: https://github.com/piwi3910/truenas-csi/releases/tag/v0.1.0
