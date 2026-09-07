# Troubleshooting

Each entry is a symptom you can observe, the cause behind it, and the fix. Start by finding
the symptom; the driver's failure modes look alike from the outside (a `Pending` PVC) but
have very different causes.

Useful first commands:

```sh
kubectl -n truenas-csi logs deploy/truenas-csi-controller -c truenas-csi
kubectl -n truenas-csi logs ds/truenas-csi-node -c truenas-csi --all-pods
kubectl describe pvc <name>            # provisioning errors surface as events here
kubectl describe pv  <name>            # volume id: <backend>/<protocol>/<pool>/<parent>/<name>
```

Every log line about a volume carries its volume ID, so one failing PVC can be traced from
the controller through the node plugin. `truenas_csi_backend_up` is `0` for any appliance
the driver cannot currently reach.

---

## "Invalid API key" on every call, after one bad connection

**Symptom.** Provisioning worked, or was never tried, and now _every_ CSI operation fails
with an authentication error. The controller logs an authentication failure and exits
instead of retrying. Re-entering the same key does not help. On the appliance, the key is
listed as revoked.

**Cause.** The key was **revoked by TrueNAS because it was presented over a plaintext
connection** — a plaintext `ws:` or `http:` scheme instead of `wss://`. TrueNAS 25.10 treats a `_PLAIN`
authentication mechanism on an insecure transport as a compromise of the credential: it
does not refuse the login, it **destroys the key**. The appliance reports "API key revoked
due to insecure transport". One connection is enough. The typical trigger is a
plaintext-scheme endpoint in values, a proxy or ingress in front of the appliance that
terminates TLS and forwards plaintext, or a debugging session done by hand with a plain
websocket client.

**Fix.**

1. Issue a **new** API key on TrueNAS. The old one is gone; nothing brings it back.
2. Correct the endpoint to `wss://<appliance>/api/current` and make sure nothing between
   the driver and the appliance downgrades the connection.
3. Update the Secret and restart the controller.

The driver rejects a non-`wss` endpoint at configuration validation, before any socket is
opened, so a plaintext endpoint in `values.yaml` shows up as a refusal to start rather than
as a dead key. That guard only covers the driver's own connections — a key you also test by
hand over a plaintext scheme is destroyed just the same.

---

## The controller exits on an authentication failure instead of retrying

**Symptom.** The controller pod logs one authentication failure, loudly, and terminates.
There is no retry loop, no backoff, and no second login attempt in the logs.

**Cause.** This is by design, and it is a safety property rather than a limitation.
**Retrying a login can revoke the key.** An authentication failure may mean the credential
is already compromised or wrong; a naive reconnect loop hammering the appliance with a bad
or plaintext-exposed credential is exactly how a replacement key gets destroyed too. So
authentication failure is terminal: log the likely cause and exit.

Connection failures are a different case and _are_ retried with exponential backoff. While
a backend is unreachable, CSI calls return `UNAVAILABLE` so the sidecars retry, and
**existing mounts are unaffected** — they are kernel-level NFS and iSCSI connections that do
not involve the middleware API. State changes are logged and reflected in
`truenas_csi_backend_up`, not one line per attempt.

**Fix.** Correct the credential (username, API key, roles), confirm the endpoint is `wss://`,
update the Secret, then let the pod restart. If the key was revoked, see the entry above.

---

## JSON-RPC error `-32000`, "too many concurrent calls"

**Symptom.** Under a burst of provisioning — a StatefulSet scaling out, or a batch of PVCs
created at once — logs show middleware errors with JSON-RPC code `-32000` and no error
`data`. Operations still complete after a retry.

**Cause.** The appliance accepts **20 in-flight calls per connection** (measured: 16
concurrent is clean; 32 concurrent yields 20 successes and 12 errors). Excess calls are
rejected with `-32000` carrying no structured error payload. The driver holds a bounded
in-flight semaphore capped at **16** per backend, for headroom.

**This is backpressure, not a volume failure.** `-32000` is retried with backoff and is
never surfaced as a failed volume operation. Seeing a few in the logs during a burst means
the ceiling was reached, which is the mechanism working.

**Fix.** Usually none. If it is constant rather than bursty, the appliance is also serving
other API consumers against the same limit; reduce parallel provisioning or spread load
across backends. Do not raise the cap: 20 is the appliance's ceiling, not a tuning knob.

---

## PVC with `fsType: xfs` stuck on a node that has no `xfsprogs`

**Symptom.** The PVC binds, the pod is scheduled, and `NodeStageVolume` fails. The event on
the pod names the missing package, for example:

```
node lacks the tooling this volume requires: xfs needs xfsprogs on this node
```

**Cause.** The node plugin ships **no storage tooling of its own**. At startup it probes the
host for each capability's binaries and modules — for XFS, `mkfs.xfs` and `xfs_growfs` — and
advertises only what it can deliver. A node without `xfsprogs` cannot create an XFS
filesystem, so staging fails immediately with a message naming the package rather than
producing a cryptic `mkfs` or mount error much later.

**Fix.** Either install the package on the node and let the node plugin re-probe (restart
the node pod), or use a `fsType` the node supports (`ext4` needs only `e2fsprogs`, which is
present on essentially every distribution):

```sh
apt-get install -y xfsprogs      # or: nfs-common, open-iscsi, multipath-tools
kubectl -n truenas-csi delete pod -l app.kubernetes.io/component=node --field-selector spec.nodeName=<node>
```

Each node publishes its capabilities as CSI topology segments
(`csi.truenas.watteel.com/xfs=true|false`), so with `volumeBindingMode:
WaitForFirstConsumer` the scheduler avoids incapable nodes in the first place. Check them
with `kubectl get csinode <node> -o yaml`.

Related states, for the same probe:

- **Module present on disk but not loaded** — the plugin `modprobe`s it and continues. That
  is recoverable, not a failure. Nothing is written to `/etc/modules-load.d`, so the load
  does not survive a reboot until the driver loads it again.
- **No multipath tooling** — the one capability that degrades instead of failing: the
  volume attaches on a single path with a warning. Do not install `multipath-tools` casually;
  `multipathd` claims block devices on sight and can seize devices already in use by another
  iSCSI consumer on the node unless `/etc/multipath.conf` blacklists them first.

---

## A pod cannot write to its NFS volume as a non-root user

**Symptom.** An NFS PVC mounts, but a container running as uid 1000 gets `Permission
denied` on the first write. Adding `fsGroup` to the pod's `securityContext` changes nothing.

**Cause.** A freshly created ZFS dataset is `root:root` with mode `0755`, so a non-root
process cannot write to it. **`fsGroup` does not fix this**: the kubelet deliberately skips
`fsGroup` ownership changes for NFS volumes, because recursively chowning a network export
is unsafe and unbounded. This is the single most common NFS-CSI failure mode, and it is not
specific to this driver.

**Fix.** Ownership and permissions for NFS volumes are set **server-side at provisioning
time**, driven by StorageClass parameters, not by anything Kubernetes does at mount time.
Set them in the StorageClass:

```yaml
parameters:
  protocol: nfs
  mode: "0770"
  uid: "1000"
  gid: "1000"
```

These are applied when the volume is created (via `filesystem.setperm`, the one job-based
middleware call the driver makes). They are **not** retroactive: a volume already
provisioned with the defaults keeps its ownership. Fix an existing volume by adjusting the
dataset's permissions on the appliance, and change the StorageClass so new volumes are
correct.

Note that `maproot` on the export and `mode`/`uid`/`gid` on the dataset solve different
halves of the problem: `maproot` decides how the client's root is mapped, the dataset
ownership decides whether a given uid can write at all.

---

## A volume reports the size of the whole pool

**Symptom.** `df` inside a pod shows tens of terabytes for a "10Gi" PVC. Usage alerts never
fire, and `kubectl get --raw` volume stats report pool-wide numbers. A single PVC can fill
the pool.

**Cause.** A ZFS filesystem with no `refquota` reports the pool's free space, because that
is genuinely how much it can grow into. Every filesystem-backed volume therefore needs
`refquota` set to the requested capacity — the driver sets it at creation, and expansion is a
`refquota` update.

The case where this reappears is a **volume restored from a snapshot**. A restored volume is
a ZFS clone, and a clone takes its properties from its position in the hierarchy, not from
its origin: it inherits neither the `refquota` **nor** the `io.truenas.csi:managed` ownership
marker. The driver re-applies both explicitly after cloning. A restored volume showing the
whole pool means that step did not happen, and the same volume is probably also
undeletable — see the next entry.

**Fix.** Check the dataset on the appliance: `refquota` should equal the PVC's requested
size. If it is `none`, set it there, and report the case — a restored volume without its
quota is a driver defect, not a configuration mistake. `NodeGetVolumeStats` reads whatever
the filesystem reports, so correcting `refquota` corrects the reported numbers too.

---

## `DeleteVolume` refuses: the dataset is not owned by the driver

**Symptom.** A PV stays `Released` or `Terminating`; the controller logs a refusal to delete
a dataset because its ownership property is missing or is inherited rather than local. The
dataset is left intact.

**Cause.** The driver deletes a dataset **only** when `io.truenas.csi:managed` is present
_and_ its `source` is `LOCAL`. ZFS user properties are inherited by children, so presence
alone is not proof of ownership — if the configured parent dataset ever carried the marker,
every pre-existing dataset beneath it would look driver-owned. Refusing is the correct
outcome: an orphaned dataset is a cleanup task, a wrongly deleted one is not recoverable.

**Fix.** Look at the dataset on the appliance and decide by hand. If it really is a leftover
of this driver, delete it there and remove the PV. Common causes are a volume restored by an
older build that did not re-stamp the clone, a dataset created by hand under the driver's
parent dataset, or a PV carried over from a different install.

---

## PVC stays `Pending`: the pool is out of capacity

**Symptom.** The PVC never binds. Events on the PVC show either a provisioning error mapping
to `RESOURCE_EXHAUSTED`, or nothing at all from the driver — with `storageCapacity` enabled,
the scheduler simply never places the pod, because no node's storage can satisfy the claim.

**Cause.** The pool is full, or the request cannot be satisfied under the current
provisioning mode:

- **The pool genuinely has less free space than the request.** The driver reports real pool
  free space through `GetCapacity`/`CSIStorageCapacity`, so an oversized PVC stays `Pending`
  rather than failing later at attach — which is the intended behaviour.
- **A thick zvol larger than 80% of the pool's free space is refused by TrueNAS**, even when
  the arithmetic looks like it fits. This only bites with `sparse: "false"`; `sparse` is
  `true` by default. So `sparse` changes failure behaviour, not just space accounting.
- **Space accounted elsewhere.** Snapshots hold blocks their source has deleted, and a
  restored volume's clone shares blocks with its origin until they diverge. Free space can
  fall without any volume growing.

**Fix.** Free space on the pool (delete unneeded volumes, and the snapshots pinning their
blocks), request less, use `sparse: "true"`, or point the StorageClass at a backend with
room. Check the pool from the appliance rather than from `df` inside a pod: a pod sees its
own `refquota`, not the pool.

---

## `DeleteSnapshot` returns `FAILED_PRECONDITION`

**Symptom.** Deleting a `VolumeSnapshot` does not complete; the snapshot controller retries
and the driver returns `FAILED_PRECONDITION`, reporting that volumes still depend on the
snapshot.

**Cause.** A volume restored from a snapshot is a **ZFS clone that remains dependent on that
snapshot** — the snapshot is the clone's origin and ZFS will not destroy it while the clone
exists. The driver does **not** call `pool.dataset.promote` to break the dependency, and
that is a deliberate decision: promoting inverts the dependency rather than removing it. It
was verified on hardware that after promoting the clone, the **original** volume becomes the
dependent one and can no longer be deleted. Trading "the snapshot is undeletable" for "the
source volume is undeletable" is strictly worse.

So the dependency is left in place and reported honestly, which is also what the CSI
specification prescribes for this case. Note the asymmetry: **deleting a PVC is never
blocked by a clone** — only deleting the snapshot the clone was restored from is.

**Fix.** Delete the restored volumes first (find them by their `origin` on the appliance, or
by the `dataSource` on the PVCs), then delete the snapshot. If you must keep the restored
volume and drop the snapshot, copy its data into a freshly provisioned volume that has no
origin, then delete the clone and the snapshot.

---

## Snapshots are not available at all

**Symptom.** `VolumeSnapshot` objects are rejected by the API server as an unknown kind, or
the driver starts but advertises no snapshot capability.

**Cause.** The `VolumeSnapshot` CRDs and the snapshot controller are a **cluster-wide
singleton owned by the cluster, not by a CSI driver**. This chart does not install them
(`snapshotter.install: false`) because two drivers installing the snapshot controller fight
over the CRD version and break snapshots for the whole cluster. With the CRDs absent, the
driver logs the situation clearly and runs without snapshot capability rather than
crash-looping.

**Fix.** Install [external-snapshotter](https://github.com/kubernetes-csi/external-snapshotter)
once, cluster-wide, then set `snapshotter.enabled: true`.

---

## TLS verification fails on connect

**Symptom.** The controller cannot connect; the log names a certificate verification
failure (unknown authority, or a hostname that does not match).

**Cause.** The stock TrueNAS certificate is self-signed with `CN=localhost` and
`SAN=DNS:localhost`, so it fails verification twice over: the chain is untrusted, and the
SAN cannot match the address in `endpoint`. This is expected on a fresh appliance, not a
misconfiguration to be solved by trusting the system roots.

**Fix.** Give the driver a certificate to trust: install a real certificate on the appliance
with a matching SAN and put its CA in `backends.<name>.caCert`, or pin the self-signed
certificate itself in `caCert`. `insecureSkipVerify: true` is a last resort — it defaults to
`false`, warns on every connect, and lets anything that can intercept the connection collect
the API key. See [security.md](security.md#tls-trust).

---

## A StorageClass names a backend that does not exist

**Symptom.** Provisioning fails immediately with `InvalidArgument`, naming the backend and
listing the configured ones.

**Cause and fix.** The `backend` parameter must match a key in the chart's `backends` map.
The parameter may be omitted only when exactly one backend is configured. A volume ID whose
backend is no longer in the configuration fails the same way, on purpose: acting on the
wrong appliance would be far worse than failing.

The same class of error covers `pool` and `parentDataset`: if a StorageClass states them,
they must **match** the backend's configuration. A mismatch is rejected, never honoured —
they are operator policy, not a redirect.

---

## Expansion is rejected

**Symptom.** Editing a PVC to a smaller size fails; the resizer reports an invalid argument.

**Cause.** ZFS cannot shrink, so expansion is one-way. The driver rejects a shrink on both
backends **itself**, and that matters: TrueNAS refuses to shrink a zvol, but it will
_silently allow_ a `refquota` to be lowered below current usage on a filesystem. Relying on
middleware to guard the NFS path would let a shrink through.

Growing a mounted volume is supported and does not require an unmount, a re-login or a pod
restart: the controller updates the size and the node rescans the device and grows the
filesystem in place.

## Node plugin never registers: "detected topology value collision"

**Symptom.** Pods stay in `ContainerCreating` with
`driver name csi.truenas.watteel.com not found in the list of registered CSI drivers`,
and the `node-driver-registrar` container logs:

```
RegisterPlugin error -- plugin registration failed with err: error updating Node object
with CSI driver node info: detected topology value collision: driver reported
"csi.truenas.watteel.com/iscsi":"true" but existing label is
"csi.truenas.watteel.com/iscsi":"false"
```

**Cause.** The driver publishes each node capability as a topology label, and Kubernetes
treats topology labels as immutable. Once a node has been labelled `iscsi=false`, the
driver cannot later report `iscsi=true` — so installing `open-iscsi` (or `xfsprogs`, or
`multipath-tools`) on a node that the driver has already registered makes registration
fail permanently rather than simply picking up the new capability.

**Fix.** Remove the driver's labels from the affected nodes and let the node plugin
re-register:

```
for n in $(kubectl get nodes -o name | cut -d/ -f2); do
  for cap in nfs iscsi ext4 xfs multipath; do
    kubectl label node "$n" "csi.truenas.watteel.com/$cap-"
  done
done
kubectl rollout restart daemonset/<release>-node -n <namespace>
```

Do **not** delete `CSINode` objects to force this. `kubectl delete csinode` also removes
every _other_ CSI driver's registration on those nodes — Longhorn included — and those
drivers only re-register when their own node plugins restart.

**Prevention.** Install the node packages listed in the README on every node _before_
installing the driver, so a node's capability set does not change afterwards.
