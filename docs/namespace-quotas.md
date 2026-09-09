# Per-namespace capacity accounting

Optional, off by default. When enabled for a backend, a Kubernetes namespace
gets its own dataset under the backend's `parentDataset`, that dataset carries a
ZFS `quota`, and every volume for the namespace is provisioned beneath it.

```
Pool0/k8s/pvc-1                 # flat layout (the default, and every existing volume)
Pool0/k8s/team-a                # namespace dataset, quota=500G
Pool0/k8s/team-a/pvc-2          # a volume in namespace team-a
```

Implemented in `internal/backend/namespace.go` (appliance mechanics),
`internal/csi/capacity.go` (`requireRoomInNamespaceQuota`) and
`internal/config/namespacequota.go` (configuration).

## Why a dataset quota rather than a ledger

Dell's CSM Authorization solves the same problem with an L7 proxy in front of
the array's REST API, an OPA policy engine, a Redis quota ledger, JWTs and
Vault, because — in Dell's own words — "the storage array does not natively
support RBAC and/or multi-tenancy". ZFS does have the mechanism those arrays
lack. A dataset quota is enforced by the appliance, so it holds against bytes
the driver never saw: a pod filling a volume, a snapshot growing, an operator
copying data in over SSH. A ledger only counts provisioning requests that went
through the driver, and is wrong the moment anything else writes.

This is the useful 80% of CSM Authorization. It is capacity only — no IOPS, no
bandwidth — which is also all CSM Authorization enforces.

## Configuring it

Per backend, in the driver configuration (Helm `backends.<name>.namespaceQuotas`):

```yaml
namespaceQuotas:
  enabled: true
  defaultBytes: 107374182400 # 100 GiB for any namespace with no entry below
  perNamespace:
    team-a: 536870912000 # 500 GiB
    kube-system: 0 # explicitly unlimited
```

The quota is **static operator configuration** and is deliberately not read from
a Kubernetes object. A namespace annotation or a `ResourceQuota` would be more
flexible, but both are writable by whoever holds edit rights in the namespace
being limited, which makes the limit self-service and therefore not a limit.
Honouring one safely would need a watch, RBAC on namespaces, a cache and an
authorisation story. Pool headroom (`reservedBytes` / `reservedPercent`) is
already configured the same way, for the same reason.

`0` means unlimited, not banned: the namespace still gets its own dataset, so
`zfs list` and the TrueNAS UI show what it consumes, but nothing is refused.

A change here **requires a driver restart**. The hot-reload path refuses it
(`namespaceQuotas` is in the restart-required set alongside `pool` and
`parentDataset`), because the controller captured the layout at startup and
adopting a change mid-flight would put two volumes of one namespace in two
different places.

## What an operator loses by enabling it

1. **Volumes created before it was enabled are not counted.** They live directly
   under `parentDataset`, their volume handles are immutable, and nothing can
   move a dataset a `PersistentVolume` already points at. Their space is real
   but invisible to the namespace quota, so a namespace's true consumption is
   its quota plus whatever it held beforehand. To fold them in, migrate: create
   a new PVC in the namespace, copy, delete the old one.
2. **The scheduler is still told pool capacity, not namespace capacity.**
   `GetCapacityRequest` carries the StorageClass parameters and the topology; it
   does not carry a namespace, and the external-provisioner publishes one
   `CSIStorageCapacity` per StorageClass and topology segment, read by every
   namespace. There is no per-namespace answer to give. The consequence is
   visible: a PVC over its namespace quota **binds and then fails provisioning
   with `ResourceExhausted`**, rather than staying `Pending`.
3. **Volume group snapshots cannot span namespaces.** The crash-consistent group
   snapshot is one recursive `pool.snapshot.create` and needs a common parent
   dataset; volumes in two namespaces no longer have one.
4. **One extra middleware round trip per `CreateVolume`** in a namespaced
   backend, to query and reconcile the namespace dataset. Flat volumes are
   unaffected and cost nothing.

Disabling it again is safe for existing volumes — their handles keep resolving —
but new volumes go back to the flat layout and stop being counted.

## Volume handles

A handle gains one component when, and only when, the volume is namespaced:

```
nas1/nfs/Pool0/k8s/pvc-1            flat        -> Pool0/k8s/pvc-1
nas1/nfs/Pool0/k8s/team-a/pvc-2     namespaced  -> Pool0/k8s/team-a/pvc-2
```

The component count alone decides which shape a handle is, and the two shapes
produce dataset paths of different depths, so no two distinct handles can name
the same dataset and no handle minted under the old layout changes meaning.
Handles are never escaped: a namespace containing a path separator is refused,
not encoded.

The namespace only enters a handle when the request actually carries one. It is
absent — and the volume is provisioned flat — when the feature is off, when
`CreateVolume` was given no PVC metadata at all (csi-sanity, a static
provisioner, an external-provisioner without `--extra-create-metadata`), or when
the namespace in the parameters is not a DNS-1123 label. That last case falls
back rather than failing: the parameter map is untrusted and anyone with
StorageClass edit rights can write those keys, so refusing would hand them a way
to break provisioning, while provisioning flat costs the accounting for one
volume.

## Enforcement

Two layers, and the second is the real one:

1. `CreateVolume` refuses with `ResourceExhausted` when
   `max(used, provisioned) + requested` exceeds the namespace's configured
   quota. This is what turns a middleware `EDQUOT` into a clear error, and it is
   what bounds **thin** volumes, whose provisioned size ZFS does not charge
   against the quota until the data is written.
2. The ZFS `quota` on the namespace dataset. This is the one that holds against
   writes the driver never saw.

`used` is ZFS's own figure for the dataset and everything beneath it: volumes,
their snapshots and every byte written into them. `provisioned` is what the
namespace's volumes were promised — `refquota` for a filesystem, `volsize` for a
zvol — counting only this driver's own direct children of the namespace dataset.

The two are measured together and the LARGER one binds, because neither alone is
enough. Usage alone cannot bound thin volumes: a ZFS quota charges nothing for a
1 GiB `refquota` until a byte is written into it, so a namespace with a 2 GiB
quota would bind an unlimited number of 1 GiB claims and the tenant would meet
the limit as write failures spreading across workloads that were already
running. Provisioned alone would miss every byte that did not arrive through
this driver — a restored replication stream, a snapshot growing, an operator
copying data in over SSH — which is what a ledger gets wrong and a ZFS quota
gets right.

Over-provisioning is therefore **not** permitted: the sum of a namespace's
volume sizes cannot exceed its quota, the way `requests.storage` behaves in a
Kubernetes `ResourceQuota`.

## Lowering a quota below current usage

ZFS accepts a quota below current usage — existing data survives, but every
further write into the dataset fails with `EDQUOT`, including writes from pods
that were running happily under the old limit. That turns an accounting change
into an outage.

So the driver **does not apply it**. It leaves the existing ZFS quota in place,
logs a warning, and refuses new volumes in that namespace (because
`requireRoomInNamespaceQuota` measures against the _configured_ quota, not the
applied one). The new ceiling therefore binds immediately for new allocations,
which is what the operator asked for, without failing writes for workloads that
are already there. The `ResourceExhausted` message says so explicitly. Once
usage falls back below the new figure — a PVC is deleted, a snapshot expires —
the next `CreateVolume` applies the quota to ZFS for real.

Raising a quota is applied immediately; there is nothing to protect against.

## Reclamation

`DeleteVolume` tries to remove the namespace dataset once its last volume has
gone. Every refusal is deliberate:

- the dataset is already gone — nothing to do;
- it is not driver-owned (`io.truenas.csi:managed` set **LOCAL**, never merely
  inherited), or carries no `io.truenas.csi:namespace` marker — it is somebody
  else's dataset that happens to sit at this path, and the driver does not touch
  it;
- it still has children — a volume, or a dataset a human put there.

The delete is **non-recursive and non-forced**. If ZFS says the dataset is not
empty, that is a fact to respect, not to override: a forced recursive delete of
a namespace dataset would destroy every volume inside it.

Reclamation is best-effort and never fails the `DeleteVolume` that triggered it.
The volume the CO asked about is already gone, and reporting an error would make
the CO retry a delete that succeeded. A leftover dataset is visible in the
TrueNAS UI and costs nothing; a stuck `DeleteVolume` blocks the
`PersistentVolume` from being released.

## Properties on a namespace dataset

| Property                   | Meaning                                                                                                                                                                                          |
| -------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `io.truenas.csi:managed`   | the usual ownership marker; the reclamation guard requires it with source `LOCAL`                                                                                                                |
| `io.truenas.csi:namespace` | the Kubernetes namespace this dataset accounts for; its presence with source `LOCAL` is what marks the dataset as a container rather than a volume                                               |
| `io.truenas.csi:nsquota`   | the quota, in bytes, the driver last applied — the driver cannot read `quota` back through its dataset view, and recording what it set is what lets an unchanged quota cost a query and no write |

`ListVolumes` and the orphan reconciler both skip datasets carrying the
namespace marker: a namespace dataset is not a volume, has no
`PersistentVolume` by design, and handing the CO a handle for one would put
`DeleteVolume` on a dataset full of other people's volumes.

## Verification status

The ZFS `quota` property and its semantics (applies to the dataset and all
descendants; may be set below current usage; blocks further writes) are ZFS
behaviour, not middleware behaviour.

`UNVERIFIED:` `pool.dataset.update` accepting `quota: 0` to clear the property
is taken from the middleware's published dataset schema at
`https://192.168.10.253/api/docs/current/` and has not been exercised against
hardware. The set path (a positive byte count) matches how `refquota` is already
set by the volume backends, which is hardware-verified.
