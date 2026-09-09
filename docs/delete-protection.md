# Delete protection

Delete protection puts a grace period between `kubectl delete pvc` and the
destruction of the data. It is **off by default**, and with it off `DeleteVolume`
takes exactly the code path it always has: the shares, extents and target
mappings come down and the dataset is destroyed.

## The trade you are accepting

> **Capacity is NOT reclaimed during the grace period.**

This is the feature, not a defect in it. Someone who deletes ten PVCs to free
space will find the pool exactly as full as it was, for the whole grace period.
The space is genuinely still allocated, so it keeps counting:

- against the pool headroom (`reservedBytes` / `reservedPercent`);
- against the capacity `GetCapacity` reports to the Kubernetes scheduler, which
  is how `CSIStorageCapacity` decides where a pod can be scheduled;
- in `truenas_pool_free_bytes` and in the per-dataset `truenas_dataset_used_bytes`
  series, where the graveyard's datasets appear under their own names.

All of that is correct behaviour. Size the graveyard into your pool headroom
before you turn this on: with a week's grace, a cluster that churns 200 GiB of
PVCs a week needs 200 GiB it will not get back.

The one exception is per-namespace accounting. A retired volume leaves its
namespace dataset, so the namespace's `zfs used` — and therefore its quota
headroom — drops immediately even though the pool's does not.

## Enabling it

Per appliance, in the chart's `backends` map:

```yaml
backends:
  nas1:
    endpoint: wss://truenas.example.com/api/current # wss:// only, never plaintext
    username: csi
    apiKey: "1-..."
    pool: Pool0
    parentDataset: k8s
    deleteProtection:
      enabled: true
      gracePeriod: 168h # one week
      graveyardDataset: ".trash" # optional; this is the default
      reapInterval: 1h # optional; this is the default
```

Rules the driver enforces at startup:

- `enabled: true` with no positive `gracePeriod` is refused. A zero grace period
  destroys the volume immediately, which is what leaving the feature off already
  does, and a graveyard whose contents are instantly reapable would only be a
  slower way to lose the same data.
- A `gracePeriod` written with `enabled` left `false` is refused rather than
  ignored, so nobody ends up with no protection and a configuration that reads
  as if there were a week of it.
- `deleteProtection` is refused on `flavour: core`. Retiring renames a dataset,
  and the CORE REST mapping for `pool.dataset.rename` in this driver has never
  been exercised against a CORE appliance.
- Changing any of this needs a **driver restart**; the credential hot-reload
  path refuses it. The reaper is started once, from the configuration the
  controller booted with, so adopting a shortened grace period live would make
  datasets reapable that the earlier configuration had promised to keep.

## What happens on delete

`DeleteVolume` with protection on:

1. Removes the NFS export / SMB share / iSCSI extent and target mapping / NVMe-oF
   namespace and subsystem — **unchanged**, and always first. The middleware
   documents `pool.dataset.rename` as performing _no safety checks_: renaming a
   dataset still in use by SMB, iSCSI, snapshot tasks or replication "may cause
   disruptions or service failures". The driver never passes `force`, so a
   rename the appliance refuses is a signal that teardown did not finish, and it
   is allowed to fail the delete rather than be overridden.
2. Creates `<pool>/<parentDataset>/<graveyardDataset>` if it is missing, marked
   with `io.truenas.csi:managed` and `io.truenas.csi:graveyard`.
3. Renames the volume's dataset to
   `<pool>/<parentDataset>/<graveyardDataset>/<YYYYMMDDThhmmssZ>-<volume>`.
4. Stamps `io.truenas.csi:deleted-at` (RFC 3339 UTC) and
   `io.truenas.csi:retired-from` (the original CSI volume handle) on it.
5. Returns success, and logs a line saying that the space is not reclaimed.

The ownership marker is not re-stamped: a rename carries a dataset's user
properties with it.

### The graveyard layout

```
Pool0/k8s/                                        parentDataset
Pool0/k8s/pvc-9d1c…                               a live volume
Pool0/k8s/team-a/pvc-4f22…                        a live volume (namespaced layout)
Pool0/k8s/.trash                                  the graveyard
Pool0/k8s/.trash/20260908T101500Z-pvc-9d1c…       retired
Pool0/k8s/.trash/20260901T084102Z-team-a-pvc-4f22… retired
```

The graveyard is always flat — one level, no namespace subdirectories — because
the reaper destroys recursively and a nested entry would take its siblings with
it.

The default name `.trash` begins with a dot deliberately, and that was checked
rather than assumed. OpenZFS's `entity_namecheck()` accepts any component made
of `[A-Za-z0-9_.: -]` and rejects only `.` and `..` as whole components; the
"must begin with a letter" rule applies to _pool_ names alone, and TrueNAS
relies on this itself for `<pool>/.system`. The dot is what makes a collision
structurally impossible: a Kubernetes namespace is a DNS-1123 label and a
PersistentVolume name a DNS-1123 subdomain, and neither may begin with one.

The entry name is a convenience for reading `zfs list`; the identity that
matters is in `io.truenas.csi:retired-from`.

### What still resolves, and what does not

- The volume handle **stops resolving** the moment `DeleteVolume` returns, which
  is what CSI requires. `ControllerExpandVolume`, `ControllerPublishVolume` and
  `ValidateVolumeCapabilities` all answer `NotFound`.
- A **retried `DeleteVolume`** succeeds, and does not produce a second graveyard
  entry.
- A **`CreateVolume` with the same name** afterwards makes a fresh, empty volume.
  Nothing resurrects the retired dataset; the two coexist until the reaper takes
  the old one.
- `ListVolumes` and the orphan report skip the graveyard and everything in it,
  recognising them by their local `io.truenas.csi:graveyard` and
  `io.truenas.csi:deleted-at` markers rather than by the configured graveyard
  name — so leftovers stay recognised after the feature is turned off or the
  graveyard renamed.
- A retired volume's **snapshots travel with it** (ZFS renames a dataset's
  snapshots along with the dataset) and are destroyed with it at the end of the
  grace period, exactly as they are destroyed with the volume today. Their
  snapshot handles stop resolving, and `ListSnapshots` omits them.

## The reaper

The controller runs a reaper that destroys expired graveyard datasets. It is a
separate component from the orphan reconciler in `internal/reconcile`, which is
report-only on purpose and stays that way: an apparent orphan is more often a
stale PersistentVolume listing than a leak, and a reconciler with a destroy path
would be one flag away from acting on that judgement.

Before **every** destroy, the reaper re-checks **four** preconditions against the
dataset the appliance just returned. All four, every time:

1. **Inside the graveyard** — a direct child of
   `<pool>/<parentDataset>/<graveyardDataset>`, and not the graveyard itself.
   Prefix _and_ depth, so a lookalike sibling (`.trash-old`) cannot match and a
   nested dataset cannot be destroyed along with its siblings.
2. **Driver-owned** — `io.truenas.csi:managed = truenas-csi` with **source
   `LOCAL`**. ZFS user properties are inherited, so the graveyard's own marker
   reaches everything beneath it; a presence-only check would clear a dataset
   somebody dropped in there by hand.
3. **Carries a deletion timestamp** — a local, parsable `io.truenas.csi:deleted-at`.
   A value the driver cannot read is never treated as old.
4. **Past its grace period** — `now - deletedAt >= gracePeriod`. A timestamp in
   the future reads as "not yet".

A dataset that fails any of them is logged at debug level and left alone,
for ever if need be. The reaper runs on every controller replica rather than
behind a lease: two replicas destroying the same expired dataset is harmless
(the middleware answers a missing dataset with `null`, not an error), and a
lease would add a way for the reaper to silently never run.

### Clones

A retired dataset that a live volume was **cloned from** cannot be destroyed —
ZFS refuses with `EBUSY` while a snapshot of it has dependent clones. The reaper
logs it and leaves it, and destroys it on a later sweep once the clone is gone.

It never promotes the clone. `pool.dataset.promote` does not free the origin, it
**inverts** the dependency: the live volume would end up depending on a dataset
that is queued for destruction. That is the same reason `DeleteSnapshot` returns
`FailedPrecondition` instead of promoting, recorded in
`.procoder/notes/truenas-api-findings.md`.

One consequence worth knowing: with protection on, deleting a PVC that is a
clone origin **succeeds** where it could previously fail with `EBUSY`, because a
rename has no such restriction. The dependency is simply resolved later.

## Recovering a volume before the grace period expires

There is no API for this — deliberately, since an "undelete" that the CO does
not know about would produce a dataset with no PersistentVolume. Recover by
hand on the appliance:

```sh
# Find it: the property says which PVC it was.
zfs get -r io.truenas.csi:retired-from Pool0/k8s/.trash

# Move it back to the path its old handle names, and clear the retirement marks.
zfs rename Pool0/k8s/.trash/20260908T101500Z-pvc-9d1c… Pool0/k8s/pvc-9d1c…
zfs inherit io.truenas.csi:deleted-at   Pool0/k8s/pvc-9d1c…
zfs inherit io.truenas.csi:retired-from Pool0/k8s/pvc-9d1c…
```

Then re-create the PersistentVolume with the original `volumeHandle`; the driver
will re-create the share on the next publish. Clear `io.truenas.csi:deleted-at`
first — while it is set, `ListVolumes` will not report the volume.

## Turning it off again

Turning `deleteProtection.enabled` back to `false` restores today's behaviour for
new deletes, but **leaves whatever is already in the graveyard**: the reaper only
runs while the feature is on. Either leave it on until the graveyard drains, or
destroy the leftovers by hand:

```sh
zfs list -r Pool0/k8s/.trash
zfs destroy -r Pool0/k8s/.trash/20260908T101500Z-pvc-9d1c…
```

## Not verified without hardware

- The rename itself has been exercised only against the driver's fake. The call
  shape (`pool.dataset.rename(id, {new_name, force})`, returning `null`) is taken
  from the appliance's own method documentation at
  `https://192.168.10.253/api/docs/current/api_methods_pool.dataset.rename.html`.
- Whether the middleware refuses a rename while a zvol's device is still held
  after extent removal. The driver retries on `EBUSY` for 30 seconds, matching
  what the delete path already does for the same observed window.
- Whether TrueNAS's own validation accepts a dot-prefixed dataset name through
  `pool.dataset.create`. ZFS does, and TrueNAS creates `<pool>/.system` itself,
  but that specific call has not been made against the box. `graveyardDataset`
  is configurable if it turns out otherwise.
