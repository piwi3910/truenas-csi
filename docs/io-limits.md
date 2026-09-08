# Per-volume I/O limits

Cap how fast one pod may read and write one volume. Enforced **on the node**,
with cgroup v2 `io.max`, for the **block protocols only** (`iscsi`, `nvme`).

## Why the node, and not the appliance

TrueNAS/OpenZFS has no per-dataset or per-zvol IOPS or bandwidth limiter. There
is no ZFS property, no middleware call and no share option that caps a single
volume's throughput. The appliance therefore cannot offer QoS, and no amount of
controller-side work would change that — which is why the CSM comparison
concluded that PowerFlex's array-side `bandwidthLimitInKbps` / `iopsLimit` "does
not translate".

That conclusion was right about the appliance and wrong about the node. For
iSCSI and NVMe/TCP the volume _is_ a block device on the node, and the kernel's
block throttle (cgroup v2 `io.max`) caps a cgroup's traffic to one device by its
`major:minor`. The pod already has a cgroup. So the limit is applied there.

For NFS and SMB there is no device — only a mount — and the I/O leaves the node
as RPC or cifs traffic that never passes through `blk-throttle`. That case is
**refused**, not ignored; see [On NFS and SMB](#on-nfs-and-smb).

## What this is, precisely

> It throttles **this pod's** access to the volume's block device **on this
> node**. Nothing more.

- **It is not appliance-side QoS.** Another pod, on another node, hammering the
  same pool is completely unaffected.
- **It does not protect the appliance.** Ten pods capped at 100 MB/s each can
  still ask the pool for 1 GB/s. This is per-workload isolation, not admission
  control.
- **It does not travel with the volume.** The limit is a value written into a
  cgroup on one node. A pod that reschedules is capped again by the new node
  when that node runs `NodePublishVolume`, and is unthrottled on the new node
  until then.
- **A pod restart re-applies it.** The kubelet creates a fresh pod cgroup and
  calls `NodePublishVolume` again, and the limit is written again.
- **It caps the pod, not the volume.** Two containers in the same pod share one
  cgroup and therefore share one budget. If two _different_ pods somehow use the
  same volume on the same node (`ReadWriteMany` block, or a shared filesystem
  volume), each gets its own full budget — the sum is not capped.
- **It is best-effort.** If the limit cannot be applied — no pod UID, an
  unresolvable device, a cgroup the driver cannot find, a cgroup with the io
  controller disabled — the node **logs a warning and the pod starts anyway**. A
  missing throttle is a performance problem; a failed mount is an outage.
  Grep the node plugin's log for `per-volume I/O limit not applied` if a cap
  appears to do nothing.

## Parameters

Set them as StorageClass `parameters`. They travel to the node in the volume
context and are read at `NodePublishVolume`.

| Parameter             | Unit  | Meaning                                      |
| --------------------- | ----- | -------------------------------------------- |
| `bandwidthLimit`      | B/s   | Caps reads and writes alike                  |
| `readBandwidthLimit`  | B/s   | Caps reads; overrides `bandwidthLimit`       |
| `writeBandwidthLimit` | B/s   | Caps writes; overrides `bandwidthLimit`      |
| `iopsLimit`           | ops/s | Caps read and write operations alike         |
| `readIOPSLimit`       | ops/s | Caps read operations; overrides `iopsLimit`  |
| `writeIOPSLimit`      | ops/s | Caps write operations; overrides `iopsLimit` |

An empty value, `"0"` and `"max"` all mean **no limit**, so a cap can be turned
off without deleting the line.

> **They must reach the PersistentVolume to reach the node.** The node plugin
> reads these keys out of the volume context, which the kubelet fills from the
> PV's `csi.volumeAttributes`. For a **statically provisioned** PV that means
> writing them into `spec.csi.volumeAttributes` yourself, and they work today.
> For a **dynamically provisioned** volume the attributes are whatever
> `CreateVolume` returned in its volume context, so a limit only reaches the node
> once the controller echoes these StorageClass parameters into it. A limit that
> never arrives is indistinguishable from no limit, so verify with
> `kubectl get pv <name> -o jsonpath='{.spec.csi.volumeAttributes}'` before
> believing a cap is in force.

### On the names and the units

Bandwidth is **bytes per second**, written as a Kubernetes quantity: `100Mi` is
104857600 B/s, `100M` is 100000000 B/s, and a bare number is bytes. PowerFlex's
`bandwidthLimitInKbps` is the obvious precedent and was deliberately not copied:
putting the unit in the _name_ forces every reader to do a conversion in their
head, and revives the kilobit-versus-kilobyte ambiguity. A Kubernetes user
already writes `10Gi` for storage a few lines above in the same StorageClass, so
these values are spelled the same way.

Only whole numbers are accepted. `1.5Gi` is rejected rather than rounded — this
value becomes a kernel throttle, and a fraction should be caught at the first
pod start rather than discovered by measuring the result.

The undirected keys exist because the common ask is "this volume gets at most
100 MB/s". The directional keys exist because a database that must not swamp the
array on flush usually wants a tighter write cap than read cap.

### Example

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-iscsi-throttled
provisioner: csi.truenas.watteel.com
parameters:
  backend: nas1
  protocol: iscsi
  pool: tank
  parentDataset: tank/k8s
  fsType: ext4
  # At most 100 MiB/s in either direction, and at most 500 write ops/s.
  bandwidthLimit: "100Mi"
  iopsLimit: "2000"
  writeIOPSLimit: "500"
reclaimPolicy: Delete
allowVolumeExpansion: true
```

The resulting line, for a device with major:minor 8:16, is exactly:

```
8:16 rbps=104857600 wbps=104857600 riops=2000 wiops=500
```

All four keys are always written, `max` included, so a relaxed limit **clears**
the one it replaces — a line that omits a key would leave that key's previous
value in force.

## On NFS and SMB

A limit on an `nfs` or `smb` StorageClass makes `NodePublishVolume` **fail**,
before anything is mounted, with a message naming the parameter and the reason.

This is a deliberate choice of error over warning. The friendlier option puts a
line in the node plugin's log — which is precisely where nobody is looking — and
leaves the user running a workload they believe is capped. The failure mode is
discovering during an incident that the isolation you configured never existed.
An error costs a pod that will not start, and it can only ever fire for someone
who has just added the parameter, since no StorageClass that predates this
feature carries it.

The fix is either to remove the limit or to use a block protocol.

## How the cgroup is found

`podInfoOnMount: true` puts the pod's UID in the volume context of
`NodePublishVolume`, which is the only call that knows which pod a volume is
for. The pod's cgroup directory is then resolved under the host's
`/sys/fs/cgroup`.

There is no single path to hardcode. The layout is the product of the kubelet's
cgroup **driver**, the pod's **QoS class**, and whatever the distribution nested
`kubepods` under. All of these are the same pod:

```
systemd,  guaranteed:  kubepods.slice/kubepods-pod<uid>.slice
systemd,  burstable:   kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice
systemd,  besteffort:  kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod<uid>.slice
cgroupfs, guaranteed:  kubepods/pod<uid>
cgroupfs, burstable:   kubepods/burstable/pod<uid>
cgroupfs, besteffort:  kubepods/besteffort/pod<uid>
```

Note that with the systemd driver the UID's dashes become underscores, and that
a **guaranteed** pod has no QoS segment at all — a resolver that assumed one
would pass every test (where everything is burstable) and fail exactly for the
pods someone cared enough to give guaranteed QoS.

So the driver tries the six known layouts as direct paths — one `stat` on a real
node — and falls back to a bounded search for a directory whose name ends in
`pod<uid>` in either spelling. The search is what catches a distribution that
nests the whole tree somewhere else, such as a kubelet running as a systemd
service:

```
system.slice/k3s.service/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice
```

It is depth-bounded so that it stops at the pod level instead of descending into
the per-container cgroups below every pod on a busy node.

## Requirements

- **cgroup v2 (unified hierarchy).** Check with
  `stat -fc %T /sys/fs/cgroup` — it must print `cgroup2fs`. There is no cgroup v1
  fallback: v1's `blkio.throttle.*` is a different interface with different
  semantics, and v1 is on its way out.
- **The `io` controller enabled** for the pod's cgroup, i.e. `io.max` exists in
  it. If it does not, the write fails and is logged; the driver never creates the
  file, because a regular file named `io.max` sitting in a cgroup directory
  would look like a limit in force while doing nothing.
- **A block protocol.** `iscsi` or `nvme`.

Which cgroup driver is in use is visible from the tree itself:
`/sys/fs/cgroup/kubepods.slice` means systemd, `/sys/fs/cgroup/kubepods` means
cgroupfs.

## Cleanup

There is none, deliberately, and no `NodeUnpublishVolume` path that pretends
otherwise. `io.max` is a file inside the pod's cgroup directory, and the kubelet
removes that entire directory when the pod's sandbox is torn down; the value
cannot outlive the pod. Clearing it at unpublish would be writing to something
about to be unlinked — and would be actively wrong while a pod is merely
restarting a container inside a cgroup that stays put.

## What you still cannot control

- Anything on **NFS or SMB**.
- **Aggregate** load on the appliance, from this cluster or any other.
- Latency, queue depth, or priority. `io.max` is a hard ceiling on rate, not a
  scheduler — there is no weighting or borrowing between pods. (cgroup v2's
  `io.latency` and `io.cost` exist and are not used here; they need a global
  view of the device that a per-volume driver does not have.)
- Throughput seen by the **host** or by another storage driver on the same node.
  The cap is written on the pod's cgroup, so only that pod's traffic to that
  device is counted.
- The **write-back** path for a buffered filesystem volume. `blk-throttle`
  accounts I/O where it is submitted to the device, and asynchronous write-back
  is submitted by kernel threads whose cgroup attribution depends on memory
  cgroup ownership of the page. Direct I/O and reads are capped precisely;
  buffered writes are capped less precisely, and a burst can exceed the cap
  briefly before back-pressure arrives. Use `direct=1` if you are measuring the
  cap.
