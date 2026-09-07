# Pool administration

The driver can report the TrueNAS appliance's own maintenance surface — pool
health, scrub state, disk health and active alerts — so an operator can see why
storage is unhappy from `kubectl` and Prometheus, without opening the TrueNAS
UI.

It is implemented in `internal/pooladmin`.

## Strictly read-only

This package **never issues a mutating middleware call**. It does not start a
scrub, resilver or SMART test; it does not replace, wipe or offline a disk; it
does not dismiss an alert; it does not touch a dataset or a share.

Managing the pool is the appliance's job. The driver only reports what the
appliance says, so that a bug on the Kubernetes side — a bad reconcile loop, a
mis-parsed status, a retry storm — can never turn into a storage-side action on
somebody's array. There is no flag to turn this off, and adding one would be a
change to the contract, not a feature.

The rule is enforced twice:

1. Every call goes through one funnel, `query()`, which parses the method name
   and refuses anything whose trailing verb is not in a closed read-only set
   (`query`, `list`, `config`, `get_instance`, `results`, `temperatures`,
   `get_disks`). A refused method returns `ErrMutatingCall` and never reaches
   the wire.
2. `TestPoolAdminIsReadOnly` drives every exported function against the fake
   middleware and fails if the appliance saw a single call that is not a read.
   That test is what keeps the package honest: it fails on a mutating call added
   anywhere, including one that bypasses `query()` entirely.

Methods actually called: `pool.query`, `disk.query`, `smart.test.results`,
`alert.list`.

## What it reports

### `PoolStatus(ctx, backend) ([]Pool, error)`

Per pool: `Status` and `Healthy` as the appliance reports them, `SizeBytes`,
`FreeBytes`, a derived `UsedPercent` for capacity trending,
`FragmentationPercent`, and the scrub:

| Field                   | Meaning                                    |
| ----------------------- | ------------------------------------------ |
| `Scrub.Function`        | `SCRUB` or `RESILVER`                      |
| `Scrub.State`           | `SCANNING`, `FINISHED`, `CANCELED`, `NONE` |
| `Scrub.Errors`          | errors found by the last completed scan    |
| `Scrub.PercentComplete` | progress while `SCANNING`                  |
| `Scrub.Start` / `End`   | when the last scan ran                     |

### `DiskHealth(ctx, backend) ([]Disk, error)`

Per disk: device name, serial, model, size, the pool it belongs to, whether
SMART is enabled, and the last SMART test result where the appliance exposes
one.

`smart.test.results` is not available everywhere — TrueNAS CORE, a controller
without SMART passthrough, and NVMe namespaces all legitimately report nothing.
A missing SMART surface is therefore **not** an error: the disk inventory is
still returned, with `SMARTStatus: "UNKNOWN"`. A disk is reported unhealthy only
when the appliance explicitly says `FAILED`; `RUNNING` and "no result" both mean
"nothing is known to be wrong", so an appliance without SMART does not page you
every night.

### `Alerts(ctx, backend) ([]Alert, error)`

The appliance's active alerts from `alert.list`: id, level, class, the
appliance's own formatted text, whether it has been dismissed in the UI, and
when it fired. Dismissed alerts are returned too — dismissal is a UI state, not
a resolution, and hiding them would hide a problem an operator chose to silence
weeks ago.

### `Collect(ctx, backend) (*Diagnostics, error)`

All three, plus publication to the metrics registry. A section that fails is
recorded in `Diagnostics.Errors` rather than returned, so one unavailable
surface does not cost you the two that worked; `Collect` returns an error only
when nothing at all could be read.

## Metrics

| Metric                                         | Labels                     | Meaning                                       |
| ---------------------------------------------- | -------------------------- | --------------------------------------------- |
| `truenas_appliance_pool_healthy`               | `backend`, `pool`          | 1 when the appliance reports the pool healthy |
| `truenas_appliance_pool_size_bytes`            | `backend`, `pool`          | pool size                                     |
| `truenas_appliance_pool_free_bytes`            | `backend`, `pool`          | free space (capacity trend)                   |
| `truenas_appliance_pool_fragmentation_percent` | `backend`, `pool`          | ZFS fragmentation                             |
| `truenas_appliance_scrub_state`                | `backend`, `pool`, `state` | 1 for the current state, 0 for every other    |
| `truenas_appliance_scrub_errors`               | `backend`, `pool`          | errors from the last completed scan           |
| `truenas_appliance_disk_healthy`               | `backend`, `disk`          | 0 only on an explicit SMART `FAILED`          |
| `truenas_appliance_alerts`                     | `backend`, `level`         | active alerts by severity                     |

`scrub_state` publishes the **whole** closed set of states — `NONE`,
`SCANNING`, `FINISHED`, `CANCELED`, `UNKNOWN` — with zeroes for the states the
pool is not in, and `truenas_appliance_alerts` publishes every level even at
zero. Series that disappear when a condition clears break alert rules that ask
"has this been true for N days"; series that go to zero do not.

Label cardinality stays bounded: backend names, pool names and disk device names
are fixed by the hardware, and the `state` and `level` label values come from
closed sets.

Useful rules:

```promql
# no successful scrub visible for this pool
max_over_time(truenas_appliance_scrub_state{state="FINISHED"}[30d]) < 1

# a disk the appliance says has failed SMART
truenas_appliance_disk_healthy == 0

# anything the appliance itself considers critical
truenas_appliance_alerts{level=~"CRITICAL|ALERT|EMERGENCY"} > 0

# capacity trend
predict_linear(truenas_appliance_pool_free_bytes[7d], 14 * 86400) < 0
```

## What you must decide yourself

The driver reports. Everything below is an appliance-side action, and it is
yours:

1. **Running a scrub.** Scrub scheduling is set on the appliance. If
   `truenas_appliance_scrub_state{state="FINISHED"}` has been stale for weeks,
   the fix is a scrub task in TrueNAS, not a call from Kubernetes.
2. **Replacing a disk.** A `FAILED` SMART result is a signal, not an
   instruction. Which disk, which spare, and when to resilver is a decision made
   with the array in front of you.
3. **Acting on alerts.** The driver surfaces the appliance's alerts verbatim and
   never dismisses one. Dismissing an alert is a statement that a human looked
   at it.
4. **Reacting to a full pool.** The driver reports free space and refuses to
   provision past its reserve; expanding a vdev, deleting snapshots or moving
   workloads is your call.
5. **Degraded pool policy.** `truenas_appliance_pool_healthy == 0` does not stop
   provisioning. Whether a degraded pool should still accept new volumes depends
   on your redundancy and your risk tolerance — express it as an alert and a
   decision, not as an automatic action.
