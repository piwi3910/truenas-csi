# Security model

This document covers the four things an operator must decide before trusting this driver
with a production pool: what privileges it holds on TrueNAS, how it trusts the appliance's
certificate, what the iSCSI object model does and does not isolate, and what stops a driver
bug from deleting data it did not create.

---

## Least privilege on TrueNAS

The driver runs against a **dedicated TrueNAS account with 14 roles**, not `FULL_ADMIN`.
The set was derived as a verified cover over every middleware method the driver calls:

```
DATASET_WRITE
DATASET_DELETE
POOL_READ
SNAPSHOT_WRITE
SNAPSHOT_DELETE
SHARING_ISCSI_EXTENT_WRITE
SHARING_ISCSI_TARGET_WRITE
SHARING_ISCSI_TARGETEXTENT_WRITE
SHARING_ISCSI_GLOBAL_READ
SHARING_ISCSI_PORTAL_READ
SHARING_ISCSI_INITIATOR_READ
SHARING_ISCSI_AUTH_READ
SHARING_NFS_WRITE
FILESYSTEM_ATTRS_WRITE
```

Write roles imply their read counterparts, which is why no `DATASET_READ` or
`SNAPSHOT_READ` appears: they are already covered. The four `SHARING_ISCSI_*_READ` roles
are read-only on purpose — the driver reads the global iSCSI configuration (for the target
base name), the portal, the initiator group and the CHAP auth entries, but the portal and
initiator objects themselves are cluster-wide state it only creates on first use.

If you run NFS only, the `SHARING_ISCSI_*` roles can be omitted; if you run iSCSI only,
`SHARING_NFS_WRITE` and `FILESYSTEM_ATTRS_WRITE` can be. Granting the whole set is simpler
and still far narrower than any admin role.

### `system.info` is deliberately never called

Calling `system.info` merely to log the appliance version at startup would require
`READONLY_ADMIN` — a role broader than everything above combined, granting read access
across the whole appliance. The driver does not call it. Do not add a "version banner"
feature that reintroduces the requirement.

### Creating the account and key

1. **TrueNAS UI → Credentials → Users → Add.** Create a user, e.g. `csi`. Give it no shell
   (`nologin`), no home directory of consequence, and no SMB access. It exists only to own
   an API key.
2. **Credentials → Privileges → Add** (or edit the user's roles, depending on release).
   Add exactly the 14 roles listed above. Do not add `FULL_ADMIN` or `READONLY_ADMIN`
   "temporarily to test" — a key issued under a broad role stays as broad as it was issued.
3. **Credentials → API Keys → Add.** Name it for the cluster it serves, associate it with
   the `csi` user, and copy the key **once** — TrueNAS does not show it again.
4. **Confirm the appliance is reachable over TLS** before the key touches anything. The
   very first connection made with the key must be `wss://`; a single plaintext `ws:` or `http:`
   attempt revokes it (see [troubleshooting](troubleshooting.md)).
5. Put the key into the chart's `backends.<name>.apiKey`, or into a Secret named by
   `existingSecret`. It is rendered into a Secret mounted at
   `/etc/truenas-csi/config.yaml`, never into a container argument or a plaintext
   environment variable.
6. **Verify the privilege is sufficient and no broader** by exercising provisioning,
   snapshot, restore, expansion and delete with the account. If anything needs a role
   outside the 14, that is a finding to report, not a reason to widen the account.

Rotate the key by issuing a new one, updating the Secret and restarting the controller.
Existing mounts are unaffected: they are kernel-level NFS and iSCSI connections that do not
involve the middleware API.

### Kubernetes-side privilege

The controller holds the RBAC its sidecars need. The **node plugin's Kubernetes credentials
are read-only** — it runs privileged on every node, so its API token is the most valuable
thing on the node and must not be able to write cluster state. The controller runs as
non-root.

---

## TLS trust

**The stock TrueNAS certificate cannot be verified, by construction.** A fresh install
presents a self-signed certificate:

```
subject/issuer: C=US, O=iXsystems, CN=localhost
SAN:            DNS:localhost
```

Verification fails twice over: the chain is self-signed, and the only SAN is `localhost`,
which cannot match the address the driver connects to. This is not a misconfiguration to be
fixed by "using the system trust store" — no public CA is involved at all.

Choose one of:

1. **Install a real certificate on the appliance** (your internal CA, or ACME through
   TrueNAS's certificate manager) with a SAN matching the address in `endpoint`, and give
   the driver the issuing CA in `backends.<name>.caCert`. This is the supported path.
2. **Keep the self-signed certificate and pin it**: put its PEM in `caCert`. When `caCert`
   is set, it is the _only_ certificate trusted for that appliance — the system roots are
   not consulted, so this is a pin, not an addition.
3. `insecureSkipVerify: true`. It exists because option 1 is not always available on day
   one. It **defaults to `false`**, and when it is on the driver warns loudly on every
   connect. It disables certificate verification entirely, which means anything that can
   intercept the connection can present its own certificate and collect the API key. Treat
   it as a temporary state with a ticket attached, never as a configuration.

---

## ⚠️ Accepted risk: one shared iSCSI target exposes every LUN to every node

**Read this before you rely on `ReadWriteOnce` as a security boundary. It is not one.**

The driver creates **one shared iSCSI target per backend**, with **one LUN per volume**.
Every node that is allowed to log in logs into that one target.

iSCSI initiator ACLs are a property of the **target**, not of individual LUNs. There is no
per-LUN masking in this model. The consequences are direct:

- **Every node logged into the target can see every LUN** the driver has created on that
  backend — including LUNs belonging to volumes attached to a different node, a different
  namespace or a different workload.
- **`ReadWriteOnce` is enforced by Kubernetes, not below it.** The kubelet and the attacher
  ensure only one node stages the volume; nothing on the SAN prevents a process with root
  on another node from opening the same block device directly and writing to it. Doing so
  corrupts the filesystem, because two hosts would be writing with independent page caches.
- **The `initiatorACL` parameter defends the cluster boundary, not one node from another.**
  With it on (the default), the target is restricted to the cluster's node IQNs, which keeps
  machines outside the cluster off the target. It does nothing to isolate node A's volumes
  from node B. The same is true of `chap`: CHAP authenticates the initiator to the target,
  it does not scope which LUNs that initiator sees.

**This was a deliberate choice.** The alternative — a target per node or a target per volume
— multiplies the cluster-wide objects the driver must create, reconcile and garbage-collect
on the appliance, and turns every attach into a target lifecycle problem. The shared target
was chosen for that reason, with the exposure accepted.

What follows from accepting it:

- **All cluster nodes must be treated as equally trusted.** A node that can reach the portal
  can reach every iSCSI volume on that backend. Anyone with root on any node, or able to run
  a privileged pod on any node, effectively has read/write access to all of them.
- Do not use iSCSI volumes from this driver as an isolation boundary between mutually
  distrusting tenants sharing a cluster.
- **If you need per-volume isolation, do not use this driver as configured.** Use NFS, where
  exports are per-volume and restricted by `networks` and by server-side permissions, or use
  a separate backend (a separate appliance or a separate driver installation) for the data
  that requires isolation.
- Keep the portal off networks that non-cluster machines can reach, and keep
  `initiatorACL: "true"` and `chap: "true"`. They are worth having — they just protect a
  different boundary than the one operators usually assume.

NFS volumes do not share this property: each volume is its own export, with its own
`networks` restriction and its own server-side ownership and mode.

---

## Data safety: what stops the driver deleting your data

The pool this driver targets holds live, irreplaceable datasets alongside the ones it
manages. Two mechanisms keep them apart, and both are enforced before any destructive call.

### 1. Confinement to one parent dataset

Each backend is configured with a `pool` and a `parentDataset`, and the driver operates
**only** beneath that dataset. Volume IDs are structured
(`<backend>/<protocol>/<pool>/<dataset path>/<name>`) and are checked against the configured
pool and parent before any middleware call. An ID that resolves outside the parent —
traversal sequences, a crafted name, a PersistentVolume left over from another install — is
rejected outright.

A StorageClass may restate `pool` and `parentDataset`, but a value that does not match the
backend's configuration is rejected rather than honoured. These parameters are operator
policy, not a redirect: a user who can write a StorageClass still cannot point the driver at
another part of the pool.

### 2. Ownership marking, verified as `LOCAL`

Every dataset the driver creates is stamped with the ZFS user property
`io.truenas.csi:managed`. Before any delete, the driver queries the dataset and **refuses
unless the property is present _and_ its `source` is `LOCAL`**.

The `source == "LOCAL"` requirement is the load-bearing part. **ZFS user properties are
inherited by children.** If the operator-configured parent dataset ever carried the marker —
set by hand, or by an earlier install — every pre-existing dataset beneath it would report
the property as inherited and would look driver-owned. Checking for presence alone would
therefore turn an inherited property into a licence to delete real data. Checking the source
distinguishes a dataset the driver actually stamped from one that merely sits under a marked
parent.

A dataset restored from a snapshot is a ZFS clone, and a clone inherits **neither** the
marker nor the quota from its origin — it takes its properties from its position in the
hierarchy. The driver therefore re-stamps the marker and re-applies the quota explicitly
after cloning. Without that, every restored volume would be undeletable and would leak
forever.

When the guard refuses, it says so and leaves the dataset intact. That is the intended
outcome: an orphaned dataset is an operator's cleanup task; a deleted one is not
recoverable. For the same reason the orphan reconciler only **reports** datasets with no
matching PersistentVolume — it never deletes anything.

---

## Secrets handling

The TrueNAS API key and any CHAP credentials are read from a mounted Secret into memory.
They must never appear in logs, error messages, metric labels or process arguments, and
there is a test asserting exactly that. Metric label values are bounded to method and
backend names, so an unbounded or sensitive value cannot become a label.

CHAP credentials are generated by the driver and stored as an `iscsi.auth` entry on TrueNAS,
which is also where the node plugin reads them from — no additional Kubernetes Secret is
needed for CHAP, and the credential does not travel through the cluster's API.

## Reporting a vulnerability

Report security issues privately through the repository's security advisory process rather
than a public issue.
