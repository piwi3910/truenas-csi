# TrueNAS middleware findings (live box, 25.10.6 Community Edition)

Box: 192.168.10.253. **Use TLS for everything.** An early version of this note
read "HTTP only (no TLS configured)" and that line is what cost three API keys:
the appliance serves a self-signed certificate rather than no certificate, so
`wss://` and `https://` both work and plaintext is never necessary. Verified
2026-09-08 — the API, and the docs page below, answer over TLS.

Docs: https://192.168.10.253/api/docs/current/ — Sphinx, one page per method,
and served **unauthenticated**, so method signatures can be checked without
presenting a key at all.

Admin user: `truenas_admin`. An API key is bound to a user; the wrong username
returns `response_type=AUTH_ERR` even from a valid key.

## API surface (881 documented JSON-RPC methods)

Everything the driver needs exists:

- Datasets/zvols: `pool.dataset.create|update|delete|query|get_instance`,
  `pool.dataset.recommended_zvol_blocksize`, `pool.dataset.set_quota`, `pool.dataset.promote`
- Snapshots/clones: `pool.snapshot.create|clone|delete|rollback|hold|release`
- iSCSI: `iscsi.extent`, `iscsi.target`, `iscsi.targetextent`, `iscsi.portal`,
  `iscsi.initiator`, `iscsi.auth` (CHAP), `iscsi.global.alua_enabled`
- NFS/SMB: `sharing.nfs.*`, `sharing.smb.*`
- NVMe-oF: `nvmet.subsys`, `nvmet.namespace`, `nvmet.port`, `nvmet.host`,
  `nvmet.host_subsys`, `nvmet.port_subsys`, DH-CHAP via `nvmet.host.generate_key`
- Jobs: `core.get_jobs`, `core.job_wait`, `core.job_abort`, `core.subscribe`

## Protocol constraints that shape the client

- Error `-32000` = "too many concurrent calls" -> client needs a bounded in-flight semaphore.
- Error `-32001` = "method call error"; the real cause is in `data.errname` / `data.error`
  (e.g. errno 207 = ENOTAUTHENTICATED). This is the mapping source for gRPC status codes.
- Long-running jobs report progress via `collection_update` notifications carrying
  `state`, `progress`, `result` -> subscribe, do not poll.

## Schema constraints found so far

- `iscsi.extent.create`: `name` max 64 chars; `type` DISK|FILE; for DISK the `disk` field
  takes the zvol; `blocksize` one of 512/1024/2048/4096. Constrains the volume naming scheme.

## RESOLVED: why API keys were being revoked

TrueNAS error: **"API key revoked due to insecure transport."**

25.10 revokes an API key the moment it is presented over a plaintext connection.
The docs warn that `_PLAIN` auth mechanisms "should not be used on untrusted / insecure
transport" — middleware enforces this by REVOKING the key, not by refusing the login.
Three keys (5, 6, 7) were burned this way by connecting over the plaintext
WebSocket scheme instead of `wss://`.

### Hard requirements this creates for the driver

1. **TLS is mandatory.** The client MUST use `wss://` and MUST refuse to start if
   configured with a plaintext URL. A plaintext connection does not merely fail — it
   destroys the credential, taking down every volume operation until an operator
   issues a new key by hand. Guard this at config-validation time, with a test.
2. **Authentication failure is terminal.** Never retry a failed login; log and fail
   fatally. Only _connection_ failures may be retried with backoff.

## TLS trust: the stock certificate is unusable for verification

Default cert on this box (and on any fresh TrueNAS install):

    subject/issuer: C=US, O=iXsystems, CN=localhost   (self-signed)
    SAN:            DNS:localhost
    validity:       2025-12-07 .. 2027-01-08

Verification fails twice over: self-signed chain, and a SAN that cannot match the
address the driver connects to. So the driver needs an explicit trust configuration —
this cannot be left to system roots.

## Live environment (verified over wss://, 2026-09-07)

TrueNAS 25.10.6, hostname `truenas`, 12 cores, 125 GiB RAM.

**Pool0** — 72.0 TB raw, 44.9 TB free, ONLINE/healthy. Single pool.
19 existing datasets holding **real production data**: `Home` (6.11 TiB),
`old_homes` (4.98 TiB), `Multimedia` (3.46 TiB), `Backup/timemachine` (1.18 TiB),
`VMs` (incl. a 1 TiB zvol), `Frigate`, `Hass_Backup`, `Software`.

**iSCSI: completely unconfigured** — 0 portals, 0 targets, 0 extents, 0 initiators.
Global config: `basename=iqn.2005-10.org.freenas.ctl`, `listen_port=3260`,
`alua=false`, `iser=false`.

**NFS: 3 existing shares.** Useful precedent — `/mnt/Pool0/Backup/longhorn-kw`
("k3s Longhorn backups") uses `networks=["192.168.10.0/24"]`,
`maproot_user=root`, `maproot_group=wheel`. That is the shape our NFS shares need.

**NVMe-oF: unconfigured** — 0 ports, 0 subsystems.
Global: `basenqn=nqn.2011-06.com.truenas:uuid:6ab80cc6-...`, `kernel=true`,
`ana=false`, `rdma=false`.

**Network: a single address**, `192.168.10.253`. `iscsi.portal.listen_ip_choices`
returns only `0.0.0.0`, `::` and that one IP.

### Consequences

1. **SAFETY — the pool holds live data.** The driver must be hard-scoped to a
   configured parent dataset, must refuse any operation resolving outside it, and
   must never delete a dataset it did not create. Mark ownership with a ZFS user
   property (e.g. `io.truenas.csi:managed`) set at creation and verified before every
   destructive call. A bug here destroys 20 TiB of the user's data.
2. **Multipath cannot be validated here.** One portal IP, ALUA off. Multipath needs
   multiple portal addresses. The code can be written, but the target environment
   gives it a single path — treat "multipath support" as implemented-but-unvalidated
   unless more NICs/VLANs appear.
3. **NVMe/RoCE confirmed unavailable** — `nvmet.global.rdma=false`, and the k3s nodes
   (RK3588, 2.5GbE) have no RDMA hardware. NVMe/TCP is the only viable transport.
4. **iSCSI starts from nothing.** No portal exists, so the driver (or its docs) must
   decide whether it creates the portal/initiator group or requires them pre-made.
   Creating shared, cluster-wide objects on the fly is a reconciliation problem;
   requiring them is simpler and safer.
5. **Least privilege.** The probe key carries FULL_ADMIN — far more than needed.
   The driver should ship a documented minimal role set:
   `DATASET_READ/WRITE/DELETE`, `SNAPSHOT_READ/WRITE/DELETE`, `POOL_READ`,
   `SHARING_ISCSI_*`, `SHARING_NFS_READ/WRITE`, `FILESYSTEM_ATTRS_WRITE`.

## Verified lifecycle (live, wss, scratch dataset created and fully removed)

All of it works. Pool0 returned to its original state; 0 leftover objects.

### Ownership marker — CONFIRMED, with a correction

`pool.dataset.create` accepts `user_properties: [{"key": "io.truenas.csi:managed", "value": ...}]`
and `pool.dataset.query` returns it with a `source` field. An unmarked dataset is cleanly
distinguishable.

**Correction that matters:** ZFS user properties are INHERITED by children. If the
operator-configured parent dataset ever carried the marker, every pre-existing dataset
beneath it would inherit it and appear driver-owned. The delete guard must therefore
require `source == "LOCAL"`, not merely that the property is present.

### Block path

- `pool.dataset.recommended_zvol_blocksize("Pool0")` -> `128K`. Use it, don't hardcode.
- Create zvol: `pool.dataset.create {type: VOLUME, volsize, sparse: true, volblocksize}`
- Expand: `pool.dataset.update <id> {volsize: N}` — worked 1 GiB -> 2 GiB, no job needed.
- `iscsi.extent.create {type: DISK, disk: "zvol/<dataset path>"}` auto-generates an NAA,
  e.g. `0x6589cfc00000027bb31e5bafcaf75064`. **The node plugin should locate the device by
  this NAA under /dev/disk/by-id/ rather than scanning.**
- `iscsi.target.create {name, groups: [{portal: <id>}]}`; full IQN is
  `<iscsi.global.basename>:<target name>` -> `iqn.2005-10.org.freenas.ctl:<name>`.
- `iscsi.targetextent.create {target, extent, lunid}` joins them.
- Deletes: `iscsi.targetextent.delete(id)`, `iscsi.target.delete(id, true)`,
  `iscsi.extent.delete(id, true, true)`, `iscsi.portal.delete(id)`.

### Snapshots and clones — DESIGN CORRECTION

- `pool.snapshot.create {dataset, name}` -> id `<dataset>@<name>`.
- `pool.snapshot.clone {snapshot, dataset_dst}` -> clone with `origin` = the snapshot.
- **`pool.dataset.promote` does NOT free the source. It INVERTS the dependency.**
  Verified: after promoting the clone, the snapshot moved to the clone and the ORIGINAL
  zvol showed `origin = <clone>@snap1`, making the SOURCE undeletable.
  Earlier assumption ("promote on clone so the source snapshot can be deleted") was wrong.

  **Decision:** do NOT promote. Leave the clone dependent on its snapshot, and return
  gRPC `FAILED_PRECONDITION` from DeleteSnapshot while dependent volumes exist — which is
  what the CSI spec prescribes anyway. Deleting a PVC must never be blocked by a clone.

- Deletion ordering matters and `{"recursive": true, "force": true}` is needed; a wrong
  order returns `EBUSY` with "dataset is busy" / "snapshot is cloned".

### Delete protection: what the method pages say (read 2026-09-08, docs only)

Read before designing the feature, from the appliance's unauthenticated docs over https.

- **`pool.dataset.rename(id, {new_name, recursive=false, force=false}) -> null`**, role
  `DATASET_WRITE`. `new_name` is the FULL new path, not a leaf. The page states outright:
  "No safety checks are performed when renaming ZFS resources. If the dataset is in use by
  services such as SMB, iSCSI, snapshot tasks, replication, or cloud sync, renaming may
  cause disruptions or service failures... Set Force to continue." `recursive` renames
  CHILD DATASETS, which a volume dataset does not have; snapshots travel with their dataset
  either way. **Consequence: the retire rename must follow share/extent teardown, and force
  must never be passed** — a refusal is the appliance saying teardown did not finish.
- **`pool.dataset.delete(id, {recursive, force}) -> true | null`**, role `DATASET_DELETE`.
  It returns **null**, not an error, when zfs destroy fails with "dataset does not exist".
  That is what makes both the retire and the reap safely repeatable across replicas.
- **`pool.dataset.update`** takes `user_properties_update: [{key, value, remove}]` with the
  key constrained to `namespace:property`. `pool.dataset.create` takes `user_properties:
[{key, value}]`. Both already used by the driver.
- **`pool.dataset.query`** returns `user_properties` as an object. The SCHEMA PAGE describes
  it only as "key-value pairs" and does not document the `{value, source}` envelope; the
  LIVE PROBE above did observe the envelope, and internal/truenas/types.go decodes it. The
  live evidence wins, but note the doc page is not sufficient on its own here.
- **`pool.dataset.promote(id) -> null`** takes the CLONE's id. Unchanged from the finding
  above: it inverts the dependency, so the reaper never promotes anything.

**ZFS naming, checked in the OpenZFS source rather than assumed** (module/zcommon/zfs_namecheck.c):
`entity_namecheck()` validates each dataset component against `valid_char()` =
`[A-Za-z0-9_.: ]` plus `-`, and additionally exempts `%`. It rejects only `.` and `..` as
WHOLE components. There is no leading-character rule: `NAME_ERR_NOLETTER` is raised in
`pool_namecheck()` alone, for POOL names. So `Pool0/k8s/.trash` is a legal dataset name —
which TrueNAS itself relies on for `<pool>/.system`. The leading dot is what makes a
collision with a namespace or PersistentVolume name structurally impossible, since a
DNS-1123 name may not begin with one.

### Volume-from-volume cloning (not yet probed)

CSI also allows cloning an existing volume, not just a snapshot. That requires an internal
hidden snapshot, which then has to be garbage-collected when either volume goes away —
a known source of leaks in other drivers. Needs an explicit design in the spec.

## Method job-ness and roles (from core.get_methods, live)

**None of the CSI-critical methods are jobs.** `pool.dataset.create/update/delete`,
`pool.snapshot.create/clone/delete`, `iscsi.*.create/delete`, `sharing.nfs.*` are all
synchronous. Of 109 job methods, the only one we need is `filesystem.setperm`
(for NFS volume permissions / fsGroup). This removes async job tracking from the main
provisioning paths — it is needed only for the permissions call.

### Minimal role set — 14 roles (verified set-cover over every method the driver calls)

    DATASET_WRITE, DATASET_DELETE, POOL_READ,
    SNAPSHOT_WRITE, SNAPSHOT_DELETE,
    SHARING_ISCSI_EXTENT_WRITE, SHARING_ISCSI_TARGET_WRITE,
    SHARING_ISCSI_TARGETEXTENT_WRITE, SHARING_ISCSI_GLOBAL_READ,
    SHARING_ISCSI_PORTAL_READ, SHARING_ISCSI_INITIATOR_READ, SHARING_ISCSI_AUTH_READ,
    SHARING_NFS_WRITE, FILESYSTEM_ATTRS_WRITE

(Write roles imply their read counterparts.) Ship this as the documented privilege for
the driver's TrueNAS account instead of FULL_ADMIN.

**Note:** `system.info` requires `READONLY_ADMIN` — a broad role. Do NOT call it merely
to log the version at startup; it would cost more privilege than everything else combined.

## Node prerequisites (verified on worker-21, k3s 1.34, Armbian rockchip64 6.12.58)

READY:

- `iscsiadm` and `iscsid` at /host/sbin (open-iscsi installed)
- Modules `iscsi_tcp`, `libiscsi`, `scsi_transport_iscsi` already LOADED
- InitiatorName configured: `iqn.2004-10.com.ubuntu:01:4f9d1b17f9aa`
- `mount.nfs`, `mkfs.ext4`, `blkid` present

MISSING:

- `multipath` / `multipathd` binaries absent; `dm_multipath` not loaded
  (module .ko IS on disk). Multipath cannot work without host packages -> reinforces
  "implemented but unvalidated", and becomes a documented prerequisite.
- `mkfs.xfs` absent -> XFS volumes would fail at node publish using host tools.
- `nvme` (nvme-cli) absent; `nvme_tcp` not loaded, though the .ko IS on disk
  (`.../kernel/drivers/nvme/host/nvme-tcp.ko`) — fine for the later NVMe milestone,
  but hosts will need nvme-cli.

### Design consequence: which tools come from the image vs the host

- **Bundle in the node plugin image:** mkfs.ext4/mkfs.xfs, fsck, resize2fs/xfs_growfs.
  Filesystem tooling has no reason to be a host dependency, and bundling fixes the
  missing-mkfs.xfs problem outright.
- **Must use the host's:** `iscsiadm` (it must talk to the host's iscsid and share
  /etc/iscsi + /var/lib/iscsi state) — invoked via nsenter/chroot into the host mount
  namespace. Same for `multipathd` when present.
- The node DaemonSet therefore needs host mount propagation, /etc/iscsi and /var/lib/iscsi
  host paths, and privileged access — standard for CSI but must be explicit in the chart.

## Concurrency ceiling — MEASURED: 20 in-flight calls per connection

    16 concurrent  -> 0 errors
    32 concurrent  -> 12 errors  (20 succeeded)
    64 concurrent  -> 44 errors  (20 succeeded)
    128 concurrent -> 107 errors (21 succeeded)

Excess calls fail with JSON-RPC code **-32000** ("too many concurrent calls") and carry
NO `data`/`errname`. The client MUST hold a bounded in-flight semaphore — use 16 for
headroom — and treat -32000 as retryable-with-backoff, never as a volume failure.

Also proven by this probe: a naive request/response client that reads a reply per call
CANNOT do concurrent calls at all. The Go client needs **one reader goroutine
demultiplexing responses to waiters by JSON-RPC id**, plus a separate path for
id-less `collection_update` notifications.

## Error mapping — `errname` IS UNRELIABLE

    delete nonexistent dataset   code=-32602 errname=EINVAL
        reason: "[ENOENT] None: PoolDataset ... does not exist"

The structured `errname` reports EINVAL while the true errno (ENOENT) appears only as a
text prefix inside `reason`. Mapping gRPC status from `errname` would turn a NOT_FOUND
into INVALID_ARGUMENT and break CSI idempotency (DeleteVolume must succeed for an
already-deleted volume).

**Design rule: query-then-act.** Establish existence/state with an explicit query and
drive idempotency from that. Parse the `[ERRNO]` prefix in `reason` only as a secondary
signal, never as the primary decision input.

Observed cases:

| operation                   | code   | errname | real meaning                               |
| --------------------------- | ------ | ------- | ------------------------------------------ |
| dataset already exists      | -32602 | EINVAL  | already exists ("Path ... already exists") |
| delete nonexistent dataset  | -32602 | EINVAL  | ENOENT                                     |
| get_instance nonexistent    | -32602 | EINVAL  | ENOENT                                     |
| create in nonexistent pool  | -32602 | EINVAL  | bad config                                 |
| zvol > 80% of pool (thick)  | -32602 | EINVAL  | capacity refusal                           |
| extent name > 64 chars      | -32602 | EINVAL  | validation                                 |
| snapshot of missing dataset | -32001 | EFAULT  | ENOENT                                     |
| shrink zvol                 | -32602 | EINVAL  | refused outright                           |
| extent for missing zvol     | -32602 | EINVAL  | ENOENT                                     |
| unknown method              | -32601 | (none)  | JSON-RPC standard                          |

Notes:

- **Shrink is refused by middleware** ("You cannot shrink a zvol"). Matches CSI, where
  expansion is grow-only — ControllerExpandVolume must reject shrink before calling out.
- **Thick zvols above 80% of free pool space are rejected.** Sparse provisioning avoids
  this; the StorageClass `sparse` parameter therefore changes failure behaviour, not just
  space accounting, and must be documented as such.

## END-TO-END iSCSI VALIDATED (NAS -> worker-21 -> mounted filesystem)

Full path proven on real hardware, then fully torn down (Pool0 back to 19 datasets,
0 iSCSI objects, node logged out, debug pods deleted).

### Device discovery is deterministic — no scanning needed

The NAA returned by `iscsi.extent.create` maps directly to stable by-id symlinks:

    naa from API: 0x6589cfc000000a960e31390c2657efa7
    /dev/disk/by-id/scsi-36589cfc000000a960e31390c2657efa7 -> ../../sdc
    /dev/disk/by-id/wwn-0x6589cfc000000a960e31390c2657efa7 -> ../../sdc

Note the `scsi-3` prefix (NAA designator type) before the digits, and `wwn-0x` for the
other form. NodeStageVolume should resolve `/dev/disk/by-id/scsi-3<naa without 0x>` and
must NOT scan /dev for new devices — scanning races with other drivers on the same node.

### Verified node sequence

    iscsiadm -m discovery -t sendtargets -p <portal>
    iscsiadm -m node -T <iqn> -p <portal>:3260 --login
    resolve /dev/disk/by-id/scsi-3<naa>  -> mkfs.ext4 -> mount -> read/write OK
    umount; iscsiadm -m node -T <iqn> -p <portal>:3260 --logout
    iscsiadm -m node -o delete -T <iqn> -p <portal>:3260

### Online expansion VERIFIED (no unmount, data intact)

    pool.dataset.update <zvol> {volsize: 2GiB}      # controller side
    iscsiadm -m node -T <iqn> -R                    # node rescan
    blockdev --getsize64: 1073741824 -> 2147483648  # device grew live
    resize2fs <dev>: 974M -> 2.0G                   # filesystem grew
    file written before the resize was still readable after

So ControllerExpandVolume = volsize update; NodeExpandVolume = rescan + fs grow.
No re-login, no unmount, no pod restart required.

## COEXISTENCE: Longhorn already uses the node iSCSI stack

worker-21 carries live Longhorn sessions (`iqn.2019-10.io.longhorn:pvc-...`) and node
records. Our driver shares `iscsid`, `/etc/iscsi` and `/var/lib/iscsi` with Longhorn.

Rules this imposes:

- NEVER use blanket operations: no `--logoutall=all`, no `-m node -o delete` without an
  explicit `-T <iqn> -p <portal>`, no global `iscsiadm -m session --rescan`.
- Always scope every iscsiadm call to our specific target and portal.
- Do not rewrite global `/etc/iscsi/iscsid.conf`; per-node settings only.
- The initiator name is shared and already set
  (`iqn.2004-10.com.ubuntu:01:4f9d1b17f9aa`) — do not regenerate it.

## END-TO-END NFS VALIDATED (NAS -> worker-21 -> mounted, non-root write)

NFS service RUNNING/enabled; `nfs.config.protocols = ["NFSV3","NFSV4"]`.
Mounted from worker-21 as **nfs4 (vers=4.2)**; teardown left the box exactly as found
(19 datasets, only the 3 pre-existing shares).

### FINDING 1 — a fresh dataset is root:root 0755; non-root pods CANNOT write

Verified on the node: uid 1000 got "Permission denied" on the default dataset.
This is the single most common NFS-CSI failure mode.

Fix verified: `filesystem.setperm {path, mode, uid, gid, options:{recursive}}`
-> uid 1000 write then succeeded. **This is the ONE job-based method the driver needs.**

Job semantics confirmed:
filesystem.setperm(...) -> returns an INT job id (e.g. 9615)
poll core.get_jobs [["id","=",<jobid>]] until state in SUCCESS|FAILED|ABORTED

Note `fsGroup` does NOT solve this — kubelet skips fsGroup ownership changes for NFS.
Permissions must be set on the TrueNAS side at provisioning time, driven by StorageClass
parameters (mode/uid/gid), not left to Kubernetes.

### FINDING 2 — without refquota, an NFS volume reports the WHOLE POOL

    no quota:        df -> 31T  (pool free space, for a "10Gi" PVC)
    refquota=10GiB:  df -> 10G  (correct)

Consequences if omitted: capacity is meaningless to the pod, NodeGetVolumeStats reports
pool-wide numbers to kubelet so usage alerts never fire, and a single PVC can consume the
entire pool. **Every NFS volume MUST get `refquota` set to the requested capacity.**
Expansion for NFS = `pool.dataset.update {refquota: N}`, verified 5 GiB -> 10 GiB.

### FINDING 3 — shrink guards are ASYMMETRIC between backends

    zvol volsize 2GiB -> 1GiB : REFUSED by middleware
    refquota 10GiB -> 5GiB    : SILENTLY ALLOWED

So ControllerExpandVolume must reject shrink **in the driver** for the NFS path; TrueNAS
will happily shrink a refquota below current usage. Do not rely on middleware to guard it.

### FINDING 4 — prefer NFSv4

Mounting with `vers=3` caused systemd on the node to enable rpc-statd
("Created symlink .../rpc-statd.service") because v3 needs the separate lock manager.
NFSv4 mounted with no such dependency. Default to `vers=4`, make it a StorageClass option.

### Verified node sequence (NFS)

    mount -t nfs -o vers=4 <nas>:/mnt/<pool>/<parent>/<vol> <target>
    ... read/write as non-root once setperm has run ...
    umount <target>

## END-TO-END SNAPSHOT / RESTORE VALIDATED (with real data integrity check)

Method: wrote `data.txt` + a 4 MiB random `blob.bin` to an NFS volume from worker-21,
recorded its md5, snapshotted, then DELIBERATELY CORRUPTED the source (overwrote data.txt,
deleted blob.bin), then cloned the snapshot into a new volume and mounted it.

    restored data.txt : "IMPORTANT DATA v1"   (pre-corruption content)
    restored blob md5 : 942a58af22148a643e34b420aded8871
    original blob md5 : 942a58af22148a643e34b420aded8871   -> EXACT MATCH

`pool.snapshot.create {dataset, name}` -> id `<dataset>@<name>`, immediately queryable
via `pool.snapshot.query` (fields incl. `createtxg`, usable as a creation ordering key).

### FINDING — a clone inherits NEITHER the ownership marker NOR refquota

    SOURCE (marked at create)  marker=YES source=LOCAL   refquota=1GiB
    CLONE  (from snapshot)     marker=NO                 refquota=None

ZFS clones take properties from their position in the dataset hierarchy, not from the
origin dataset. Consequences if unhandled:

1. **Every restored volume would leak forever.** With no marker, the delete guard
   (`source == "LOCAL"`) refuses to delete it — DeleteVolume would fail permanently for
   any volume created from a snapshot.
2. **Every restored volume would report the whole pool** (refquota unset), same failure
   as FINDING 2 in the NFS section.

**CreateVolume-from-snapshot must therefore explicitly, after cloning:**

- set the ownership marker via
  `pool.dataset.update <id> {"user_properties_update":[{"key":..., "value":...}]}` (verified)
- set `refquota` (filesystem) or confirm `volsize` (zvol) to the requested capacity
- run `filesystem.setperm` for NFS volumes (a clone's permissions come from the snapshot,
  which may not match the new volume's StorageClass parameters)

## Node capability preflight (single uniform mechanism for ALL protocols)

Decision: the node plugin does NOT bundle tooling. At startup it probes the host for each
capability's binaries and kernel modules, advertises only what it can actually deliver,
and logs exactly what the operator must install for anything missing. A StorageClass
requesting an unavailable capability fails fast with a clear message instead of a cryptic
mount error, and the node reports the gap in its readiness/status.

Measured on worker-21 (Armbian rockchip64 6.12.58) — the current state of your nodes:

| capability | needs                                     | present?                         | to install        |
| ---------- | ----------------------------------------- | -------------------------------- | ----------------- |
| NFS        | `mount.nfs`                               | YES                              | —                 |
| iSCSI      | `iscsiadm`, `iscsid`, `iscsi_tcp`         | YES (modules already loaded)     | —                 |
| ext4       | `mkfs.ext4`, `resize2fs`                  | YES                              | —                 |
| XFS        | `mkfs.xfs`, `xfs_growfs`                  | **NO**                           | `xfsprogs`        |
| SMB        | `mount.cifs`, `cifs` module               | **NO** (cifs.ko on disk)         | `cifs-utils`      |
| NVMe-oF    | `nvme`, `nvme_tcp` module                 | **NO** (nvme-tcp.ko on disk)     | `nvme-cli`        |
| multipath  | `multipath`, `multipathd`, `dm_multipath` | **NO** (dm-multipath.ko on disk) | `multipath-tools` |

Every missing item's kernel module IS present on disk — only userspace packages are
absent, so enabling any of these is an apt install plus a module load, not a kernel change.

Rules:

- Probe binaries by absolute path in the host mount namespace, and modules via
  /proc/modules plus a modinfo/`.ko` existence check (available-but-not-loaded is a
  DIFFERENT state from unavailable, and is recoverable with modprobe).
- Never silently degrade: if a StorageClass asks for xfs on a node without `mkfs.xfs`,
  fail the NodeStageVolume with a message naming the missing package.
- Multipath is the one exception where degrading is correct: fall back to single-path
  and log a warning, since the volume still works.

## END-TO-END SMB VALIDATED (deferred protocol, but proven)

Installed `cifs-utils` on worker-21, loaded the `cifs` module, mounted and wrote.

    mount -t cifs //192.168.10.253/<share> <target> -o credentials=<file>,vers=3.0
    -> mounted, write OK, read back OK

### FINDING — SMB datasets use a DIFFERENT permission model to NFS

`pool.dataset.create {share_type: "SMB"}` yields **mode 0770 with an NFSv4 ACL**
(`filesystem.stat -> acl: true`), whereas a plain dataset is 0755 with no ACL.
So SMB volumes need `filesystem.setacl`, NOT the `filesystem.setperm` used for NFS.
The two file backends do not share a permissions path.

### FINDING — SMB ownership is a MOUNT-TIME concern

The client mount reports `uid=0,noforceuid,gid=0,file_mode=0755,dir_mode=0755`.
CIFS maps ownership client-side via mount options, so for SMB the uid/gid/mode a pod sees
is set by NodeStageVolume mount options, not by anything done on TrueNAS. Opposite of NFS.

### SMB requires credentials

An SMB share needs a real TrueNAS user (`user.create {..., smb: true}`). The driver must
take SMB credentials from a Secret and write a credentials file for `mount.cifs` — never
pass the password on the command line, where it is visible in the process table.

### SMB also reports the whole pool without a quota

`df` showed 31T for the share. Same refquota requirement as NFS — it applies to every
filesystem-backed volume, regardless of protocol.

## END-TO-END NVMe/TCP VALIDATED (deferred protocol, but proven)

Installed `nvme-cli`, loaded `nvme_tcp` (`/dev/nvme-fabrics` appeared), then:

    nvmet.subsys.create {name, allow_any_host}   -> subnqn = <global basenqn>:<name>
                                                    plus an auto-generated `serial`
    nvmet.namespace.create {subsys_id, device_type: "ZVOL", device_path: "zvol/<ds>"}
    nvmet.port.create {addr_trtype: "TCP", addr_traddr: <ip>, addr_trsvcid: 4420}
    nvmet.port_subsys.create {port_id, subsys_id}

    node: nvme discover -t tcp -a <ip> -s 4420
          nvme connect  -t tcp -a <ip> -s 4420 -n <subnqn>
          mkfs.xfs -> mount -> write/read OK
          nvme disconnect -n <subnqn>

### FINDING — device resolution must key on the subsystem SERIAL

worker-21 has its OWN local NVMe SSD at `/dev/nvme0n1` (Lexar NM620 2TB); our volume
attached as `/dev/nvme1n1`. Never assume an index. The stable link is:

    /dev/disk/by-id/nvme-TrueNAS_TVS-1688_adfa04662ffcf780febc -> ../../nvme1n1
                         ^model (varies by NAS)  ^serial from nvmet.subsys.create

So match on the `serial` returned by `nvmet.subsys.create` (glob `nvme-*_<serial>`),
not on the model string, which differs per appliance.

### FINDING — volblocksize surfaces as physical sector size

`mkfs.xfs` warned: "specified blocksize 4096 is less than device physical sector size
16384; switching to logical sector size 512". The zvol's `volblocksize` is visible to the
initiator as physical sector size and interacts with filesystem creation. Block-size
choice is therefore not purely a performance knob — document it and pick defaults
deliberately.

### Production note

The probe used `allow_any_host: true`. Production should register host NQNs via
`nvmet.host` + `nvmet.host_subsys` — the NVMe equivalent of iSCSI initiator groups —
and optionally DH-CHAP (`nvmet.host.generate_key`).

## Final state after ALL probing

Pool0: 19 datasets (unchanged). NFS shares: 3 (original). SMB shares: 10 (original).
iSCSI extents/targets/portals: 0. nvmet subsys/ports/namespaces: 0. Probe user removed.

### Cluster brought to a uniform baseline (all 8 nodes)

`cifs-utils`, `nvme-cli` and `xfsprogs` installed on every node
(master-11/12/13, worker-21..25); `cifs` and `nvme_tcp` loaded on each. Verified:
all report "MISSING: none". So XFS, SMB and NVMe/TCP are available cluster-wide.

`multipath-tools` was deliberately NOT installed. multipathd claims block devices on
sight and can seize devices already in use by Longhorn's iSCSI volumes unless it is
blacklisted first. Installing it is a separate, deliberate change requiring a
`/etc/multipath.conf` blacklist — not a side effect of a probe.

### Kernel module loading — DECIDED

The node plugin `modprobe`s what it needs at startup (cifs, nvme_tcp, iscsi_tcp,
and dm_multipath when multipath is enabled) and reports anything it could not load.
Rationale: self-healing, works on any node the DaemonSet lands on, and a newly added
node needs no manual host config. Nothing is written to `/etc/modules-load.d`, so the
modules loaded during probing will NOT survive a reboot until the driver loads them —
which is the intended behaviour, not a gap.

## Reporting, client lists and share ACLs (verified 2026-09-08, 25.10.6)

Auth: the appliance's admin account here is **`truenas_admin`**, not `root`, `admin`
or `csi`. An API key is bound to a user; the wrong username yields
`response_type=AUTH_ERR` from `auth.login_ex` even when the key is valid. That is a
rejection, not a revocation — the key survives, because the connection was `wss://`.

### reporting.get_data cannot answer per-volume questions

`reporting.netdata_graphs` returns exactly **40 graphs** and **none is per-dataset,
per-zvol or per-pool I/O**:

    cpu, cputemp, memory, disk (17 ids: PHYSICAL devices sdc, nvme0n1, ...),
    interface (2 NICs), load, uptime, arcsize, arcfreememory, arcavailablememory,
    ~24 demand*/l2arc* ARC counters, disktemp (17 ids), ups* (6)

`reporting.graphs` returns the same payload. `reporting.realtime` does not exist
(`jsonrpc -32601`). So per-volume IOPS/bandwidth/latency **cannot** come from the
appliance; it must be measured node-side from `/proc/diskstats` and
`/proc/self/mountstats`. Do not go looking for a dataset graph again.

That node-side measurement is now implemented (`internal/podmon/volumeio.go`,
`internal/obs/volumeio.go`): counters per volume labelled with the PV, pod and
namespace, exported by the node plugin. Its two inherent limits — only mounted
volumes are visible, and a volume's series moves between node exporters when its
pod reschedules — are documented in `docs/metrics.md`.

Request shape is `[[{"name":"cpu"}], {"start":<unix>,"end":<unix>}]` — the window is
one object, not two positional arguments. Response:
`[{"name","identifier","data":[[unix_ts, v1, v2, ...], ...]}]`.

### NFS client list: use the v4 call

`nfs.get_nfs3_clients` returns `[]` when exports are v4 — it is not "no clients", it
is the wrong call. `nfs.get_nfs4_clients` is the real one and gives liveness per
client:

    [{"id":"3","info":{"clientid":<int>,"address":"192.168.10.102:666",
      "status":"confirmed","seconds from last renew":14,
      "name":"Linux NFSv4.2 <hostname>","minor version":2,
      "callback state":"UP","callback address":"192.168.10.102:0",
      "admin-revoked states":0},"states":[]}]

`address` is `host:port` and must be split. Several keys contain **spaces**
(`"seconds from last renew"`, `"minor version"`, `"callback state"`) and need
explicit struct tags. `seconds from last renew` is the freshness signal a
connectivity check should use.

Counts: `nfs.client_count` and `iscsi.global.client_count` both return a bare
integer.

### Share access lists, and the empty-list trap

`sharing.nfs.query` has **two independent flat top-level arrays**, `hosts` and
`networks`. Access is unrestricted only when **both** are empty — and that is the
live state of shares 1 and 2 on this appliance today. Emptying `hosts` to revoke a
node does not fence it if `networks` is also empty; it exports to everyone. Any
per-node revoke must assert on both fields.

`sharing.smb.query` keeps its host lists **nested under `options`**
(`options.hostsallow`, `options.hostsdeny`), and `options` carries many unrelated
sibling keys that vary by `purpose` (`DEFAULT_SHARE` is small, `LEGACY_SHARE` adds
recyclebin, guestok, streams, durablehandle, shadowcopy, timemachine, ...). Updating
the host lists requires a read-modify-write of the whole `options` object; sending a
fresh one wipes the rest of the share's configuration.

### Still unverified

`iscsi.global.sessions` returns `[]` and `iscsi.global.client_count` returns `0`
while nothing is attached. The call is correct; the **populated element shape is
unverified** and needs a run with a LUN actually attached.

## Session observability per protocol (enumerated 2026-09-08 from the live docs)

Of the 913 documented methods, these are the only per-client session sources:

| Protocol | Method                  | Notes                                                                                                                                                                                                                                                                                                                           |
| -------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| iSCSI    | `iscsi.global.sessions` | fields `initiator`, `initiator_addr`, `initiator_alias`, `target`, `target_alias`. Returns `[]` with nothing attached; **populated element shape still unverified**                                                                                                                                                             |
| NFS      | `nfs.get_nfs4_clients`  | the real one for v4 exports; carries `seconds from last renew`                                                                                                                                                                                                                                                                  |
| NVMe-oF  | `nvmet.global.sessions` | fields `host_traddr`, `hostnqn`, `subsys_id`, `port_id`, `ctrl` (schema read from the live docs 2026-09-08; role `SHARING_NVME_TARGET_READ`). Wrapped as `truenas.NVMeSessions` and consumed by the connectivity service, so NVMe-oF **is** fenceable. **Populated element shape still unverified** — no initiator was attached |
| SMB      | **none**                | the entire SMB surface is `smb.config/update/bindip_choices/unixcharset_choices` plus `sharing.smb.*`. The older `smb.status` is gone from 25.10. SMB connectivity is therefore _unobservable_, not merely unknown                                                                                                              |

Consequence for fencing: an SMB volume can never satisfy the fence precondition,
so an SMB-only pod is never force-deleted. That is the safe direction, but it
must be stated rather than discovered.

`nfs.config` exposes no lease-period field. The NFSv4 lease is assumed to be the
90s default and `DefaultStaleLeaseAfter` is 2x that; confirm with
`/proc/fs/nfsd/nfsv4leasetime` on the appliance and raise it if that differs.

## The NFS fence, verified end to end (2026-09-08, live appliance)

Driving the real controller against the appliance and reading the export back
after each step:

    at create      hosts=["192.0.2.1"]                     networks=[]
    after publish  hosts=["192.0.2.1", "192.168.10.212",
                          "fd6e:ee75:...:5198"]            networks=[]
    after fence    hosts=["192.0.2.1"]                     networks=[]

Three things this establishes:

1. **A volume is created already fenced.** There is no window between provision
   and first attach in which the export is open.
2. **`ControllerUnpublishVolume` really revokes.** The node's addresses are gone
   from the appliance's own access list, not merely from driver bookkeeping.
3. **The unroutable deny host is always present and `networks` is always empty**,
   so the "both lists empty means exported to everyone" state is never reached.

IPv6 node addresses are granted alongside IPv4 without special handling.

### Consequence for the node-side e2e tests

`test/integration/e2e_test.go` mounts from a cluster node **without** calling
`ControllerPublishVolume`, which used to work because exports were created open.
Against the fence the server answers such a client with a bare

    mount.nfs: ... failed, reason given by server: No such file or directory

which is an NFS server refusing to admit the export exists to a host that is not
in its access list — not a missing dataset. Those tests must publish first.

Note this needs a node resolver that can see real Node objects:
`LocalNodeResolver` returns _this process's_ interface addresses, so publishing
from a laptop grants the laptop and the cluster node still cannot mount. That is
correct and safe, and it is why `csi.NewController` needs the resolver to be
injectable for the node-side suite to run outside the cluster.

## Mutable dataset properties, verified 2026-09-08

Driving `pool.dataset.update` against a real dataset, exactly as
`ControllerModifyVolume` does:

| property       | sent       | result                                                                     |
| -------------- | ---------- | -------------------------------------------------------------------------- |
| `sync`         | `DISABLED` | accepted, `source=LOCAL`                                                   |
| `compression`  | `ZSTD`     | accepted, `source=LOCAL`                                                   |
| `atime`        | `OFF`      | accepted, `source=LOCAL`                                                   |
| `recordsize`   | `1M`       | accepted, `source=LOCAL`                                                   |
| `sync`         | `INHERIT`  | accepted — reverts to `STANDARD` with `source=DEFAULT`                     |
| `primarycache` | `metadata` | **rejected**: `[EINVAL] data.primarycache: Extra inputs are not permitted` |
| `logbias`      | `metadata` | **rejected**: `[EINVAL] data.logbias: Extra inputs are not permitted`      |

Three things this settles:

1. **`INHERIT` is the undo, and it works.** The property returns to the pool
   default and its `source` returns to `DEFAULT`. That is why idempotency in
   `ControllerModifyVolume` is checked by **source** and not by value: a
   dataset whose `sync` reads `STANDARD` may be inheriting it or holding it
   locally, and those are different states to re-apply.

2. **`primarycache` and `logbias` are not settable through the middleware**,
   whatever ZFS itself allows. They were proposed for the allowlist from
   memory and refused on the strength of the published schema; the appliance
   confirms the schema. Do not re-propose them.

3. The uppercase enum spellings are what the middleware wants
   (`STANDARD`/`ALWAYS`/`DISABLED`/`INHERIT`), so a `VolumeAttributesClass`
   written in ZFS's natural lowercase has to be normalised, not rejected.

## Delete protection, verified end to end (2026-09-09) — three bugs, one important

Driving the real controller against the appliance found three defects that every
unit test passed, because the fake is more permissive than the middleware in
three different ways.

### 1. `pool.dataset.rename` requires `force` on EVERY rename

The method's warning reads like a conditional safety gate:

> No safety checks are performed when renaming ZFS resources... Set Force to
> continue.

It is not conditional. A freshly retired dataset with no share, no extent and
nothing holding it is still refused:

    [EINVAL] pool.dataset.rename.force: ... please set force and proceed

So `force: false` does not make a caller careful, it makes the call fail 100% of
the time. Safety has to come from the ORDER of operations — tear the share and
extent down first — not from withholding the flag.

### 2. ZFS user property names must be lowercase

`io.truenas.csi:deletedAt` is rejected:

    cannot set property for '<dataset>': invalid property 'io.truenas.csi:deletedAt'

zfs accepts only lowercase letters, digits and `:-._` after the namespace. The
properties are now `deleted-at` and `retired-from`. Every other property in this
driver was already lowercase; these two were the outliers.
`internal/volume/propertyname_test.go` now fails on any illegal spelling.

### 3. An INHERITED user property is reported with `source: "LOCAL"`

This is the one worth remembering. **ZFS inherits user properties to children,
and TrueNAS does not distinguish the inherited value from a locally-set one** —
`pool.dataset.query` reports `source: "LOCAL"` for both. Verified deliberately:

    Pool0/inhprobe         managed = truenas-csi  source=LOCAL   (set here)
    Pool0/inhprobe/child   managed = truenas-csi  source=LOCAL   (INHERITED,
                                                    never set, never created
                                                    by the driver)

Immediate consequence: every dataset renamed into the graveyard inherits the
root's `graveyard` marker, so a "refuse anything carrying the marker" check
refused every retired volume for ever. The reaper now separates the root from
its entries by PATH, which is exact.

**Wider consequence, not yet addressed.** `volume.VerifyOwned` requires
`io.truenas.csi:managed` at `source == LOCAL` specifically to distinguish "this
driver created it" from "this merely sits under something the driver created".
On TrueNAS that distinction does not exist for user properties. A dataset an
operator creates by hand underneath a marked dataset inherits the marker and
passes the ownership check. The practical exposure today is small — the marked
datasets with children are the graveyard root and, when namespace quotas are on,
the namespace datasets — but the guarantee the code documents is stronger than
what the platform provides, and any future guard built on `source == LOCAL`
inherits the same weakness.

## Ownership cannot be established from a property's source (fixed 2026-09-09)

`pool.dataset.query` reports **every** user property with `source: "LOCAL"`,
inherited or not. `zfs.dataset.query`, over the same datasets, reports it
correctly:

    Pool0/p     managed=truenas-csi   pool.dataset.query: LOCAL   zfs.dataset.query: LOCAL
    Pool0/p/c   managed=truenas-csi   pool.dataset.query: LOCAL   zfs.dataset.query: INHERITED

`volume.VerifyOwned` was written around the source field, so on TrueNAS a
dataset an operator created by hand underneath a driver-owned one inherited the
marker, read as LOCAL, and passed the guard that stands in front of every
destructive path.

### Why not just call zfs.dataset.query

It is the accurate answer, and it was rejected on cost: correcting the sources
means a second middleware call on every dataset query, against an appliance with
a hardware-verified 20-call concurrency ceiling — and it would make correctness
depend on a call 23 test files would have to script, i.e. on the fake being
honest, which is precisely what failed here.

### What is used instead

`io.truenas.csi:owner-id`, carrying the id of the dataset the marker was stamped
on. A child inherits its ANCESTOR's id, which is not its own, so inheritance is
visible in the VALUE and no source field is consulted. Verified on the appliance
against the real failure:

    Pool0/k8s/pvc-own-…                 owner-id=Pool0/k8s/pvc-own-…   VerifyOwned: owned
    Pool0/k8s/pvc-own-…/operator-data   owner-id=Pool0/k8s/pvc-own-…   VerifyOwned: REFUSED
                                        (both report source=LOCAL)

Two consequences worth remembering:

- **`Retire` must re-stamp the owner id after the rename.** The property names
  the dataset it was set on, and a rename changes that name; left alone the
  retired dataset's id names its original path, the guard reads the mismatch as
  inheritance, and the reaper refuses it for ever.
- Datasets stamped before this existed have no owner id and fall back to the
  old source check. That is no weaker than what they always had, but it cannot
  detect inheritance — so the fallback is legacy-only and every dataset stamped
  from now on carries the id.

## Snapshot properties (verified 2026-09-09 on 25.10.6)

`pool.snapshot.query` returns a `properties` block. Two fields matter and both
have traps:

- **`creation`** is the only durable snapshot timestamp. Its `parsed` is an
  OBJECT — `{"$date": <milliseconds>}` — and its `value` is a human string
  ("Wed Sep  9 11:07 2026"). Only `rawvalue` is machine-readable, and it is unix
  SECONDS. Any decoder that reads `parsed` as a number or a numeric string gets
  nothing.
- **`volsize`** is present on a zvol snapshot and carries the source's
  PROVISIONED size. On a FILESYSTEM snapshot it is absent — and so is
  `refquota`, which is not carried on a snapshot at all. A filesystem
  snapshot's provisioned size can only come from its live dataset.

`pool.snapshot.create` does not return the property block, so a creation time
has to be read back with a second query.

## Kubernetes protects a snapshot before the driver ever sees the delete

Verified on the live cluster: deleting a VolumeSnapshot that any PVC names in
`spec.dataSource` never reaches `DeleteSnapshot`. The snapshot-controller holds
`volumesnapshot-as-source-protection` and events
`SnapshotDeletePending: Snapshot is being used to restore a PVC` — and it holds
it even after that PVC is Bound. The driver's own FailedPrecondition for a
dependent ZFS clone is therefore unreachable from the CO in normal use; it
still guards static content and direct gRPC callers, and stays unit-tested.

## csi-resizer must match the VolumeAttributesClass API the cluster serves

v1.12.0 lists `*v1beta1.VolumeAttributesClass` only. On 1.34 — which serves
`storage.k8s.io/v1` — it logs "the server could not find the requested
resource" forever and never calls ControllerModifyVolume. Nothing fails; the
feature is simply inert. v2.2.1 uses the v1 informer. Verified end to end:
after the bump the PVC reached `status.currentVolumeAttributesClassName` and
the zvol really became `sync=DISABLED compression=ZSTD`.

## The claim name reaches the node only if the controller sends it

Kubernetes passes `csi.storage.k8s.io/pvc/name` to CreateVolume and NOT to
NodePublishVolume; `podInfoOnMount` supplies pod name and namespace but never
the claim. Echoing it into the volume context at CreateVolume is the only way
a node-side metric can carry a `pvc` label without giving the node plugin
API-server credentials. Verified: `pvc="label-check"` on a volume provisioned
after the change, empty on one provisioned before it.

## A missing destination PARENT is reported as ENOENT naming the SOURCE

`pool.dataset.rename` into a path whose parent does not exist fails with
`[ENOENT] Dataset '<SOURCE>' not found` — naming the dataset that does exist.
Verified while recovering a retired volume whose namespace dataset had been
reclaimed. Anyone reading the error concludes the source is gone.

`pool.dataset.create` is clearer about the same condition:
`[EINVAL] pool_dataset_create.name: Parent dataset (<parent>) does not exist.`
EINVAL, with the real cause only in the text — matched by `IsParentMissing`.

## A recursive snapshot takes the whole subtree, and there is no subset form

`pool.snapshot.create` with `recursive: true` snapshots the dataset and EVERY
descendant in one transaction group. There is no way to snapshot a subset
atomically. Measured: a group snapshot of two volumes produced four snapshots
(anchor, two members, one bystander). The driver now deletes the non-member
snapshots immediately after the recursive create — atomicity is unaffected,
because the members were already captured in one transaction group.

## Graveyard markers are inherited, not set

`io.truenas.csi:graveyard` lives on the graveyard dataset, so an entry inside it
merely INHERITS the marker. A `zfs rename` out of the graveyard drops it, and
trying to remove it explicitly fails the whole property update with
`[EINVAL] properties.<key>: Property does not exist and cannot be inherited` —
a `user_properties_update` is applied atomically, so one bad key discards the
others in the same call.

## Namespace quotas: a ZFS quota does not bound thin volumes

A `quota` on a namespace dataset charges nothing for a child's `refquota` until
data is written. Verified: three 1 GiB claims all bound under a 2 GiB namespace
quota. Bounding over-provisioning needs the driver to total the children's
`refquota`/`volsize` itself; the appliance will not do it.

## refquota has a hard 1 GiB floor; volsize has none

`pool.dataset.create` rejects a `refquota` below 1073741824 with
`[EINVAL] data.PoolDatasetCreateFilesystem.refquota.constrained-int: Input
should be greater than or equal to 1073741824`, alongside three unrelated
complaints about the `PoolDatasetCreateVolume` variant the middleware also
tried — none of which name the caller's mistake. Verified: 1073741823 refused,
1073741824 accepted. A zvol has no equivalent floor; `volsize` of 64 MiB was
accepted. So NFS and SMB volumes must be rounded up to 1 GiB, iSCSI and
NVMe/TCP need not be.

## 25.10 has no SMART API, and disk.query hides the pool by default

`core.get_methods` on 25.10.6 lists 769 methods and NOT ONE in the `smart.*`
namespace: `smart.test.results` and `smart.config` both return jsonrpc -32601.
`disk.query` no longer returns a `togglesmart` field either. Any code that
infers "SMART is disabled" from those absences is asserting something about the
operator's hardware that it never checked.

`disk.query` also returns `"pool": null` for every disk unless called with
`{"extra": {"pools": true}}`. The field is present, so nothing errors — the
answer is just empty.

## TrueNAS MASKS secrets it will not let you read, it does not refuse

`iscsi.auth.query` from an account holding only `SHARING_ISCSI_AUTH_READ`
returns the entry with `"secret": "********"` — eight asterisks — and no error.
An account with `SHARING_ISCSI_AUTH_WRITE` gets the real 12-16 character value.

This is the most dangerous appliance behaviour found so far, because the
failure lands nowhere near its cause: the driver reads the mask, puts it in the
publish context as the CHAP password, the volume provisions, the claim binds,
and every attach then fails on the NODE with
`iscsi login failed due to authorization failure` while the controller's logs
show nothing wrong. A real CHAP secret can never look like the mask (12-16
characters required, and the mask is 8 asterisks), so it can be detected.

## The documented least-privilege role set was missing NVMe

`nvmet.global.config` returns `[EACCES] Not authorized` without
`SHARING_NVME_TARGET_WRITE`. 25.10 exposes `SHARING_NVME_TARGET_READ` and
`SHARING_NVME_TARGET_WRITE`; the driver needs the write role.

Verified method: create a local group, a user in it, a `privilege` granting
exactly the documented roles to that group, and an `api_key` for the user, then
run the whole integration suite as that account. `privilege.roles` lists all 141
role names the appliance knows.
