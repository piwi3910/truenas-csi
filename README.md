# truenas-csi

A Container Storage Interface driver for **TrueNAS SCALE 25.10 and newer**. It provisions
ZFS-backed PersistentVolumes on a TrueNAS appliance and speaks the API TrueNAS actually
supports today: **JSON-RPC 2.0 over a `wss://` websocket** at `/api/current`. A backend may
instead be marked `flavour: core` to speak TrueNAS CORE's legacy REST v2 API — see
[TrueNAS CORE](#truenas-core-implemented-but-unverified) below, and note that that path is
UNVERIFIED against real CORE hardware.

Driver name: `csi.truenas.watteel.com` (immutable once PersistentVolumes exist — changing
it orphans every PV). Go module: `github.com/piwi3910/truenas-csi`. Licence: Apache 2.0.

**Protocols: NFS, iSCSI and NVMe/TCP**, all three validated end to end against a live
appliance and real cluster nodes. SMB was validated too but does not ship yet. **NVMe over
RDMA/RoCE is implemented but UNVALIDATED**: the target appliance reports
`nvmet.global.rdma = false` and the arm64 nodes have no RDMA-capable NICs, so the driver
refuses `transport: rdma` on an appliance that reports no RDMA rather than exporting a
volume nothing can connect to.

---

## ⚠️ The endpoint MUST be `wss://`

TrueNAS 25.10 **revokes an API key the moment it is presented over a plaintext
connection** — "API key revoked due to insecure transport". It does not refuse the login;
it destroys the credential. Three keys were burned this way during research.

A plaintext connection therefore does not merely fail: it takes down every volume
operation in the cluster until an operator issues a new key by hand.

The driver refuses to start when a backend endpoint uses `http:`, plaintext `ws:` or any other
non-`wss` scheme, and rejects it at config validation before a socket is opened. Do not
work around this. If a connection was ever attempted over plaintext, assume the key is
already dead and issue a new one.

Because authentication failure can be a symptom of a revoked key, **a failed login is
fatal and is never retried** — a reconnect loop could revoke the replacement key too. Only
_connection_ failures are retried with backoff.

---

## TrueNAS CORE: implemented, but UNVERIFIED

A backend may set `flavour: core` to talk to a TrueNAS CORE appliance over its legacy
**REST v2 API** (`https://<nas>/api/v2.0`, `Authorization: Bearer <apikey>`) instead of
SCALE's JSON-RPC websocket. `flavour: scale` is the default and is unchanged.

**This path has never run against a real CORE appliance.** No CORE hardware was available.
The request routing and payload shapes follow the documented REST v2 conventions and are
exercised only against a recorded in-process fake
(`internal/truenas/core/fake`). Everything above the transport — dataset, snapshot, share
and iSCSI logic — is the SAME code SCALE uses, and a parity test drives both clients
through identical operations and asserts identical results, so the two cannot drift in
behaviour. What is unproven is narrower but real: whether a CORE box answers these paths,
verbs and payloads the way the documentation says.

Treat CORE as **experimental**. Run it against a scratch pool first, and read the CORE
section of [docs/troubleshooting.md](docs/troubleshooting.md) before reporting a bug.

```yaml
backends:
  nas-core:
    flavour: core
    endpoint: https://nas-core.example.com/api/v2.0 # https:// only — see below
    username: root
    apiKey: "1-abcdef..."
    pool: tank
    parentDataset: tank/k8s
```

**HTTPS is as mandatory for CORE as `wss://` is for SCALE.** The credential-revocation rule
is a property of the API key, not of the transport, so the driver refuses a `http://`
endpoint at config validation for exactly the same reason — and a 401 from CORE is
terminal and never retried, so a rejected key is presented once and only once.

The flavour is validated as a closed set: anything other than `scale` or `core` fails at
startup rather than silently defaulting.

---

## Requirements

| Component    | Requirement                                                                               |
| ------------ | ----------------------------------------------------------------------------------------- |
| Kubernetes   | 1.31 or newer (validated on k3s 1.34)                                                     |
| TrueNAS      | SCALE 25.10 or newer. CORE via `flavour: core` is experimental and UNVERIFIED             |
| Transport    | `wss://` to `/api/current` (SCALE), or `https://` to `/api/v2.0` (CORE)                   |
| Credentials  | A TrueNAS account with the documented 14 roles — see [docs/security.md](docs/security.md) |
| Architecture | arm64 is the primary and validated architecture; amd64 images are built too               |
| Snapshots    | The external-snapshotter CRDs and controller, installed cluster-wide (see below)          |

### Node package prerequisites

The node plugin **ships no storage tooling of its own**. At startup it probes the host
filesystem for the binaries and kernel modules each capability needs, `modprobe`s the
modules it requires, advertises through CSI topology only the capabilities it can actually
deliver, and logs exactly what an operator must install for anything missing. A
StorageClass asking for a capability a node lacks fails fast with a message naming the
missing package instead of a cryptic mount error.

| Capability              | Debian/Ubuntu package | Needs                                            |
| ----------------------- | --------------------- | ------------------------------------------------ |
| iSCSI                   | `open-iscsi`          | `iscsiadm`, `iscsid`, module `iscsi_tcp`         |
| NFS                     | `nfs-common`          | `mount.nfs`                                      |
| ext4                    | `e2fsprogs`           | `mkfs.ext4`, `resize2fs`                         |
| XFS                     | `xfsprogs`            | `mkfs.xfs`, `xfs_growfs`                         |
| multipath               | `multipath-tools`     | `multipath`, `multipathd`, module `dm_multipath` |
| NVMe-oF                 | `nvme-cli`            | `nvme`, module `nvme_tcp`                        |
| SMB (deferred protocol) | `cifs-utils`          | `mount.cifs`, module `cifs`                      |

Notes:

- Binaries are probed by absolute path inside the **host** mount namespace, not through
  the container's `PATH`.
- A module that is present on disk but not loaded is a recoverable state, not an
  unavailable one: the plugin loads it and continues. Nothing is written to
  `/etc/modules-load.d`, so a module the driver loaded does not survive a reboot until the
  driver loads it again — which is intended.
- **multipath is the one capability that degrades rather than fails**: without
  `multipath-tools` the volume still attaches on a single path, with a warning.
- Installing `multipath-tools` is a deliberate change, not a side effect: `multipathd`
  claims block devices on sight and can seize devices already in use by another iSCSI
  consumer on the node (Longhorn, for example) unless `/etc/multipath.conf` blacklists
  them first.
- The node plugin shares the host's iSCSI stack (`iscsid`, `/etc/iscsi`,
  `/var/lib/iscsi`) with anything else using iSCSI on that node. Every `iscsiadm`
  invocation is scoped to this driver's own target and portal — no `--logoutall`, no
  unscoped `-o delete`, no global session rescan, and `iscsid.conf` is never rewritten.

---

## Installation

### 1. Prepare the TrueNAS side

Create a dedicated TrueNAS account and API key holding **only** the 14 documented roles,
and decide how the driver will trust the appliance's certificate. Both are covered in
[docs/security.md](docs/security.md). Pick the pool and the parent dataset the driver is
allowed to manage; the driver is confined to that dataset and refuses to act outside it.

### 2. Install the snapshot controller — a prerequisite, not part of this chart

The `VolumeSnapshot` CRDs and the snapshot controller are a **cluster-wide singleton owned
by the cluster, not by any one CSI driver**. Two drivers installing them fight over the CRD
version and break snapshots for both. The chart therefore ships `snapshotter.install:
false` and that value should stay `false`.

Install [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter)
once, cluster-wide, before enabling snapshots. If the CRDs are absent the driver still
starts, logs clearly, and simply does not advertise snapshot capability — it does not
crash-loop.

### 3. Install the chart

```yaml
# values.yaml
backends:
  nas1:
    endpoint: wss://nas1.example.com/api/current # wss:// only
    username: csi
    apiKey: "1-abcdef..."
    pool: tank
    parentDataset: tank/k8s
    # Pool headroom this driver will never hand out. See "Pool reservation" below.
    reservedBytes: 53687091200 # 50 GiB
    reservedPercent: 10 # 10% of the pool's total size
    # The stock TrueNAS certificate is self-signed with SAN=DNS:localhost and cannot
    # be verified against a real address. Supply a PEM bundle here.
    caCert: |
      -----BEGIN CERTIFICATE-----
      ...
      -----END CERTIFICATE-----
    insecureSkipVerify: false

node:
  # k3s and microk8s do not use the default kubelet root.
  kubeletDir: /var/lib/kubelet

snapshotter:
  enabled: true # requires the CRDs and controller from step 2
  install: false # leave false
```

```sh
helm upgrade --install truenas-csi deploy/helm/truenas-csi \
  --namespace truenas-csi --create-namespace \
  -f values.yaml
```

Credentials are rendered into a Secret mounted at `/etc/truenas-csi/config.yaml`; they
never appear in a container argument or a plaintext environment variable. Set
`existingSecret` to a Secret you manage yourself (holding key `config.yaml`) to keep them
in an external secret store instead — `backends` is then ignored.

#### Pool reservation

TrueNAS needs pool headroom of its own — system datasets, snapshots that already exist on
the pool, replication targets. Left alone, the driver reports the pool's entire free space
and PVCs can fill it to the last byte, at which point snapshots and replication start
failing and the appliance itself degrades. Two per-backend options reserve that headroom:

| Backend option    | Values                        | Default              |
| ----------------- | ----------------------------- | -------------------- |
| `reservedBytes`   | absolute bytes, non-negative  | `0` (no reservation) |
| `reservedPercent` | `0`–`100`, of the pool's size | `0` (no reservation) |

Both are optional. When **both** are set the **larger** of the two reservations wins — they
are two ways of stating the same headroom, not two headrooms to be added together. A
`reservedPercent` outside `0`–`100`, or a negative `reservedBytes`, is rejected at startup.

The reservation is enforced twice, because reporting it alone would not be enough:

- `GetCapacity` reports `pool free − reserve`, never less than zero, so the scheduler and
  `CSIStorageCapacity` already see the reduced figure.
- `CreateVolume` refuses a claim that would eat into the reserve with `ResourceExhausted`,
  naming the pool free space, the reserve and the request — a PVC can be created larger
  than the reported capacity, so the check has to be made at provisioning time too.

Multiple appliances are configured as multiple entries under `backends`; each gets its own
connection, concurrency budget and capacity figures, and one unreachable appliance does not
stall operations against a healthy one. The backend name is part of every volume ID, so it
is fixed for the life of a volume.

### 4. Create a StorageClass

The chart ships two disabled example classes (`storageClasses.nfs`, `storageClasses.iscsi`)
which can be enabled and edited, or write your own:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-nfs
provisioner: csi.truenas.watteel.com
allowVolumeExpansion: true # ZFS cannot shrink; expansion is one-way
reclaimPolicy: Delete
parameters:
  backend: nas1
  protocol: nfs
  nfsVersion: "4"
  networks: 10.0.0.0/8
  mode: "0770"
  uid: "1000"
  gid: "1000"
```

---

> **Install the node packages before installing the driver.** The driver publishes each
> node capability as a topology label, and Kubernetes makes topology labels immutable.
> Adding `xfsprogs` or `open-iscsi` to a node the driver has already registered makes its
> node plugin fail to re-register until the labels are cleared by hand — see
> [troubleshooting](docs/troubleshooting.md).
>
> **The same applies to network routes.** Each node also probes, at startup, whether it can
> reach each configured appliance's data path (a bounded TCP dial to NFS 2049 and iSCSI
> 3260, never ICMP) and publishes `csi.truenas.watteel.com/backend-<name>` as `true` or
> `false`; the controller requires `true` for a volume's own backend, so a node with no
> route to an appliance is excluded from scheduling instead of failing at mount. Because
> the value is immutable too, give a node its route to the appliance **before** the driver
> first registers there — see [troubleshooting](docs/troubleshooting.md).

## StorageClass parameters

A parameter left empty means "use the default"; that is not an error.

| Parameter       | Applies to | Values                                                              | Default                                                                |
| --------------- | ---------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `backend`       | all        | a name from the chart's `backends` map                              | required, unless exactly one backend is configured                     |
| `protocol`      | all        | `nfs`, `iscsi`, `nvme`                                              | required                                                               |
| `pool`          | all        | ZFS pool name                                                       | the backend's configured pool                                          |
| `parentDataset` | all        | dataset every volume is created under                               | the backend's configured parent dataset                                |
| `fsType`        | iscsi      | `ext4`, `xfs`                                                       | `ext4`                                                                 |
| `sparse`        | iscsi      | `true`, `false`                                                     | `true`                                                                 |
| `volblocksize`  | iscsi      | e.g. `16K`, `128K`                                                  | the appliance's `pool.dataset.recommended_zvol_blocksize` for the pool |
| `server`        | nfs        | address nodes mount the export from                                 | the appliance's own endpoint host                                      |
| `nfsVersion`    | nfs        | `3`, `4`                                                            | `4`                                                                    |
| `networks`      | nfs        | comma-separated CIDRs allowed to mount the export                   | none (no network restriction)                                          |
| `maproot`       | nfs        | `user:group` mapped to root on the export                           | unset                                                                  |
| `mode`          | nfs        | dataset permissions applied at provisioning                         | `0770`                                                                 |
| `uid`           | nfs        | owning uid applied at provisioning                                  | `0`                                                                    |
| `gid`           | nfs        | owning gid applied at provisioning                                  | `0`                                                                    |
| `portalID`      | iscsi      | reuse this existing portal instead of letting the driver create one | unset (driver creates the portal)                                      |
| `chap`          | iscsi      | `true`, `false` — CHAP on the shared target                         | `true`                                                                 |
| `initiatorACL`  | iscsi      | `true`, `false` — restrict the target to the cluster's node IQNs    | `true`                                                                 |
| `nodeIQNs`      | iscsi      | comma-separated node IQNs allowed on the shared target              | unset — no initiator group is created, so the target stays open        |
| `multipath`     | iscsi      | `true`, `false` — use multipath where the node supports it          | `false`                                                                |
| `transport`     | nvme       | `tcp`, `rdma` — NVMe-oF transport (see below)                       | `tcp`                                                                  |
| `hostNQNs`      | nvme       | comma-separated initiator NQNs allowed on the subsystem             | unset — the subsystem is created with `allow_any_host`                 |
| `sparse`        | nvme       | `true`, `false`                                                     | `true`                                                                 |
| `volblocksize`  | nvme       | e.g. `16K`, `128K`                                                  | the appliance's `pool.dataset.recommended_zvol_blocksize` for the pool |
| `portAddress`   | nvme       | address the NVMe-oF port listens on                                 | the appliance's own endpoint host                                      |
| `port`          | nvme       | transport service id                                                | `4420`                                                                 |

### `pool` and `parentDataset` are policy, not a redirect

If a StorageClass states `pool` or `parentDataset`, the value must **match the backend's
configuration**. A mismatch is rejected with `InvalidArgument`; it is never honoured. These
parameters exist so a class can restate operator policy for readability, not so a
user-writable StorageClass can point the driver at another part of the pool. Volume IDs are
confined to the configured parent dataset and checked before any TrueNAS call is made.

### `sparse` changes failure behaviour, not just accounting

TrueNAS refuses to create a **thick** zvol larger than 80% of the pool's free space. With
`sparse: "false"` a large PVC can therefore be refused outright (surfaced as
`RESOURCE_EXHAUSTED`) where a sparse one would have been created.

### `transport: rdma` is implemented but UNVALIDATED

NVMe over RDMA/RoCE is written and wired, but not one byte has moved over it: the
validation appliance reports `nvmet.global.rdma = false` and the cluster's RK3588 nodes
have no RDMA hardware, so the path cannot be exercised here. The driver therefore
**refuses** `transport: rdma` against an appliance reporting `rdma = false`, rather than
creating a port nothing can connect to and failing much later at attach time. Treat RDMA
as untested until someone runs it on hardware that has it. NVMe/TCP is validated end to
end.

### `hostNQNs` empty means "any host", not "no host"

An NVMe subsystem with `allow_any_host = false` and an empty host ACL accepts **nobody**.
Writing that when no NQNs are configured would provision every volume cleanly and then
fail every attach, so an empty `hostNQNs` creates the subsystem with `allow_any_host`
instead. Setting `hostNQNs` closes the subsystem and registers exactly those initiators
(`nvmet.host` + `nvmet.host_subsys`). This is the same rule the iSCSI backend applies to
an empty initiator group.

### NVMe uses one subsystem per volume

Unlike the iSCSI backend's single shared target, each NVMe volume gets its own subsystem.
A namespace only exists inside a subsystem and host ACLs are a property of the subsystem,
so per-volume subsystems keep exposure and teardown per-volume. The transport **port** is
the one shared object: it is queried first, created only when absent, and never removed by
a volume's delete or rollback.

### `volblocksize` is visible to the initiator

A zvol's `volblocksize` surfaces to the initiator as the device's _physical_ sector size
and interacts with filesystem creation (`mkfs.xfs` will warn and fall back to the logical
sector size when its block size is smaller). It is not purely a performance knob.

---

## Protocol and access-mode matrix

|           | ReadWriteOnce | ReadOnlyMany | ReadWriteMany | ReadWriteOncePod | Block volumeMode |
| --------- | ------------- | ------------ | ------------- | ---------------- | ---------------- |
| **NFS**   | yes           | yes          | **yes**       | yes              | no               |
| **iSCSI** | yes           | no           | **no**        | yes              | yes              |
| **NVMe**  | yes           | no           | **no**        | yes              | yes              |

**iSCSI is single-node by nature.** A volume is one zvol exported as one block device; two
nodes writing the same block device with independent page caches and a non-cluster
filesystem corrupts it. `ReadWriteMany` is not offered for iSCSI, and no clustered
filesystem is provided.

NFS supports `ReadWriteMany` because the export is a filesystem with server-side locking.
Prefer NFSv4: v3 needs the separate lock manager (`rpc-statd`) on every node and its
stateless locking interacts badly with pod rescheduling.

Read [docs/security.md](docs/security.md) before relying on `ReadWriteOnce` as an isolation
boundary: the driver uses one **shared** iSCSI target per backend, so every node logged in
can see every LUN. RWO is enforced by Kubernetes, not below it.

---

## Node access is granted at attach and revoked at detach

Appliance-side access is **per node and per attachment**, not permanent:

| Protocol | Granted by `ControllerPublishVolume`          | Revoked by `ControllerUnpublishVolume` |
| -------- | --------------------------------------------- | -------------------------------------- |
| iSCSI    | the `iscsi.targetextent` LUN mapping          | the mapping is deleted                 |
| NVMe-oF  | the subsystem's binding to the transport port | the binding is deleted                 |
| NFS      | the node's addresses in the export's `hosts`  | those addresses are removed            |
| SMB      | the node's addresses in `options.hostsallow`  | those addresses are removed            |

The revoke is a **fence**: after it returns the named node cannot reach the volume's data
whatever state its kernel is in. That is what makes automated recovery from a stuck node
possible, and it is why the CSIDriver object declares `attachRequired: true` — a container
orchestrator only calls `ControllerUnpublishVolume` for a driver that has an attach step.

Two consequences worth knowing:

- **A file share is never left with an empty access list.** On TrueNAS an NFS export is
  reachable by everyone when its `hosts` **and** `networks` lists are both empty, so
  removing the last node would open the volume to the world rather than close it. Every
  export this driver manages therefore keeps one unroutable sentinel host (`192.0.2.1`,
  RFC 5737) that no client can present, and every SMB share it manages carries
  `hostsdeny: ["ALL"]` so its `hostsallow` list is authoritative.
- **The `networks` StorageClass parameter is a policy filter, not an export field.** The
  appliance ORs `hosts` with `networks`, so a network left on the export would let any
  address inside it through and defeat the per-node fence. The driver keeps the export's
  `networks` empty and instead refuses to grant a node whose addresses fall outside the
  configured networks. Restricting a volume to `10.0.0.0/8` still means "only nodes in
  10.0.0.0/8" — it just no longer means "and anything else in 10.0.0.0/8 as well".

### Upgrading to the fenced attach model

`spec.attachRequired` on a CSIDriver object is **immutable**, so `helm upgrade` alone
cannot switch it on. The object has to be deleted and recreated:

```sh
kubectl delete csidriver csi.truenas.watteel.com
helm upgrade truenas-csi deploy/helm/truenas-csi -n truenas-csi
```

During the window between those two commands:

- **Safe:** volumes that are already mounted. The mount is a node-side fact; reads and
  writes continue uninterrupted, and nothing revokes access to a running workload.
- **Not safe:** anything needing a _new_ attachment — a pod starting or rescheduling, a
  new PVC being bound. These fail while the object is absent and retry on their own once
  it is back, so keep the window short rather than draining the cluster.

Volumes provisioned before this change already have their iSCSI LUN mapping or NVMe port
binding. The first publish **adopts** what is there instead of creating a second one, and
records its LUN id so it stays stable, so no running workload has to be restarted. Their
NFS and SMB shares are, however, still open as they were created: they are narrowed to the
attaching node the first time each is published, and until then they remain reachable as
before.

---

## Capabilities

Controller: `CREATE_DELETE_VOLUME`, `PUBLISH_UNPUBLISH_VOLUME`, `CREATE_DELETE_SNAPSHOT`,
`LIST_SNAPSHOTS`, `LIST_VOLUMES`, `EXPAND_VOLUME`, `CLONE_VOLUME`, `GET_CAPACITY`.
GroupController: `CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT`.
Node: `STAGE_UNSTAGE_VOLUME`, `EXPAND_VOLUME`, `GET_VOLUME_STATS`.

- **Snapshots and restore.** `CreateSnapshot`/`DeleteSnapshot`/`ListSnapshots`, and
  `CreateVolume` from a snapshot source. A restored volume is a ZFS clone that stays
  dependent on its snapshot, so `DeleteSnapshot` returns `FAILED_PRECONDITION` while a
  restored volume still depends on it — as the CSI spec prescribes. Deleting the PVC is
  never blocked by a clone. Cloning directly from a volume is not supported; snapshot it
  first.
- **Volume group snapshots.** `CreateVolumeGroupSnapshot`/`DeleteVolumeGroupSnapshot`/
  `GetVolumeGroupSnapshot` capture several volumes at a single instant, which is what makes
  a restore of a database's data, WAL and log PVCs consistent with itself.

  **Every member must live on the same backend and share a parent dataset.** ZFS's only
  multi-dataset atomic primitive is a recursive snapshot of a common ancestor: one call,
  one transaction group, one instant. Members without a common parent — or spread across
  two appliances — would have to be snapshotted one at a time, in as many transaction
  groups, and the result would not be crash-consistent however it was labelled. The driver
  therefore refuses such a group (`FAILED_PRECONDITION` for a missing common parent,
  `INVALID_ARGUMENT` across appliances) rather than hand a database a restore point that
  looks valid and is not. Volumes provisioned by one backend share its configured parent
  dataset, so this holds by construction for ordinary use.

  Each member snapshot carries the same id format as a single-volume snapshot and can be
  restored on its own through `CreateVolume`. Deleting a group is blocked with
  `FAILED_PRECONDITION` while any member still has a dependent clone, and — as with
  `DeleteSnapshot` — the clone is never promoted, because promotion inverts the dependency
  and would strand the source volume instead.

- **Expansion is grow-only**, and works live: a mounted iSCSI volume grows by device rescan
  plus filesystem grow, with no unmount and no pod restart. Shrink is rejected by the
  driver on both backends.
- **Capacity reporting.** `GetCapacity`/`CSIStorageCapacity` from real pool free space, so
  an oversized PVC stays `Pending` instead of failing at attach.
- **Topology.** Each node publishes one segment per capability
  (`csi.truenas.watteel.com/<capability>` = `true`/`false`), so a volume is steered to a
  node that can actually mount it.
- **Orphan reconciler.** Datasets this driver owns that have no matching PersistentVolume
  are reported in logs and in a metric. It never deletes anything.

## Observability

Prometheus metrics on `metricsAddr` (default `:9090`) and health on `healthAddr` (default
`:9808`), plus a gRPC probe for the liveness sidecar. The exported series are
`truenas_csi_calls_total`, `truenas_csi_call_duration_seconds`,
`truenas_csi_middleware_calls_total`, `truenas_csi_middleware_duration_seconds`,
`truenas_csi_backend_up` and `truenas_csi_orphaned_volumes`. Label values are bounded to
method and backend names — a volume ID or a credential never becomes a label. Volume IDs
appear in log lines instead, so a failing volume can be traced end to end.

### Array-level metrics

The controller also polls each appliance for its own figures and exports them on the same
endpoint, so the backend can be watched without handing anyone a TrueNAS login:

| Metric                                | Labels               | Meaning                                                                  |
| ------------------------------------- | -------------------- | ------------------------------------------------------------------------ |
| `truenas_pool_size_bytes`             | `backend`, `pool`    | Total pool size.                                                         |
| `truenas_pool_free_bytes`             | `backend`, `pool`    | Free space in the pool.                                                  |
| `truenas_pool_used_bytes`             | `backend`, `pool`    | Used space (size minus free).                                            |
| `truenas_pool_healthy`                | `backend`, `pool`    | 1 when the appliance reports the pool healthy, 0 otherwise.              |
| `truenas_dataset_used_bytes`          | `backend`, `dataset` | Space used by a dataset this driver owns.                                |
| `truenas_dataset_quota_bytes`         | `backend`, `dataset` | Provisioned capacity: `refquota` for a filesystem, `volsize` for a zvol. |
| `truenas_iscsi_sessions`              | `backend`            | Open iSCSI sessions on the appliance.                                    |
| `truenas_collection_duration_seconds` | `backend`            | Duration of the last array metrics collection.                           |
| `truenas_collection_errors_total`     | `backend`            | Failed collections since the driver started.                             |

- **Only datasets this driver owns are exported** — the ZFS ownership marker must be
  present with `source == "LOCAL"`. The pool holds other people's data, which is neither
  ours to publish nor bounded in cardinality.
- **Polling is on an interval, never on the scrape path.** `metricsInterval` in the
  configuration sets it (a Go duration, default `60s`); a scrape is served from the last
  snapshot, so a slow appliance can never stall Prometheus.
- **A failed collection keeps the last known values** and increments
  `truenas_collection_errors_total`. Zeroing the gauges would turn a middleware outage
  into a false capacity alert. One unreachable appliance never stops the others.
- The collector runs on the **controller only** (the node plugin has no appliance client)
  and is skipped with a log line, not a crash loop, when no backend answers at startup.

## Data safety

The pool this driver was built against holds live, irreplaceable data, and that shaped the
design more than any other requirement:

- Every backend is **confined to one parent dataset**; a volume ID resolving outside it is
  rejected before any middleware call.
- Every dataset the driver creates is stamped with the ZFS user property
  `io.truenas.csi:managed`, and **nothing is deleted unless that property is present with
  `source == "LOCAL"`**. ZFS user properties are inherited by children, so presence alone
  is not proof of ownership.
- The driver keeps no database. Volume IDs are structured
  (`<backend>/<protocol>/<pool>/<dataset path>/<name>`) and every TrueNAS object name
  derives from them, so `DeleteVolume` is idempotent by construction and a controller
  restart mid-operation converges on the next retry.

## Documentation

- [docs/security.md](docs/security.md) — least-privilege TrueNAS roles, TLS trust, the
  accepted shared-target risk, and the data-safety model.
- [docs/troubleshooting.md](docs/troubleshooting.md) — failure modes and their signatures.

## Observability

Prometheus metrics on `metricsAddr` (default `:9090`) and health on `healthAddr` (default
`:9808`), plus a gRPC probe for the liveness sidecar. The exported series are
`truenas_csi_calls_total`, `truenas_csi_call_duration_seconds`,
`truenas_csi_middleware_calls_total`, `truenas_csi_middleware_duration_seconds`,
`truenas_csi_backend_up` and `truenas_csi_orphaned_volumes`. Label values are bounded to
method and backend names — a volume ID or a credential never becomes a label. Volume IDs
appear in log lines instead, so a failing volume can be traced end to end.

### Connectivity health monitoring

The node plugin polls the data path of every volume it has staged (every 10s, each probe
bounded to 3s) and the data address of every backend behind them. The probe runs under a
deadline in a goroutine of its own: the failure being detected — a hung NFS mount or an
iSCSI session whose portal is gone — is exactly the failure that makes `statfs(2)` never
return, so a monitor that waited for it would report nothing at all.

Two more series come out of it:

- `truenas_csi_volume_health{volume_id_hash,protocol}` — 1 when the volume's data path
  answered its last check, 0 when it did not. The label is the first 12 hex characters of
  the volume ID's SHA-256, never the ID itself: one series per PVC ever staged would grow
  without bound. The full ID is in the log line next to the transition.
- `truenas_csi_node_backend_reachable{backend}` — 1 when this node's bounded TCP probe of
  the appliance's data address succeeded. Distinct from `truenas_csi_backend_up`, which
  describes the controller's middleware websocket: a node can lose NFS while the API
  connection is perfectly healthy.

A transition is logged exactly once in each direction (`volume data path is unreachable`
at ERROR, `volume data path recovered` at INFO) — never once per poll. Unhealthy volumes
are also reported to Kubernetes through the CSI volume-health surface: the driver
advertises the `GET_VOLUME_HEALTH` node capability and answers `NodeGetVolumeHealth` with
an `INACCESSIBLE` condition and the underlying error. (CSI v1.13 replaced the earlier
alpha `volume_condition` field on `NodeGetVolumeStats` with this RPC; it is the same
signal.) The RPC answers from the monitor's recorded state, so it cannot hang on the mount
it is reporting about. Only volumes this driver staged are watched — the monitor never
reads `/proc/mounts`, so another storage system's mounts can never be reported as this
driver's.

### `ValidateVolumeHostConnectivity` — a driver extension, not CSI

Off by default; enable with `podmon.enabled=true`. The driver then serves one JSON-over-HTTP
route on a listener of its own (`127.0.0.1:9820` by default, or a UNIX socket via
`-podmon-addr=unix:///csi/podmon.sock`), alongside — never on — the CSI socket:

```
POST /podmon/v1/validate-volume-host-connectivity
{"nodeId": "worker-21", "volumeIds": ["nas1/nfs/tank/k8s/pvc-a"], "ioSampleWindow": 60000000000}

{"nodeId": "worker-21", "connected": true, "iosInProgress": true, "messages": []}
```

**The CSI specification defines no such call.** Dell's CSM for Resiliency defines one in its
own proto and its podmon sidecar calls it; this is the same question and the same shape of
answer, served as this driver's own extension with no generated stubs, so a sidecar, an
operator or `curl` can ask. No CO will ever call it.

The point of a second health checker is that it survives the first one stalling, so it
shares nothing that a stalled driver could hold: its own listener, its own `http.Server`,
its own bounded probes (a TCP dial to the appliance, a `statfs` per volume) on its own
deadlines, and never a call through the driver's middleware client or CSI socket. An
optional probe of the driver itself is advisory only — it runs concurrently under its own
timeout, and a driver that never answers costs one line in `messages` instead of the whole
response. `TestPodmonAnswersWhileDriverStalled` pins that with a driver fake that never
responds.

`iosInProgress` is evidence, not proof: it reports whether the volume's mount point was
modified within the sample window, so "no I/O observed" means no evidence of activity, not
a guarantee of idleness. Weigh it alongside `connected` rather than acting on it alone.

- [docs/replication.md](docs/replication.md) — StorageProtectionGroup: cross-appliance
  replication, failover, test failover and failback, their safety rules, and what has
  not been verified against real hardware.
