# Volume migration

Migration imports an existing **foreign** PersistentVolume's data — Longhorn,
local-path, another vendor's CSI driver — into a volume this driver provisioned
on TrueNAS, so a cluster can move off its old storage without an operator
hand-copying data between mounts.

It is implemented in `internal/migration`.

## Running it

```sh
# Plans and prints. Changes nothing.
truenas-csi migrate -namespace apps -source old-claim -target new-claim

# Performs the copy. -confirm must repeat the target's name.
truenas-csi migrate -namespace apps -source old-claim -target new-claim \
  -apply -confirm new-claim
```

Two separate gates on purpose. `-apply` alone is not enough: `-confirm` has to
repeat the target claim's name, so a command recalled from shell history cannot
run against a different claim than the one it was written for. Without `-apply`
the command is a dry run and prints the plan it would execute.

Ownership is not re-checked by the command. `internal/migration` refuses any
volume this driver does not own, by the same `io.truenas.csi:managed` property
with `source == LOCAL` that every destructive path here checks — a second,
looser check in the CLI would be a second answer to the same question.

The copy runs as a Kubernetes Job, so the command needs cluster access as well
as the driver configuration.

## What it does, and what it does not do

Migration copies **data**. It does not:

- create the target volume (you do that with a normal PVC on a driver
  StorageClass, before you start),
- repoint your workload at the new claim (you edit the Deployment/StatefulSet),
- delete, release or reclaim the source volume — ever. See
  [What you must decide](#what-you-must-decide-yourself).

The driver process has neither volume mounted, so it does not copy anything
itself. It renders a Kubernetes **Job** that mounts the source claim read-only
and the target claim read-write, copies, and verifies.

## The four safety rules

These are non-negotiable, and each has a test named after it in
`internal/migration/migration_test.go`.

### 1. The target must be a volume this driver created

`Plan` refuses any target whose dataset does not carry the ownership marker
`io.truenas.csi:managed=truenas-csi` **with ZFS property source `LOCAL`**.

The `LOCAL` requirement is the whole point. ZFS user properties are inherited by
child datasets, so if the operator-configured parent dataset ever carried the
marker — set by hand, or by an earlier install — every pre-existing dataset
beneath it would report the marker, and a presence-only check would clear a
migration to copy over the operator's real data. Only `LOCAL` means "set on this
dataset, at creation, by this driver".

The target must also be on this migrator's backend and confined inside the
configured `<pool>/<parentDataset>`.

Refusal happens in `Plan`, before any Job object exists.

### 2. The source is mounted read-only

The generated Job mounts the source PVC with `readOnly: true` on **both** the
pod volume and the container volume mount. A migration must never be able to
modify the thing it is copying from — including through a bug in the copy
script, which the kernel then simply cannot execute.

### 3. Success requires verification

The Job does not report success because the copy command exited zero. After
copying it builds a **checksum manifest** of every regular file on each side
(`find . -type f | sort | sha256sum` per file, paths relative to the volume
root), compares the **file counts** and the **manifest digests**, and exits
non-zero when they differ, printing the first 50 differing lines.

It writes a JSON summary to `/dev/termination-log`:

```json
{
  "source_files": 1204,
  "target_files": 1204,
  "source_digest": "…",
  "target_digest": "…",
  "bytes_copied": 8123456,
  "verified": true
}
```

`Run` reads that summary back from the pod's container status and re-checks it
in Go (`Summary.Check`). A Job that exited zero **without** a summary is
reported as `Failed`, not `Succeeded`: it has proved nothing, and defaulting an
unproved copy to success is exactly how a partial copy gets declared complete —
after which the operator deletes the source.

`Verify(source, target Manifest)` is the same rule, exported, so an operator who
has fetched both manifests can re-run the judgement themselves rather than
trusting an exit code. It names the missing, differing and unexpected paths.

### 4. The source is never deleted

Nothing in this package, and nothing in the generated script, deletes anything:
no `rm`, no `rsync --delete`, no `mkfs`, no middleware `delete` call. The report
carries `SourceRetained: true` and says so in its message.

## Copy mechanism

The Job runs, by default (`ModeAuto`):

```sh
rsync -aHAX --numeric-ids --partial --inplace --stats /source/ /target/
```

falling back to `tar -C /source -cf - . | tar -C /target -xpf -` when the image
has no rsync. `ModeRsync` fails the Job instead of falling back; `ModeTar`
always uses tar.

- `-aHAX --numeric-ids` preserves permissions, hard links, ACLs, xattrs and the
  numeric uids/gids — the container has no knowledge of the source's user
  database, so names must not be resolved.
- `--partial --inplace` is what makes the copy **resumable**: a re-run over a
  partial target transfers only what differs.

The container runs as uid 0 so `-aHAX` can restore ownership, with every
capability dropped except `CHOWN`, `DAC_OVERRIDE`, `FOWNER` and `FSETID`.

The default image is `docker.io/library/alpine:3.22`; set `Request.Image` to
anything with a POSIX shell, `find`, `sha256sum` and preferably `rsync`.

## Resumability

The Job name is derived deterministically from
`(namespace, sourcePVC, targetPVC)`, so:

- `Run` after a failure **adopts** the existing Job rather than creating a
  second copy into the same volume,
- the Job's own `backoffLimit` (4 by default) retries the copy in place,
- each retry resumes from the partial target instead of starting over.

`Run` is therefore safe to call repeatedly. To force a genuinely fresh start,
delete the Job yourself; the driver will not do it for you.

## Usage sketch

```go
m := migration.New(kubeClient, truenasClient, backendCfg)

plan, err := m.Plan(ctx, migration.Request{
    SourceNamespace: "apps",
    SourcePVC:       "data-longhorn",   // the volume you are leaving
    TargetPVC:       "data-truenas",    // an already-bound PVC on a driver StorageClass
})
if err != nil {
    // volume.ErrNotManaged  -> the target is not a volume this driver created
    // migration.ErrNotEligible -> unbound claim, wrong driver, target too small
}

report, err := m.Run(ctx, plan)   // creates or adopts the copy Job
// report.Phase: Running | Succeeded | Failed
// report.Verified, report.Summary, report.SourceRetained
```

Both claims must be in the **same namespace**: one pod has to mount both.

## What you must decide yourself

The driver deliberately stops short of these. They are irreversible, and they
depend on facts the driver cannot see.

1. **Quiesce the workload.** Scale the application to zero before migrating.
   The copy is a point-in-time snapshot of a live filesystem; a database written
   to during the copy will be copied inconsistently and will still verify,
   because both manifests are taken after the copy. Verification proves the two
   filesystems match — it does not prove the data is application-consistent.
2. **Repoint the workload.** Edit the Deployment/StatefulSet to use the new
   claim, restart it, and confirm the application is healthy on the new volume.
3. **Retire the old volume.** Only you know when the migration is proven in
   production. Deleting the source PVC — and whether its reclaim policy then
   destroys the underlying Longhorn volume — is your call, taken after step 2,
   never by the migration.
4. **Clean up the Job.** Migration Jobs are named `truenas-csi-migrate-<hash>`
   and are left in place so their logs and termination messages remain readable.

## Verifying a migration by hand

```sh
kubectl -n apps get job -l app.kubernetes.io/name=truenas-csi-migration
kubectl -n apps logs job/truenas-csi-migrate-<hash>
kubectl -n apps get pod -l job-name=truenas-csi-migrate-<hash> \
  -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.message}'
```

The last command prints the verification summary the driver reads.
