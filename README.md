# truenas-csi

A Container Storage Interface driver for **TrueNAS SCALE 25.10 and newer**. It provisions
ZFS-backed PersistentVolumes on a TrueNAS appliance and speaks the API TrueNAS actually
supports today: **JSON-RPC 2.0 over a `wss://` websocket** at `/api/current`. There is no
REST client and no dual-transport abstraction — the legacy REST API is deprecated upstream.

Driver name: `csi.truenas.watteel.com` (immutable once PersistentVolumes exist — changing
it orphans every PV). Go module: `github.com/pwatteel/truenas-csi`. Licence: Apache 2.0.

**Protocols in v1: NFS and iSCSI.** SMB and NVMe/TCP were both validated end to end
against a live appliance and against real cluster nodes, and the backend abstraction is
shaped to accept them, but neither ships in v1. **NVMe over RDMA/RoCE is out of scope**:
the target appliance reports `nvmet.global.rdma = false` and the arm64 nodes have no
RDMA-capable NICs, so it cannot be validated at all.

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

## Requirements

| Component    | Requirement                                                                               |
| ------------ | ----------------------------------------------------------------------------------------- |
| Kubernetes   | 1.31 or newer (validated on k3s 1.34)                                                     |
| TrueNAS      | SCALE 25.10 or newer. TrueNAS CORE is not supported                                       |
| Transport    | `wss://` to the appliance's `/api/current` endpoint                                       |
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

| Capability                  | Debian/Ubuntu package | Needs                                            |
| --------------------------- | --------------------- | ------------------------------------------------ |
| iSCSI                       | `open-iscsi`          | `iscsiadm`, `iscsid`, module `iscsi_tcp`         |
| NFS                         | `nfs-common`          | `mount.nfs`                                      |
| ext4                        | `e2fsprogs`           | `mkfs.ext4`, `resize2fs`                         |
| XFS                         | `xfsprogs`            | `mkfs.xfs`, `xfs_growfs`                         |
| multipath                   | `multipath-tools`     | `multipath`, `multipathd`, module `dm_multipath` |
| SMB (deferred protocol)     | `cifs-utils`          | `mount.cifs`, module `cifs`                      |
| NVMe-oF (deferred protocol) | `nvme-cli`            | `nvme`, module `nvme_tcp`                        |

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

## StorageClass parameters

A parameter left empty means "use the default"; that is not an error.

| Parameter       | Applies to | Values                                                              | Default                                                                |
| --------------- | ---------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| `backend`       | all        | a name from the chart's `backends` map                              | required, unless exactly one backend is configured                     |
| `protocol`      | all        | `nfs`, `iscsi`                                                      | required                                                               |
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

## Capabilities

Controller: `CREATE_DELETE_VOLUME`, `PUBLISH_UNPUBLISH_VOLUME`, `CREATE_DELETE_SNAPSHOT`,
`LIST_SNAPSHOTS`, `LIST_VOLUMES`, `EXPAND_VOLUME`, `CLONE_VOLUME`, `GET_CAPACITY`.
Node: `STAGE_UNSTAGE_VOLUME`, `EXPAND_VOLUME`, `GET_VOLUME_STATS`.

- **Snapshots and restore.** `CreateSnapshot`/`DeleteSnapshot`/`ListSnapshots`, and
  `CreateVolume` from a snapshot source. A restored volume is a ZFS clone that stays
  dependent on its snapshot, so `DeleteSnapshot` returns `FAILED_PRECONDITION` while a
  restored volume still depends on it — as the CSI spec prescribes. Deleting the PVC is
  never blocked by a clone. Cloning directly from a volume is not supported; snapshot it
  first.
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
