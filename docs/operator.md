# The TrueNAS CSI lifecycle operator

The operator installs and maintains the driver through a single cluster-scoped
resource, `TrueNASCSIDriver`. It lives in its own Go module under
[`operator/`](https://github.com/piwi3910/truenas-csi/tree/main/operator), and it renders **the same Helm chart** that a Helm
install uses — [`deploy/helm/truenas-csi`](https://github.com/piwi3910/truenas-csi/tree/main/deploy/helm/truenas-csi) — through
the Helm Go SDK, then applies the result server-side.

There is exactly one copy of the driver's manifests in this repository. The
operator does not template Deployments in Go. That matters more than it sounds:
two copies of a manifest set drift, and the drift is only ever discovered by
whichever install path nobody is testing.

## Should you use it?

Helm is fine. If you install once, upgrade by hand, and watch the rollout, you
gain nothing from the operator and you have one fewer component to run.

The operator earns its place when upgrades and credential rotations happen
without someone watching, because those are the two moments a CSI driver can
take a cluster's workloads down.

## What the operator owns beyond Helm

### 1. Typed, validated configuration

`helm install -f values.yaml` accepts whatever YAML you give it. A wrong pool
name, a StorageClass naming a backend that does not exist, or a reserve
expressed two contradictory ways is discovered by the first PVC that fails to
bind — often hours later, often by someone else.

`TrueNASCSIDriver` is a typed projection of the chart's values, and the API
server enforces it at `kubectl apply` time. The rules that matter:

- **The endpoint must be `wss://`.** This is the single most important
  validation in the whole project. TrueNAS 25.10 _revokes_ an API key the moment
  it is presented over a plaintext connection — a plaintext WebSocket endpoint
  does not fail to connect, it destroys the credential and someone has to issue a new one by
  hand. Three keys were lost this way during development. The CRD's pattern is
  anchored at both ends, because Kubernetes evaluates `pattern` as an unanchored
  match and an unanchored expression would accept
  `http://elsewhere/?x=wss://nas`.
- **There is nowhere to put an API key.** Credentials are supplied only as a
  `apiKeySecretRef`. A key typed into the resource would be readable by anyone
  with `get truenascsidrivers`, would land in every backup of the cluster's
  resource inventory, and would be pasted into bug reports with the rest of
  `kubectl get -o yaml`. The structural schema prunes an `apiKey:` field rather
  than storing it, and a test keeps that true as fields are added.
- **Every `storageClasses[].backend` must name a real backend**, checked by a
  CEL rule on the resource.
- `reservedBytes` and `reservedPercent` are mutually exclusive; `parentDataset`
  may not contain `..`; `kubeletDir` must be an absolute path; enums are enums.

### 2. Drain-aware node rollout

A DaemonSet's own `RollingUpdate` strategy knows nothing about storage. It will
delete a plugin pod on a node that is halfway through staging a volume — the CSI
call in flight fails, and on a _terminating_ pod the iSCSI session and the mount
are left with no process that knows how to unstage them. Ripping the node plugin
out from under a mounted volume is how a storage upgrade takes workloads down.

So the operator sets the node DaemonSet to `updateStrategy: OnDelete` and drives
the rollout itself:

- one node at a time, never two;
- a node is rolled only when it reports **clear** — no pod on it is terminating
  with one of this driver's volumes, and no pod on it is Pending waiting for one;
- the next node does not start until the previous node's plugin is `Ready` again;
- nodes are considered in name order, so an operator restart resumes the rollout
  rather than picking a new victim.

A pod that is merely _Running_ with a mounted volume is not "busy". The volume is
staged and nothing is in flight, so the plugin can be replaced under it — and
treating it as busy would mean the rollout never finishes on a cluster that
actually uses its storage.

Progress and the reason for any pause are in `status.rollout`:

```console
$ kubectl get truenascsidriver truenas -o jsonpath='{.status.rollout}' | jq
{
  "desiredRevision": "3f1c9a02b7d4e551",
  "updatedNodes": 2,
  "totalNodes": 4,
  "currentNode": "worker-3",
  "waitingFor": "node worker-4 is not clear to roll: pod prod/postgres-0 is terminating with volume prod/pgdata still staged (1 node(s) waiting)"
}
```

### 3. API key rotation

The driver reads its API key once, at startup, and holds the websocket open for
the life of the process. It also treats an authentication failure as terminal and
never retries — deliberately, because a naive reconnect loop is exactly what
revokes a key. So rotating the key on the appliance and updating the Secret
changes nothing until the pods restart, and when the appliance finally rejects
the old key the driver does not recover on its own.

The operator watches the referenced Secrets and, when one changes:

1. restarts the **controller** first (its config checksum changes, and leader
   election hands provisioning to the standby);
2. waits until the controller is healthy on the new key;
3. only then rolls the node plugins, through the drain-aware rollout above.

If the new key is wrong, the failure lands on two controller pods instead of on
every node in the cluster at once.

The Secret is created by an administrator, not by the operator, so it carries no
owner reference and `Owns()` would never see it — the operator watches Secrets by
name through the resources that reference them.

### 4. Version-skew refusal

An upgrade whose driver version is incompatible with the CSI sidecar versions the
chart pins is refused **before anything is applied**. The alternative is worse
than not upgrading: a new driver image against sidecars it cannot talk to leaves
the controller up, the provisioner failing, PVCs unbound, and the DaemonSet
half-rolled with some nodes unable to mount anything.

A refusal sets `Degraded=True` with reason `VersionSkew` and leaves the previous
release exactly as it was:

```console
$ kubectl get truenascsidriver truenas -o jsonpath='{.status.conditions}' | jq '.[] | select(.type=="Degraded")'
{
  "type": "Degraded",
  "status": "True",
  "reason": "VersionSkew",
  "message": "version skew: driver version 2.0.0 is outside the range this operator supports (>= 0.1.0-0, < 1.0.0-0); upgrade the operator first"
}
```

A driver tag that is not a semantic version — `main`, a development build — is
accepted: that is a deliberate act by whoever set it. A _sidecar_ pinned to a
floating tag such as `latest` is refused, because nobody sets that by hand and a
pin the operator cannot read is a pin it cannot reason about.

### 5. Status worth reading

- `observedGeneration` — a status whose `observedGeneration` lags `generation`
  describes the _previous_ spec. Every condition carries it too.
- `conditions` — `Ready`, `Progressing`, `Degraded`.
- `appliedVersion` — the driver version actually applied.
- `backends[]` — per-appliance `reachable` and `orphanedVolumes`, read from the
  driver's own `truenas_csi_backend_up` and `truenas_csi_orphaned_volumes`
  metrics. The operator never opens its own connection to TrueNAS: a second
  process holding the same key means a second connection against the appliance's
  concurrency budget and a second chance to destroy the credential.
  `orphanedVolumes` counts datasets this driver owns with no matching
  PersistentVolume. The driver never deletes them — the number is a prompt to
  investigate, not a failure.
- `rollout` — as above.

## Installing

### Prerequisites

- Kubernetes 1.31 or newer.
- The credential Secret, in the namespace the driver will live in.
- For snapshots: the cluster-wide external-snapshotter CRDs and controller. The
  operator will **never** install them. They are a cluster singleton, and two
  drivers installing them breaks snapshots for both.

### With kustomize

```console
kubectl apply -k operator/config/default
```

That creates the `truenas-csi-operator-system` namespace, the CRD, the RBAC and
a two-replica manager Deployment with leader election.

### With OLM / OperatorHub

The bundle is in [`operator/bundle`](https://github.com/piwi3910/truenas-csi/tree/main/operator/bundle). With `operator-sdk`:

```console
operator-sdk run bundle ghcr.io/piwi3910/truenas-csi-operator-bundle:0.1.0
```

### Then create the driver

```console
kubectl create namespace truenas-csi
kubectl -n truenas-csi create secret generic truenas-credentials \
  --from-literal=apiKey='1-…'
kubectl apply -f operator/config/samples/truenas_v1alpha1_truenascsidriver.yaml
kubectl get truenascsidriver truenas -w
```

The endpoint must be `wss://`. If it is not, `kubectl apply` fails — which is the
point.

## Migrating from a plain Helm install

The operator renders the chart under the release name `truenas-csi`, so it
produces **byte-identical resource names** to
`helm install truenas-csi ./deploy/helm/truenas-csi -n truenas-csi`. The
migration is therefore an adoption, not a reinstall: no PersistentVolume is
touched, no volume is unmounted, and no pod that is using storage restarts
because of the switch itself.

The driver name `csi.truenas.watteel.com` is a compile-time constant and never
varies with the release name. Nothing in this migration changes it — which is
essential, because changing it orphans every PersistentVolume in the cluster.

1. **Write down what you have.** The CRD's spec is narrower than `values.yaml`;
   check that everything you set has a home in it before you start.

   ```console
   helm -n truenas-csi get values truenas-csi > /tmp/current-values.yaml
   ```

2. **Move the API keys out of Helm values into a Secret.** If you were using
   `existingSecret`, you already have one — but it holds the whole rendered
   `config.yaml`, and the operator wants one key per credential:

   ```console
   kubectl -n truenas-csi create secret generic truenas-credentials \
     --from-literal=apiKey='1-…'
   ```

3. **Install the operator** (kustomize or OLM, above). It does nothing until a
   `TrueNASCSIDriver` exists.

4. **Write the `TrueNASCSIDriver`** from your recorded values, using the sample
   as a starting point. Set `spec.namespace` to the namespace the Helm release is
   already in. Apply it and check that it is accepted — this is where a bad
   endpoint or a StorageClass naming a missing backend is caught.

5. **Release Helm's ownership without deleting anything.** `--keep-resources`
   removes the release record and leaves every object in place:

   ```console
   helm -n truenas-csi uninstall truenas-csi --keep-resources
   ```

   Do this _after_ the resource is applied, so there is no window in which
   nothing is managing the driver.

6. **Let the operator adopt the objects.** It applies each rendered object
   server-side under the field manager `truenas-csi-operator` and stamps an owner
   reference, so they become the CR's children and are garbage-collected with it.
   Watch it settle:

   ```console
   kubectl get truenascsidriver truenas -o yaml | yq '.status'
   ```

   Expect one node rollout: the operator switches the DaemonSet to `OnDelete` and
   adds its revision annotation, which changes the pod template. That rollout is
   drain-aware, one node at a time — which is exactly the behaviour you installed
   the operator for.

7. **Delete the old Helm values file** if it contained API keys. It did, unless
   you were using `existingSecret`.

### Going back

Nothing here is one-way. Delete the `TrueNASCSIDriver` with
`--cascade=orphan` to leave the objects behind, uninstall the operator, and
`helm upgrade --install` over the existing objects.

## Security notes

- **The node plugin is privileged, by necessity.** It runs with `hostPID` and
  `hostNetwork`, mounts `/dev`, `/etc/iscsi`, `/var/lib/iscsi` and the kubelet
  root, and enters the host mount namespace with `nsenter` so `iscsiadm` talks to
  the host's `iscsid`. There is no way to attach a block device to a node from
  inside an unprivileged container. That state is shared with Longhorn on the
  target cluster, so every `iscsiadm` call the driver makes is scoped to one
  target and one portal — no `--logoutall`, no unscoped deletes, no global
  rescan.
- **The controller is not privileged.** It runs as UID 65532, non-root, with
  `RuntimeDefault` seccomp, and touches no host state.
- **The operator itself is not privileged either**, holds no TrueNAS credential
  of its own, and never dials an appliance. It reads the driver's metrics
  endpoint over HTTP inside the cluster to report backend health.
- **Credentials never leave the Secret.** The operator reads the referenced keys
  into memory, renders them into the driver's config Secret, and writes nothing
  about them anywhere else. `status.credentialsRevision` is a hash of Secret
  _resourceVersions_, not of key material: a hash of a secret is still an oracle
  for it.
- The operator's ClusterRole includes `escalate` and `bind` on RBAC resources,
  because it creates the driver's ClusterRoles. That is a genuine privilege and
  is why the operator's own namespace should be treated as sensitive.

## Development

```console
cd operator
make generate manifests fmt vet test
```

The tests run without a cluster. The envtest-backed cases — the ones that check
the endpoint pattern against a real API server — skip with a clear message when
the API server binaries are absent; `make envtest` prints the `KUBEBUILDER_ASSETS`
path that turns them on.

The repository is a Go workspace ([`go.work`](https://github.com/piwi3910/truenas-csi/blob/main/go.work)) tying the driver and
the operator modules together for local development. Both modules also build on
their own with `GOWORK=off`, which is what CI and the release build do.
