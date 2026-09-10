# Metrics

The driver exports Prometheus metrics on `metricsAddr` (`:9090` by default) from
both the controller and the node plugin. Everything below is served from local
state or a cached poll: **a scrape never reaches the appliance**, so a slow or
dead TrueNAS cannot stall — or time out — a scrape of the driver.

## Per-volume performance

### Why these are measured on the node

Dell's CSM polls the array for per-volume IOPS, bandwidth and latency. TrueNAS
cannot answer that question. Verified against the appliance on 2026-09-08,
`reporting.netdata_graphs` returns exactly 40 graphs — CPU, memory, `disk` (per
_physical device_), interfaces, load, uptime, ~24 ARC/L2ARC counters, disk
temperature and UPS — and **none of them is per-dataset, per-zvol or per-pool
I/O**. `reporting.realtime` does not exist. See
`.procoder/notes/truenas-api-findings.md`.

The data does exist on the node, where the volume is used:

| Protocol    | Source                  | Operations | Bytes                | Latency                 | Utilisation |
| ----------- | ----------------------- | ---------- | -------------------- | ----------------------- | ----------- |
| iSCSI, NVMe | `/proc/diskstats`       | yes        | sectors × 512        | device service time     | `io_ticks`  |
| NFS         | `/proc/self/mountstats` | yes        | server (wire) bytes  | per-operation RPC RTT   | —           |
| SMB (cifs)  | `/proc/self/mountstats` | —          | client byte counters | — (cifs publishes none) | —           |

What this buys: the measurement **costs the appliance nothing**. Dell's design
polls every array every 10–20 seconds and needed a concurrency cap after their
own bug report about session exhaustion; this reads three files on the node.

### The two limitations, stated plainly

1. **Only mounted volumes are visible.** A volume that is provisioned, bound or
   even attached but not mounted reports nothing at all. An array-side view
   would still describe it; this one cannot see a volume no kernel is using.
2. **A volume's series moves between node exporters when its pod reschedules.**
   The counters on the new node are that node's kernel counters, starting from
   zero, so the move looks like a counter reset on a new series. `rate()`
   handles the reset. Aggregate with `sum(rate(...))`, never `rate(sum(...))`.

A third, smaller one: for NFS the byte counters are the **server** columns, so a
read served from the node's page cache moves no exported counter. That is the
right answer for "how hard is this workload hitting the NAS" and the wrong one
for "how much I/O is this pod doing".

### The metrics

All are counters carrying the kernel's own cumulative value. Nothing is averaged
or smoothed by the exporter: a gauge of "current IOPS" would pick an averaging
window nobody asked for and throw away everything between two scrapes.

| Metric                                        | Meaning                                                             |
| --------------------------------------------- | ------------------------------------------------------------------- |
| `truenas_csi_volume_read_ops_total`           | Read operations completed                                           |
| `truenas_csi_volume_write_ops_total`          | Write operations completed                                          |
| `truenas_csi_volume_read_bytes_total`         | Bytes read                                                          |
| `truenas_csi_volume_write_bytes_total`        | Bytes written                                                       |
| `truenas_csi_volume_read_seconds_total`       | Cumulative time spent on reads (service time, or RPC RTT)           |
| `truenas_csi_volume_write_seconds_total`      | Cumulative time spent on writes                                     |
| `truenas_csi_volume_io_busy_seconds_total`    | Cumulative time with I/O in flight (`io_ticks`); block only         |
| `truenas_csi_volume_io_sample_failures_total` | Scrapes that could not read a staged volume's counters, by protocol |

A metric a protocol cannot answer is **absent**, not zero: a flat zero counter
reads as "idle", which is a different claim from "not measured".

Latency is deliberately two counters rather than one ratio. Divide them over the
window you care about:

```promql
sum by (namespace, persistentvolume) (rate(truenas_csi_volume_read_seconds_total[5m]))
/
sum by (namespace, persistentvolume) (rate(truenas_csi_volume_read_ops_total[5m]))
```

This is how `node_exporter` models the same `/proc/diskstats` fields, and it is
correct across any scrape interval, where a pre-divided average is not.

### Labels, and where each one actually comes from

`volume_id_hash`, `persistentvolume`, `pvc`, `namespace`, `pod`, `protocol`.

- **`persistentvolume`** — the last component of the CSI volume handle, which is
  the PV name the dataset was provisioned under.
- **`namespace`** and **`pod`** — the volume context of `NodePublishVolume`,
  filled in by the kubelet because the CSIDriver sets `podInfoOnMount: true`. A
  pod may only mount a claim from its own namespace, so `namespace` **is** the
  claim's namespace.
- **`pvc`** — the claim name. Kubernetes passes it to _CreateVolume_ on the
  controller and **not** to the node, so the controller echoes it into the
  volume context, which the kubelet then hands back at `NodePublishVolume`. The
  node plugin deliberately talks to neither the appliance nor the API server, so
  it never looks the claim up — that would mean giving a privileged DaemonSet on
  every node credentials it does not otherwise need.

  It is empty for a volume provisioned before this echo existed, and for a
  hand-written PersistentVolume whose `csi.volumeAttributes` does not carry
  `csi.storage.k8s.io/pvc/name`. Both keep working; only the label is missing.
  Recover the claim name for those by joining on `persistentvolume` against
  kube-state-metrics:

  ```promql
  sum by (namespace, persistentvolume) (rate(truenas_csi_volume_read_bytes_total[5m]))
  * on (persistentvolume) group_left(claim_namespace, claim_name)
  kube_persistentvolume_claim_ref
  ```

  or record it once:

  ```yaml
  - record: truenas_csi:volume_read_bytes:rate5m
    expr: |
      sum by (namespace, persistentvolume, pod) (rate(truenas_csi_volume_read_bytes_total[5m]))
      * on (persistentvolume) group_left(claim_name)
      kube_persistentvolume_claim_ref
  ```

- **`volume_id_hash`** — twelve hex characters of SHA-256 over the volume handle,
  the same form `truenas_csi_volume_health` uses, so the two join.

**Cardinality** is bounded by the number of volumes currently _staged_ on the
node — at most `MaxVolumesPerNode` (128) — because a series exists only while
the node holds a staged volume, and `NodeUnstageVolume` retires it immediately.
The metrics are const metrics built from the live volume set on every scrape, so
an unstaged volume cannot leave a stale series behind.

## Array metrics (controller)

Collected from the appliance on a background poll (`metricsInterval`, 60s by
default) and served from cache: `truenas_pool_size_bytes`,
`truenas_pool_free_bytes`, `truenas_pool_used_bytes`, `truenas_pool_healthy`,
`truenas_dataset_used_bytes`, `truenas_dataset_quota_bytes`,
`truenas_iscsi_sessions`, `truenas_collection_duration_seconds`,
`truenas_collection_errors_total`.

Appliance self-report, from the same poll: `truenas_appliance_disk_healthy`,
`truenas_appliance_scrub_state`, `truenas_appliance_scrub_errors`,
`truenas_appliance_alerts`, `truenas_appliance_pool_healthy`,
`truenas_appliance_pool_size_bytes`, `truenas_appliance_pool_free_bytes`,
`truenas_appliance_pool_fragmentation_percent`.

`truenas_appliance_disk_healthy` is the one worth an alert rule. TrueNAS 25.10
exposes no SMART API at all, so an appliance alert naming a disk's serial is the
only warning anyone gets that a disk is failing; the driver joins those alerts
onto the disk inventory and drops this gauge to 0 for the disk they name.

The three pool byte counts are RAW, which is what `zpool list` and the
appliance's own pool view show. On a RAIDZ pool that includes parity and is
therefore larger than the data the pool can hold — a 12-disk RAIDZ2 measured
41.05 TiB raw free against 30.33 TiB writable. Alert on them for the health of
the pool itself. For "can another volume be provisioned", the answer is the
driver's `CSIStorageCapacity`, which is measured in writable bytes and already
excludes the operator's reserve, so it is deliberately the smaller number.

## Driver and node health

`truenas_csi_calls_total`, `truenas_csi_call_duration_seconds`,
`truenas_csi_middleware_calls_total`, `truenas_csi_middleware_duration_seconds`,
`truenas_csi_backend_up`, `truenas_csi_orphaned_volumes`,
`truenas_csi_volume_health`, `truenas_csi_node_backend_reachable`.

## Scraping and the dashboard

```yaml
metrics:
  podMonitor:
    enabled: true
    labels:
      release: kube-prometheus-stack # whatever your Prometheus selects on
  dashboards:
    enabled: true
    labels:
      grafana_dashboard: "1" # whatever your Grafana sidecar watches
```

A **PodMonitor**, not a ServiceMonitor: the per-volume series live on the node
DaemonSet, and a Service in front of it would load-balance scrapes so that each
one returned one arbitrary node's volumes. The node PodMonitor relabels
`__meta_kubernetes_pod_node_name` to `node`, which is what makes a volume's
series moving between exporters legible rather than mysterious.

The dashboard (`deploy/helm/truenas-csi/dashboards/truenas-csi-volumes.json`)
templates on namespace, PersistentVolume and PVC, and carries read/write IOPS,
bandwidth and latency per volume, block device utilisation, and — from the
controller's array metrics — `predict_linear` capacity-exhaustion panels for
pools and for dataset quotas.

## What you still cannot see

- A volume nobody has mounted: no series at all, not a zero one.
- The appliance's own view of a volume — queue depth on the zvol, ARC hit rate
  for that dataset, or which volume is responsible for the pool's write load.
  Those figures do not exist in the TrueNAS API.
- Continuous history for a volume across a reschedule, as one series. The
  counters are per node and start again on the new one.
- Latency for SMB volumes, and operation counts for SMB on current kernels: the
  cifs client publishes byte counters only.
