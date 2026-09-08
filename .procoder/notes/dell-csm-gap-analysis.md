# Dell CSM vs truenas-csi — capability gap analysis

Date: 2026-09-08. Dell side read from repo HEADs (drivers v2.17.x, CSM 1.16.2 docs,
csm-operator 1.12.1) plus csm-docs; our side read from this repo's source, not from
memory. Facts I could not confirm are marked as such rather than smoothed over.

Dell CSM is a five-array enterprise suite (PowerStore, PowerFlex, PowerScale,
PowerMax, Unity XT). A one-to-one comparison is not meaningful — half of what CSM
does exists because Unisphere, SRDF and the SDC kernel module exist. This document
compares only what _translates_ to a single-appliance ZFS NAS, and says plainly
where a Dell capability has no TrueNAS analogue.

---

## 1. CSI specification surface

### Controller service

| Capability                   | Dell (best of 5)     | truenas-csi         | Note                                                |
| ---------------------------- | -------------------- | ------------------- | --------------------------------------------------- |
| CREATE_DELETE_VOLUME         | all 5                | yes                 |                                                     |
| CREATE_DELETE_SNAPSHOT       | all 5                | yes                 |                                                     |
| LIST_SNAPSHOTS               | 4 of 5               | yes                 |                                                     |
| LIST_VOLUMES                 | 3 of 5               | yes                 |                                                     |
| CLONE_VOLUME                 | all 5                | yes                 |                                                     |
| EXPAND_VOLUME                | all 5                | yes                 |                                                     |
| GET_CAPACITY                 | all 5                | yes                 |                                                     |
| PUBLISH_UNPUBLISH_VOLUME     | all 5                | **no**              | see §4.1 — this is the fencing gap                  |
| SINGLE_NODE_MULTI_WRITER     | all 5                | **no**              | accepted in `supportsAccessMode` but not advertised |
| GET_VOLUME                   | all 5 (config-gated) | **no**              |                                                     |
| VOLUME_CONDITION             | all 5 (config-gated) | **no** (controller) | we have it node-side only                           |
| LIST_VOLUMES_PUBLISHED_NODES | 2 of 5               | **no**              |                                                     |
| PUBLISH_READONLY             | Unity only           | no                  |                                                     |
| MODIFY_VOLUME                | **none**             | no                  | Dell ships the resizer feature gate but no impl     |

`internal/csi/controller.go` / `internal/csi/identity.go`.

### Node service

| Capability                           | Dell                | truenas-csi        |
| ------------------------------------ | ------------------- | ------------------ |
| STAGE_UNSTAGE_VOLUME                 | 4 of 5              | yes                |
| EXPAND_VOLUME                        | 4 of 5              | yes                |
| GET_VOLUME_STATS                     | all 5, config-gated | yes, **always on** |
| VOLUME_CONDITION / GET_VOLUME_HEALTH | all 5, config-gated | yes, **always on** |
| SINGLE_NODE_MULTI_WRITER             | all 5               | no                 |

### Plugin / group

|                                  | Dell   | truenas-csi                |
| -------------------------------- | ------ | -------------------------- |
| VOLUME_ACCESSIBILITY_CONSTRAINTS | 4 of 5 | yes                        |
| VolumeExpansion ONLINE           | all 5  | yes                        |
| VolumeExpansion OFFLINE          | 3 of 5 | **no**                     |
| GROUP_CONTROLLER_SERVICE         | 3 of 5 | **yes**                    |
| Ephemeral inline volumes         | 4 of 5 | **no** (`Persistent` only) |

**Verdict on the CSI surface: near parity, with four concrete holes** —
`SINGLE_NODE_MULTI_WRITER`, controller-side `GET_VOLUME`/`VOLUME_CONDITION`,
offline expansion, and ephemeral inline volumes. Three of the four are small.
`PUBLISH_UNPUBLISH` is not small; §4.1.

We are ahead of Dell on group snapshots: 3 of their 5 drivers have it, we have it
on a driver that also serves NFS and SMB.

---

## 2. Beyond-spec extensions

Dell publishes four independently versioned proto modules registered as extra gRPC
services on the _same_ CSI socket via their gocsi fork's `RegisterAdditionalServers`
hook: `common` (probe), `podmon` (1 RPC), `replication` (8 RPCs), `migration`
(4 RPCs, PowerMax only).

| Extension                       | Dell                                               | truenas-csi                                  |
| ------------------------------- | -------------------------------------------------- | -------------------------------------------- |
| ValidateVolumeHostConnectivity  | proto, on the CSI socket, controller-side          | **JSON/HTTP, own listener, node-side**       |
| Replication RPCs                | 8, driver-implemented, consumed by csm-replication | in-process controller + CRD, no RPC boundary |
| Migration RPCs                  | `VolumeMigrate`, `ArrayMigrate` — array-side       | **rsync Job between PVCs** — data copy       |
| Volume group snapshot extension | **deleted**, replaced by upstream KEP-3476         | we went straight to KEP-3476                 |

Two of these deserve their own sections.

---

## 3. Where Dell is genuinely ahead

### 3.1 Per-volume performance metrics — our largest observability gap

Dell's `karavi-observability` emits, per volume, per array:
`{powerflex,powerstore,powermax}_volume_{read,write}_{bw_megabytes_per_second,
iops_per_second,latency_milliseconds}` — six gauges per volume, labelled with
`PersistentVolumeName`, `PersistentVolumeClaimName`, `Namespace`, `VolumeID` and
(PowerFlex) the mapped node IDs and IPs. Polled from the array REST API on a
per-family ticker (PowerFlex 10s, PowerStore 20s, PowerMax 300s), pushed OTLP→
OTel Collector→Prometheus, with 12 shipped Grafana dashboards including
`predict_linear` "PVs full in 2 days / 5 days / 1 week" panels.

We emit **zero per-volume performance metrics**. `internal/obs/metrics.go` gives
CSI call counters/histograms, backend up, orphan count, per-volume _health_ (a
boolean), and `internal/arraymetrics/collector.go` gives pool size/free/used/
healthy, dataset used/quota, iSCSI session count. Capacity and liveness, not
performance. A user cannot answer "which PVC is causing the latency" from our
metrics.

**This translates directly.** TrueNAS exposes exactly the right data:
`reporting.get_data` carries per-dataset and per-zvol read/write ops and bytes,
and `pool.dataset.query` gives space. A `truenas_csi_volume_read_iops` family
labelled with PVC/namespace is a bounded piece of work against APIs we already
speak. This is the single highest-value gap in the list.

Worth stealing alongside it: their array-API **rate limiter and circuit breaker**
(`X_CSI_METRICS_ARRAY_RATE_LIMIT`, `..._CB_THRESHOLD`) and metrics **leader
election** so only one controller replica polls. We poll on a 60s ticker with a
16-call in-flight cap and no breaker; given the verified 20-call concurrency
ceiling on the appliance, a breaker is not decoration.

### 3.2 Resiliency: we answer the question, nobody asks it, and we ask it from the wrong side

This is the most serious architectural finding, and it is worse than "we lack a
feature".

Dell's protocol (`karavi-resiliency`, `internal/monitor/controller.go`):

1. Pod carrying `podmon.dellemc.com/driver` goes not-Ready on a tainted/unreachable node.
2. Controller-side podmon calls `ValidateVolumeHostConnectivity` **over the driver's
   controller UDS**, which asks the **array** whether that host is still connected
   and whether I/O was seen in a sample window.
3. If connected-and-not-force-tainted, or I/O in progress, or the RPC errored →
   **abort** (fail-safe: an RPC error returns `connected=true, ios=true`).
4. Otherwise **fence**: `ControllerUnpublishVolume` for every volume, revoking the
   array-side host mapping. Any failure aborts before anything is deleted.
5. Taint the node, delete the VolumeAttachments, force-delete the pod.
6. When the node returns, node-side podmon sees its own taint, waits 4 cycles,
   unpublishes/unstages/unmounts every stale path, checks CRI that no container is
   still running, and only then removes the taint.

Our side:

- `internal/podmon/service.go` implements the _answer_ — and only the answer.
  `grep` for a consumer finds nothing outside `cmd/truenas-csi/main.go`.
- It runs **on the node** (`pm = podmon.New(...)` inside the node-plugin branch),
  binds `127.0.0.1` by default, and is `enabled: false` in the chart.
  A node-side connectivity oracle is unavailable in precisely the scenario it
  exists for: the node is gone.
- `ControllerUnpublishVolume` is a no-op returning success, and we advertise no
  `PUBLISH_UNPUBLISH`, so **we have no fencing primitive at all**.
- Deeper: `internal/backend/iscsi/target.go` uses **one shared target for the whole
  cluster** with a cluster-wide initiator group. Per-node, per-volume access
  revocation is not expressible in the current design. NFS is closer — exports
  carry a `networks` list — but it is per-export, not per-node.

So today: we report volume health and never act on it, and could not act if we
wanted to. That is a defensible v1 position, but it should be stated as such rather
than implied by the presence of a podmon package.

If this is to be closed, the honest translation is:

- move the extension **controller-side**, answering from the appliance
  (`iscsi.global.sessions` for live initiator sessions, `sharing.nfs` client lists,
  `reporting.get_data` for the I/O window) — TrueNAS has all three;
- give iSCSI a per-node initiator group or per-node target so a fence is possible
  at all;
- then, and only then, write the consumer.

One thing we already do better: our I/O detection reads `/proc/diskstats` and
`/proc/self/mountstats` kernel counters (`internal/podmon/iocounters.go`), and
`TestLastIOProbeDoesNotUseModTime` pins that it can never regress to the directory
mtime it used to use. Dell's equivalent is array-side and per-array; the mechanism
is fine on both sides. It is the _placement_ and the _missing consumer_ that are wrong.

### 3.3 Multi-tenancy / authorization — real gap, expensive, arguably not ours

CSM Authorization is an L7 MITM proxy for the array's REST API. The driver is
pointed at a localhost sidecar; the sidecar swaps array Basic-auth for a short-lived
JWT (access ~1 min, refresh 30 days); a proxy-server outside the tenant cluster
holds the real credentials (in Vault), evaluates an OPA policy plus a Redis-backed
per-(systemType, systemID, pool) capacity ledger, and re-issues the request. Denials
come back as HTTP 400 or **507 Insufficient Storage**. Enforcement is
**capacity-only** — no IOPS, no bandwidth — and PowerScale gets nothing at all.

We have none of this. Every cluster with our driver holds a TrueNAS API key with
its full 14-role grant, and nothing bounds how much of a pool one cluster consumes
beyond `reservedBytes`/`reservedPercent`, which protect the _appliance_, not other
tenants.

Assessment: this is a genuine gap and the design is sound, but it is a large
subsystem, Dell's own v2 server is **closed source**, and its whole reason for
existing — "the array has no native RBAC" — is only half true for TrueNAS, which
does have API-key roles. A cheaper 80% for us: per-backend, per-namespace capacity
accounting enforced in `CreateVolume` against a ZFS-quota-backed parent dataset per
tenant. That uses a mechanism ZFS already has and Dell's arrays mostly do not.

### 3.4 QoS

PowerFlex takes `bandwidthLimitInKbps` / `iopsLimit` as StorageClass parameters and
applies them at **ControllerPublishVolume** via `SetMappedSdcLimits` — per
(volume, SDC) mapping, immutable after first publish. PowerMax attaches host I/O
limits to the Storage Group; Unity uses a named `hostIOLimitName` policy; PowerStore
passes an opaque `performance_policy_id`.

We have none. **This mostly does not translate**: OpenZFS has no per-dataset IOPS
or bandwidth limiter, and TrueNAS SCALE exposes no such API. The honest answer is
"not available on this platform", not "not implemented". What _does_ translate and
we already do: `refquota` as a hard capacity limit — the equivalent of PowerScale's
SmartQuotas, which Dell treats as a headline feature.

### 3.5 Credential hot-reload

PowerStore detects an edited array secret after the kubelet remount (~1 min) and
switches credentials with **no restart**, with a documented list of fields that
still require one (`endpoint`, `globalID`, `blockProtocol`, adding an array). We
read `/etc/truenas-csi/config.yaml` at startup and never again; rotating a TrueNAS
API key means a rollout.

Small, well-defined, and worth doing — an fsnotify watch on the mounted Secret plus
a re-dial. Given that this project has already destroyed three API keys, credential
rotation being a disruptive operation is a bad property.

### 3.6 Smaller Dell-side wins

- **Dynamic log level via a watched ConfigMap** (viper + fsnotify, re-read at
  runtime). Ours is a startup flag. Cheap; genuinely useful in an incident.
- **Multi-array from one driver instance** with a `default: true` array and
  per-array protocol. We have multi-backend via the `backend` StorageClass
  parameter — parity — but no default-backend fallback, so every StorageClass must
  name one.
- **Documented rounding/minimum-size semantics** (PowerFlex: 8 GiB minimum, round up
  to multiples of 8 GiB). We should document zvol `volblocksize` rounding the same way.
- **Version-skew refusal in the operator.** csm-operator ships `upgrade-path.yaml`
  per driver version with a `minUpgradePath`, and `IsValidUpgrade` _rejects_ a CR
  edit that jumps further than the declared window. Our operator does not gate
  upgrades. This is a good idea and cheap to add.
- **Digest-pinned image bill of materials** per release. Our chart pins sidecars by
  tag, not digest.

### 3.7 PVC identity is not written to the appliance

Dell's `csi-metadata-retriever` reads `pvc.Labels` and merges them into
`CreateVolume` parameters, so PowerStore stamps array-side attributes from the
PVC and defaults every volume's array description to `<pvcName>-<pvcNamespace>`
(`csi-powerstore/pkg/controller/base.go`). PowerScale drives per-PVC quota soft
limits the same way; PowerFlex uses it for a per-PVC fsck override. An operator
looking at the array sees which workload owns each object.

We already pass `--extra-create-metadata` to the provisioner
(`deploy/helm/truenas-csi/templates/controller.yaml:138`), so
`csi.storage.k8s.io/pvc/name` and `/pvc/namespace` **arrive in `CreateVolume`
parameters — and we discard them**. We set only `io.truenas.csi:managed` and
`io.truenas.csi:protocol`.

Writing `io.truenas.csi:pvc` and `io.truenas.csi:namespace` as ZFS user
properties is a handful of lines and makes the TrueNAS UI usable for someone
debugging a cluster. Note the caveat their design has and ours would not:
Dell's is a _label_ merge into StorageClass parameters, so a relabelled PVC
silently changes array behaviour. Recording identity for diagnosis is the safe
subset.

### 3.8 Certification harness

Dell shipped `cert-csi`: 15 performance suites (provisioning, scaling, volume
I/O, snapshot, clone, multi-attach RWX, expansion, health metrics, block snap,
migration, ephemeral, pgbench) and 7 functional suites (volume/pod/clone/snapshot
deletion, node drain, node uncordon, capacity tracking), timing seven named
stages (`PVCBind`, `PVCAttachment`, `PVCCreation`, `PVCDeletion`,
`PVCUnattachment`, `PodCreation`, `PodDeletion`) into per-StorageClass SQLite
plus HTML/XML reports and gonum plots, with a longevity mode and a formal
submission process for "Community Qualified Configuration".

It was **archived 2025-11-05 and deprecated at CSM 1.17**, with the stated
replacement being upstream Kubernetes E2E / OpenShift E2E — which `cert-csi`
itself already wrapped (`k8s-e2e` subcommand).

Our position: csi-sanity conformance, `test/integration` against the live
appliance, `test/chart` install/upgrade on the live cluster. We have no
timing/longevity harness. Given Dell just retired theirs in favour of upstream
E2E, the right move is **upstream external-storage E2E with a driver-config
manifest**, not a bespoke harness. Worth doing; low urgency.

---

## 4. Where we are ahead

### 4.1 Liveness and readiness probes

`grep -rn 'livenessProbe|readinessProbe' helm-charts/charts/` across **all five**
Dell driver charts returns exactly one hit — in the bundled Redis of the
Authorization chart. No Dell CSI driver chart deploys the `csi-liveness-probe`
sidecar or defines a probe on any container.

We deploy `livenessprobe` on both controller and node and define `livenessProbe`
blocks on both (`deploy/helm/truenas-csi/templates/{controller,node}.yaml`). This is
a straightforward correctness win for us against the entire suite.

### 4.2 Test failover that actually exists

Dell's proto defines `TEST_FAILOVER` and `TEST_FAILOVER_STOP`, but they are **absent
from `repctl`'s command surface** — there is no way for an operator to invoke them.

We implement `ActionTestFailover` / `ActionStopTestFailover`: clone the last
replicated snapshot into scratch datasets, never touching production, and destroy
the clones on stop (`internal/replication/v1alpha1/types.go`). ZFS clones make this
nearly free; on Dell's arrays it evidently was not.

We also track `FailoverSnapshot` per volume and **measure divergence against it on
failback** — Dell's `ACTION_FAILBACK_DISCARD_CHANGES_*` exists but the controller has
a literal `// TODO: Verify if the Remote PV matches with the one we are expecting`.

### 4.3 Ownership guards on destructive operations

Every destructive path of ours checks the `io.truenas.csi:managed` user property with
`source == LOCAL` before acting, and replication refuses a group containing a dataset
we did not create. Dell's equivalents are weaker: csm-replication compares only the
cluster ID on an existing remote PV; csm-sharednfs's `DeleteExport` removes every
line in `/etc/exports` matching a string prefix.

### 4.4 Four protocols in one driver

PowerScale is NFS-only, PowerFlex is SDC + NVMe/TCP, Unity has no NVMe. We serve
NFS, iSCSI, SMB and NVMe/TCP from one binary with a registry-based backend
interface. **SMB has no analogue anywhere in CSM.**

### 4.5 Shared NFS: the gap that isn't

`csm-sharednfs` gave block arrays RWX by publishing a LUN to one node, mounting it,
re-exporting it over kernel NFSv4, and pointing every other node at a per-volume
ClusterIP. It is **archived (2025-09-17)**, shipped one release, was PowerStore-only,
was subsequently _removed_ from PowerStore as dead weight, and was never documented.
Its code carries scaffolding strings (`Name: "your-plugin-name"`), a no-op
`DeleteVolume`, a `PVLock` that is an in-process `sync.Map` (so a second controller
replica breaks it), and a `podCIDR` fallback that exports read-write with
`no_root_squash` to a `/8`.

We do not need it: TrueNAS serves NFS and SMB natively, so RWX is a first-class
protocol choice rather than a re-export hack. **Not a gap.**

### 4.6 Blast radius and scope discipline

Dell's controller ClusterRole grants cluster-wide `configmaps: [create,delete,...]`
and `apiextensions.k8s.io/customresourcedefinitions: [create, delete]` — the driver
can delete CRDs. Their sidecars run `--v=5` hardcoded regardless of `logLevel`.
Their node pods mount `/`, `/usr/bin`, `/dev`, `/sys`, `/run` with
`allowPrivilegeEscalation: true`.

Ours is not clean either — `hostPID: true`, `hostNetwork: true`, `privileged: true`,
`/host` mounted `Bidirectional` — because chroot-ing the host's mount binaries
requires it. But we do not grant CRD deletion, and our sidecar verbosity follows
`logLevel`.

---

## 5. Gaps that do not translate

State these once so they stop being counted as gaps:

- **Per-volume IOPS/bandwidth QoS** — OpenZFS has no such limiter.
- **Array-side `VolumeMigrate` / `ArrayMigrate`** — needs two arrays and an array-side
  mover. Our rsync-Job migration is a different, weaker thing, correctly labelled.
- **PowerMax reverse proxy** — a request-queueing tier for Unisphere. Our equivalent
  is the in-client `MaxInFlight=16` cap against the verified 20-call ceiling; a
  separate proxy tier would be over-engineering for one appliance.
- **SDC kernel-module lifecycle management** — PowerFlex-specific.
- **Multi-cluster replication (paired clusters, mirrored PV/PVC, remote kubeconfig
  Secrets)** — this one _could_ translate, and we deliberately did not build it.
  Our replication is single-cluster: it moves ZFS data and flips primacy, but does
  not create the target-side PV in another cluster. Note that Dell's own PVC remap
  on failover **only works when `remoteClusterID == "self"`**, i.e. single-cluster,
  so the gap is narrower than the feature list suggests.

---

## 6. Ranked recommendations

1. **Per-volume performance metrics** from `reporting.get_data`, labelled with
   PVC/namespace, plus a Grafana dashboard. Largest real gap; data already available.
2. **Decide and document the resiliency position.** Either (a) declare podmon an
   answer-only extension and say so in the README, or (b) build it properly:
   controller-side, appliance-backed, per-node iSCSI initiator groups so a fence is
   possible, then a consumer. Do not leave it looking like (b) while being (a).
3. **Credential hot-reload** — fsnotify on the mounted Secret, re-dial. Small, and
   this project has a history with API keys.
4. **Array-API circuit breaker + metrics leader election** — the 20-call ceiling
   makes an unbreakered poller a real hazard.
5. **Advertise `SINGLE_NODE_MULTI_WRITER`** — already honoured in
   `supportsAccessMode`, just not advertised.
6. **Upgrade-path gating in the operator** — copy `minUpgradePath` + refusal.
   (Note their operator does _not_ do drain-aware node rollout or credential
   rotation, contrary to how it is often described: server-side apply onto the
   DaemonSet with the default `RollingUpdate`, and no watch on `authSecret`.)
7. **Controller-side `GET_VOLUME` / `VOLUME_CONDITION`** — we already have the health
   data node-side; surfacing it controller-side closes the last CSI-surface hole
   that matters.
8. **Dynamic log level via watched ConfigMap.**
9. **Record PVC name/namespace as ZFS user properties** — the metadata already
   reaches `CreateVolume` and is thrown away (§3.7).
10. Multi-tenant capacity accounting via per-namespace parent datasets with ZFS
    quotas — the cheap 80% of Authorization, using a mechanism ZFS already has.

Not recommended: shared-NFS-style RWX-on-block (archived, and we serve NFS natively);
per-volume QoS (no platform support); a reverse-proxy tier; the full Authorization
proxy architecture.
