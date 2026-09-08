# Volume attributes — changing a live volume's ZFS properties

A `VolumeAttributesClass` names ZFS properties. Putting its name in a
PersistentVolumeClaim's `volumeAttributesClassName` applies them to that **one**
volume: no recreation, no unmount, no downtime. Changing the name applies a
different set; the driver's `ControllerModifyVolume` is idempotent, so
re-applying the same class does nothing at all.

This is the CSI `MODIFY_VOLUME` capability, and it is worth having here because
ZFS genuinely has properties that matter on a volume that is already in use.

## Why: the number this exists for

From [docs/performance.md](performance.md), measured through this driver on the
reference pool (12×6TB RAIDZ2, no SLOG):

| Protocol | 4 KiB random write IOPS | write latency |
| -------- | ----------------------: | ------------: |
| iSCSI    |                  45,155 |       0.71 ms |
| NFS      |                 **337** |   **94.6 ms** |

Almost all of that gap is ZFS honouring synchronous writes on spinning disks
with no separate log device. `sync` is a per-dataset property and it can be
changed on a mounted, in-use volume. So an operator can make that trade on one
claim — and take it back afterwards.

```yaml
apiVersion: storage.k8s.io/v1beta1
kind: VolumeAttributesClass
metadata:
  name: truenas-fast-writes
driverName: csi.truenas.watteel.com
parameters:
  sync: disabled
  compression: lz4
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: build-cache
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: truenas-nfs
  volumeAttributesClassName: truenas-fast-writes
  resources:
    requests:
      storage: 100Gi
```

## `sync: disabled` — read this before using it

`sync=disabled` tells ZFS to acknowledge a write **before it has reached stable
storage**. That is the whole speed-up, and it is not free:

> A power failure, kernel panic or appliance crash can lose writes the
> application was already told had succeeded — roughly the last five seconds of
> them. The pool and the filesystem stay consistent; the acknowledged data does
> not.

The driver **does not refuse it**. An informed operator is entitled to make that
trade. It does log a `WARNING` naming the volume every single time the class is
applied — including on a re-apply that changes nothing — so the choice is never
silent:

```
level=WARN msg="sync=disabled requested for volume: writes will be acknowledged
before they reach stable storage, so a power failure or appliance crash can lose
data this volume has already told the application was written"
volume_id=nas1/nfs/Pool0/k8s/pvc-abc dataset=Pool0/k8s/pvc-abc
```

Reasonable uses: build caches, scratch space, CI workspaces, a replica that
resyncs from a leader, anything whose contents can be rebuilt. Never the only
copy of anything.

Going back is one field change — `volumeAttributesClassName: truenas-durable`
with `sync: always` (or `sync: inherit` to fall back to the parent dataset's
setting). It takes effect on the next write.

## What the driver will change

A closed set, and nothing else. Every entry can be changed while the volume is
mounted, affects **future writes only**, and says nothing about capacity.

| Attribute     | Values                                                                                                                           | Applies to          |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------- | ------------------- |
| `sync`        | `standard`, `always`, `disabled`, `inherit`                                                                                      | filesystem and zvol |
| `compression` | `off`, `on`, `lz4`, `gzip`, `gzip-1`, `gzip-9`, `zstd`, `zstd-1`…`zstd-19`, `zstd-fast`, `zstd-fast-N`, `zle`, `lzjb`, `inherit` | filesystem and zvol |
| `atime`       | `on`, `off`, `inherit`                                                                                                           | **filesystem only** |
| `recordsize`  | `512`, `1K`…`1M` (powers of two), `inherit`                                                                                      | **filesystem only** |

Values may be written in ZFS's own lowercase spelling; the driver normalises
them to the uppercase enum the TrueNAS middleware demands. `inherit` clears the
property so the volume follows its parent dataset again — the only way to undo a
change without knowing what the site default was.

### Filesystem versus zvol

`nfs` and `smb` volumes are ZFS filesystems. `iscsi` and `nvme` volumes are
zvols: one block device, with no filenames inside it as far as ZFS is concerned.
`atime` and `recordsize` describe a filename namespace, so they do not exist on
a zvol — ZFS refuses them there, and so does this driver, with `InvalidArgument`
naming the attribute rather than letting the appliance produce an error the CO
would retry forever. The zvol analogue of `recordsize` is `volblocksize`, which
cannot be changed after the volume is created at all.

## What the driver refuses, and why

Anything not in the table above is rejected with `InvalidArgument` naming the
key. This is deliberate and it is the security boundary of the feature: a
`VolumeAttributesClass` is a cluster-scoped object whose parameters are chosen
by whoever may create one — not necessarily the people who own the pool.

- **`quota`, `refquota`, `volsize`, `reservation`, `refreservation`** — capacity
  belongs to `ControllerExpandVolume` alone. Two code paths writing a volume's
  size will eventually disagree, and the one that loses silently resizes a volume
  the orchestrator believes is a different size.
- **`volblocksize`** — immutable after a zvol exists.
- **`readonly`** — would break every pod writing to the volume, with no way for
  the pod to learn about it except an I/O error.
- **`mountpoint`** — would detach the data from the share serving it.
- **`deduplication`** — a pool-wide memory commitment that one claim must not be
  able to make on the operator's behalf.
- **`user_properties`, `io.truenas.csi:managed`** — the ownership marker. This
  driver refuses to touch any dataset that does not carry it with source `LOCAL`,
  and that guard must not be writable through the same door it protects.
- **`primarycache`, `logbias`** — genuinely good candidates on paper, and absent
  for a duller reason: TrueNAS 25.10's `pool.dataset.update` does not accept
  them. Its schema declares no additional properties and lists neither, so
  allowlisting them would only produce guaranteed appliance rejections.

Ownership is checked before anything is written: a volume whose dataset does not
carry `io.truenas.csi:managed` with source `LOCAL` is never modified, on the same
rule every destructive path in this driver uses
(`internal/volume/ownership.go`).

## Error codes

The code decides whether the orchestrator retries, so they are chosen carefully:

| Condition                                                   | Code                 |
| ----------------------------------------------------------- | -------------------- |
| attribute not on the allowlist, or a value outside its enum | `InvalidArgument`    |
| a filesystem-only attribute on a zvol                       | `InvalidArgument`    |
| the volume does not exist                                   | `NotFound`           |
| the dataset is not managed by this driver                   | `FailedPrecondition` |
| another operation is in progress on the volume              | `Aborted`            |
| the driver accepted it and the appliance refused it         | `Internal`           |

The last row is the one that matters most: a property this driver understands,
refused by the appliance, is a condition on the appliance — the class is still
valid and will apply once the condition clears. Reporting it as
`InvalidArgument` would make the orchestrator give up on a change that is going
to work.

## Enabling it

Off by default, in three places that must be turned on together.

1. **Kubernetes.** 1.31 or newer, with the `VolumeAttributesClass` feature gate
   enabled on the API server, the scheduler and the controller manager, and
   `storage.k8s.io/v1beta1` served (`--runtime-config=storage.k8s.io/v1beta1=true`).
   The API graduates to `storage.k8s.io/v1` in 1.34.
2. **Sidecars.** `csi-provisioner` ≥ v5.0 (it carries the class named on a claim
   into `CreateVolume`) and `csi-resizer` ≥ v1.11 (it watches a live claim and
   calls `ControllerModifyVolume`). Both are already pinned above those in the
   chart.
3. **The chart.**

```console
helm upgrade truenas-csi ... \
  --set volumeAttributesClass.enabled=true \
  --set volumeAttributesClass.classes.fastWrites.enabled=true
```

That flag adds the sidecars' feature gate and a **read-only** RBAC grant on
`volumeattributesclasses`. Nothing in this driver creates or edits a class: the
set of properties it will change is compiled in, not configured.

The chart ships three example classes, all disabled — `truenas-fast-writes`
(the trade above), `truenas-durable` (the way back), and `truenas-archive`
(`zstd-9`, `atime=off`, `recordsize=1M` for large sequential files). They are
disabled for the same reason the example StorageClasses are, and one stronger:
a class that exists by default is a class somebody attaches to a claim without
reading what it costs.

Without step 1 or step 2 a class attached to a claim is **inert** — nothing
errors, nothing changes, and there is no event to explain it. That is the
failure mode to check for first.

## Limits

- A class applies to one volume. There is no bulk apply; changing many claims
  means changing many claims.
- Kubernetes applies a class asynchronously and reports progress on the claim's
  `status.currentVolumeAttributesClassName` and
  `status.modifyVolumeStatus`. `kubectl describe pvc` is where a rejected class
  shows up.
- The properties affect writes made **after** they are applied. Existing blocks
  keep the compression they were written with, and data already on the volume is
  not rewritten. Nothing here is a migration.
