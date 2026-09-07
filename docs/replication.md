# Storage replication — StorageProtectionGroup

This driver can replicate the volumes it provisions from one TrueNAS appliance
to another, and coordinate failover between them. The model follows Dell's CSM
for Replication (`StorageProtectionGroup`, failover, test failover, failback,
suspend, resume, `CreateRemoteVolume`) and is implemented on top of TrueNAS's
own dataset replication rather than anything invented here.

**Verification status:** the model, the API payloads and every safety refusal
are exercised against the in-process middleware fake. A real source-to-target
replication has **not** been run: only one appliance is available. See
[What has and has not been verified](#what-has-and-has-not-been-verified) — read
that section before enabling this in production.

## The model

A `StorageProtectionGroup` names:

- a set of **driver-managed volumes** on a **source** appliance,
- a **target** appliance,
- how often to snapshot and replicate,
- and, optionally, one **action** to perform.

```yaml
apiVersion: replication.truenas.io/v1alpha1
kind: StorageProtectionGroup
metadata:
  name: payments
  namespace: prod
spec:
  sourceBackend: nas1
  targetBackend: nas2
  volumeHandles:
    - nas1/nfs/Pool0/k8s/pvc-3f0c...
    - nas1/iscsi/Pool0/k8s/pvc-91ab...
  sshCredentialID: 3 # SSH_CREDENTIALS on nas1 describing nas2
  reverseSSHCredentialID: 5 # SSH_CREDENTIALS on nas2 describing nas1
  schedule:
    minute: "*/15"
  retention:
    value: 2
    unit: WEEK
```

### What it builds on the appliance

| Object                                        | TrueNAS call               | Purpose                                                                                                                       |
| --------------------------------------------- | -------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| One periodic snapshot task per member dataset | `pool.snapshottask.create` | Produces the snapshot stream, named `csi-<group>-%Y-%m-%d_%H-%M`                                                              |
| A baseline snapshot per member                | `pool.snapshot.create`     | The first replication run has something to send immediately, instead of leaving the target unusable until the first cron tick |
| One replication task per group                | `replication.create`       | `direction: PUSH`, `transport: SSH`, bound to those snapshot tasks, `target_dataset` = the target's configured parent dataset |
| An initial run                                | `replication.run`          | Seeds the target now rather than at the next tick                                                                             |

Three settings on the replication task carry weight:

- `readonly: SET` — the replica is not writable until a failover deliberately
  clears it. This is what makes the target safe to fail over _to_ and prevents
  anything writing to it before then.
- `allow_from_scratch: false` — a snapshot stream that no longer matches fails
  loudly instead of destroying the target and re-sending it. The one exception
  is a _forced_ failback, where mismatch is the expected condition.
- `retention_policy: SOURCE` — the target mirrors the source rather than
  becoming an archive nobody prunes.

Because the task is bound to periodic snapshot tasks it cannot also carry a
`schedule`; the group's cron becomes `restrict_schedule`, the window in which
transfers may run.

### Group state

Group state lives in `status` on the custom resource: the phase, the replication
task id, and — per volume — the snapshot the target held when it was promoted.
This is not bookkeeping. `status.phase` is what makes failover idempotent, and
`status.volumes[].failoverSnapshot` is what makes divergence detectable. A
controller restart must not lose either, which is why they live in the object
rather than in memory.

Phases: `Ready`, `Suspended`, `FailingOver`, `FailedOver`, `FailingBack`.

## Operations

Set `spec.action`. It is a one-shot request, not a desired state: the controller
records the generation it acted on, so re-applying the same manifest never
repeats the action.

### Failover

Promotes the target to primary.

1. Disables the replication task and its snapshot tasks on the source, so
   nothing overwrites the replica while it is being promoted.
2. For each member, on the target: proves the dataset is driver-owned, records
   the newest replicated snapshot as the failback reference point, then clears
   `readonly`. A target that is genuinely a ZFS clone additionally gets
   `pool.dataset.promote`.
3. Records `FailedOver`.

**Failover is explicit and idempotent.** A group already in `FailedOver` returns
unchanged without issuing a single write — a second failover cannot promote
twice. A group in `FailingOver` (a failover that started and did not finish) is
**refused**: that one may still be in flight, and two in flight is exactly how
two appliances end up both believing they are primary. Forcing it is possible,
and requires stating so with `spec.force: true`.

Failover with an unreachable source is a legitimate case — that is what a
disaster is. Step 1 then fails, and the refusal says so; forcing skips it.

### TestFailover

Rehearses a failover without disturbing anything.

1. Creates a scratch dataset `<target parent>/csi-testfailover-<group>`.
2. Clones each member's newest replicated snapshot into it
   (`pool.snapshot.clone`).
3. Stamps the ownership marker on each clone — a ZFS clone inherits neither the
   marker nor the quota, and an unstamped clone could never be cleaned up.

**It cannot promote the real target.** Every middleware call this path makes
goes through a guard that permits only reads plus writes _inside the scratch
dataset_, and refuses `pool.dataset.promote` and every `replication.*` mutation
outright. The production target's `readonly` is never cleared and the
replication task is never touched, disabled or deleted. That is enforced by the
guard, not merely intended by the code above it.

`StopTestFailover` destroys the scratch clones, each only after proving the
driver created it.

### Failback

Returns primacy to the source.

**Failback refuses when the source has diverged since failover.** Failback is a
reverse replication, and reverse replication rolls the source back to the
target's snapshot stream: anything written to the old source in the meantime is
destroyed. Before doing anything, the driver compares each source dataset's
snapshots against the failover reference point and refuses if the source has
gained snapshots since — naming the volume and every snapshot involved:

```
source has diverged since failover: pvc-3f0c gained rogue-2026-09-08,
rogue-2026-09-08-2; failing back would discard this. Force only if these
changes are expendable
```

Two related cases are also treated as divergence, because refusing is
recoverable and a silent rollback is not: the reference snapshot no longer
exists on the source, and no reference was recorded for a volume at all.

With `spec.force: true` the reverse task is created with
`allow_from_scratch: true` — the source's stream no longer matches, and that is
the only situation where restarting it from scratch is correct.

The reverse task lives on the target, runs once, and is deleted. Leaving it
behind would give the group two tasks that could both run.

### Suspend / Resume

`replication.update {enabled: false|true}` on the group's task, plus its
snapshot tasks. Both are **level-triggered**: the appliance is read first and a
write is issued only if it disagrees. Calling them repeatedly — which a
controller does on every resync — costs one query and no writes.

Suspend and resume are refused while the group is failed over: there is no
source stream to suspend.

### CreateRemoteVolume

Provisions the target-side dataset for a member volume, mirroring the source's
type and size, **stamped with the ownership marker at creation**. Without the
marker the driver's own guard would refuse to remove it later, so an unstamped
remote dataset is a permanent leak — and indistinguishable from a dataset the
operator created.

It is idempotent, but only for a dataset the driver owns: an existing dataset at
the remote path that is _not_ driver-owned is refused rather than adopted.

## Safety rules

These are the properties the tests exist to protect. Each one is a way this
package could destroy data if it were wrong.

1. **Every member must be driver-owned.** A group is refused if any member's
   dataset lacks `io.truenas.csi:managed` with source `LOCAL`. A replication
   task _overwrites_ its target on every run, unattended and on a schedule, so a
   group pointed at someone's real dataset would destroy it. The check is
   `volume.VerifyOwned` — the same guard provisioning uses, not a copy of it —
   and `source == LOCAL` matters as much as the value: ZFS user properties are
   inherited, so an inherited marker is not proof of ownership.
   _Test: `TestGroupRefusesUnownedVolumes`._

2. **Failover is explicit and idempotent.** A repeat is a no-op; an incomplete
   one is refused unless forced.
   _Tests: `TestFailoverIsIdempotent`, `TestFailoverRefusesAfterAnIncompleteOne`._

3. **A test failover cannot promote the real target.** Enforced by a call guard,
   not by convention.
   _Test: `TestTestFailoverDoesNotPromoteTarget`._

4. **Failback refuses a diverged source and says what diverged.**
   _Test: `TestFailbackRefusesOnDivergence`._

5. **Suspend and resume are safe to repeat.**
   _Test: `TestSuspendResumeAreIdempotent`._

6. **Nothing here deletes a dataset it did not create.** Group teardown proves
   ownership of _every_ target dataset before destroying _any_ of them, so a
   group containing one foreign dataset deletes nothing at all. The same guard
   covers the test-failover scratch clones. By default a group's deletion leaves
   the replicated data alone entirely: a group is a policy, and deleting the
   policy must not delete the only surviving copy.
   _Test: `TestReplicationNeverDeletesForeignDatasets`._

7. **Remote volumes are stamped at creation.**
   _Test: `TestCreateRemoteVolumeStampsOwnership`._

Additionally: a group whose source and target are the same appliance is refused
outright — the task would push a dataset over itself.

### Refusals are not retried

The controller classifies errors. A safety refusal (unowned dataset, divergence,
incomplete failover, active test failover, wrong phase, guard violation) is
recorded on the object as a `Refused` condition with the full explanation, and
is **not** requeued. Retrying it in a backoff loop would be retrying a decision
only a person should make. Transient failures — an unreachable appliance, a
middleware timeout — are requeued normally.

## Installation

```
kubectl apply -f deploy/crds/replication.truenas.io_storageprotectiongroups.yaml
```

The controller needs, on the TrueNAS account it uses, the roles the rest of the
driver needs plus `REPLICATION_TASK_WRITE` and `SNAPSHOT_TASK_WRITE`. SSH
connectivity between the appliances is configured in TrueNAS itself
(`keychaincredential`), and the group references it by id: this driver never
handles the SSH private key.

## What has and has not been verified

**Only one TrueNAS appliance is available in this environment.** A
source-to-target replication needs two, so the following are honest about their
status.

Verified against the live appliance (25.10.6), via the published API schema at
`https://<appliance>/api/docs/current/`:

- `replication.create`, `replication.query`, `replication.update`,
  `replication.delete` and `replication.run` exist, and the parameter shapes
  used here (`direction`, `transport`, `ssh_credentials`, `source_datasets`,
  `target_dataset`, `periodic_snapshot_tasks`, `restrict_schedule`,
  `retention_policy`, `readonly`, `allow_from_scratch`, `enabled`) match the
  documented schema. `replication.run` is a job; the rest are synchronous.
- `pool.snapshottask.create/query/update/delete` and their schedule shape.
- `pool.snapshot.create`, `pool.snapshot.clone` and `pool.dataset.promote`
  behave as documented — all three were exercised for real by the snapshot and
  clone work, including the finding that a clone inherits neither the ownership
  marker nor the quota, which is why test-failover clones are stamped.

Verified in tests, against the middleware fake:

- Every safety property in the list above, plus the exact middleware calls each
  operation issues and — as importantly — the calls it does **not** issue.

**Not verified, and it must be said plainly:**

- **No real replication has been run.** No data has moved from one appliance to
  another through this code. The task payload is schema-correct, but whether
  TrueNAS accepts and successfully runs _this particular_ combination of bound
  snapshot tasks, `restrict_schedule` and `readonly: SET` is untested on
  hardware.
- **No real failover.** Clearing `readonly` on a replicated dataset makes it
  writable in ZFS; that a Kubernetes workload then mounts and uses it correctly
  through the node plugin has not been demonstrated end to end.
- **No real failback.** The reverse-replication path, and how TrueNAS behaves
  when the source's snapshot stream has diverged, is untested on hardware. The
  divergence check itself is snapshot-based, so it detects changes that were
  snapshotted; writes to the old source that were never snapshotted are caught
  only by replication refusing the mismatched stream (`allow_from_scratch:
false`) — which is a real backstop, but a later one.
- **SSH credential handling is untested.** The group takes a keychain
  credential id and never sees the key, but no cross-appliance SSH transport has
  been established from this code.
- **Timing and job behaviour are untested.** `replication.run` is a long-running
  job; this code treats it as complete when the call returns. On real hardware a
  large initial sync will need job tracking, which the driver already has for
  `filesystem.setperm`.

Anyone commissioning this against two real appliances should start with a
throwaway group, one small volume, and `TestFailover` — the one operation that
is structurally incapable of touching production.
