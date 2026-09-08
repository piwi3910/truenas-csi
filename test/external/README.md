# Upstream Kubernetes external-storage conformance

This directory runs the **upstream Kubernetes `External.Storage` E2E suite**
against a live cluster backed by a live TrueNAS appliance.

Dell shipped `cert-csi` for exactly this job — 22 suites, timed stages, SQLite
and HTML reports — then archived it in November 2025 and deprecated it at
CSM 1.17, telling users to move to upstream Kubernetes E2E and OpenShift E2E.
`cert-csi` had already been wrapping the upstream suite itself through its
`k8s-e2e` subcommand. So there is no bespoke harness here, and there should
never be one: the industry settled on the upstream suite, and so does this
repository.

## What this covers that `test/integration` does not

`test/integration` drives the driver's **gRPC surface directly** against a real
appliance. It is the fastest way to find out whether `CreateVolume` produces the
right zvol, whether a refquota shrink is refused, whether a clone inherits the
ownership marker. It never involves Kubernetes.

This suite involves nothing else. It exercises the driver **as Kubernetes uses
it**, through the sidecars and the kubelet:

|           | `test/integration`                      | `test/external`                                                                                                                                                        |
| --------- | --------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Talks to  | the driver's gRPC socket                | a real cluster's API server                                                                                                                                            |
| Needs     | an appliance                            | an appliance **and** a cluster with the driver installed                                                                                                               |
| Exercises | backend logic, appliance calls          | external-provisioner, -attacher, -resizer, -snapshotter, kubelet, the scheduler                                                                                        |
| Finds     | wrong middleware calls, wrong ZFS state | wrong CSI _semantics_: idempotency under retry, topology that does not schedule, expansion that never reaches the pod, `subPath`, `fsGroup`, snapshot restore ordering |
| Runtime   | minutes                                 | hours                                                                                                                                                                  |

`test/chart` is a third thing again: it proves the chart installs and upgrades.
None of the three replaces another.

`test/sanity` (csi-sanity) checks the driver against the CSI _specification_
with a fake appliance. This suite checks it against Kubernetes' _interpretation_
of that specification, which is where the interesting disagreements live.

## Requirements

- A cluster with the driver installed and at least one StorageClass backed by it.
- **A homogeneous cluster.** Every node must have the tooling for the protocol
  under test (`nfs-common`, `open-iscsi`, `nvme-cli`, plus `e2fsprogs` /
  `xfsprogs`). The driver publishes per-capability node labels with the values
  `"true"` and `"false"`, and the topology testsuite pins a StorageClass to one
  value it observes; on a mixed cluster it can pin `"false"` and every provision
  then fails for a reason that is not a driver defect.
- `curl`, `tar`, `awk` and — unless `TRUENAS_E2E_VERSION` is pinned — `kubectl`.
- A TrueNAS appliance reachable over **`wss://` / `https://` only**. Plaintext to
  a TrueNAS appliance permanently revokes the API key presented to it.

## Running it

```sh
TRUENAS_E2E_KUBECONFIG=~/.kube/config \
TRUENAS_E2E_STORAGECLASS=truenas-nfs \
TRUENAS_E2E_PROTOCOL=nfs \
TRUENAS_E2E_SNAPSHOTCLASS=truenas-snapshots \
make e2e-external
```

or `test/external/run.sh` directly, or `go test ./test/external/ -timeout 5h`
with the same variables set (which is what `make e2e-external` shells into).

### Environment

| Variable                                         | Required | Meaning                                                                                                        |
| ------------------------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------- |
| `TRUENAS_E2E_KUBECONFIG`                         | yes      | kubeconfig for the cluster. Also the **gate**: with it unset, `go test ./...` skips this suite entirely.       |
| `TRUENAS_E2E_STORAGECLASS`                       | yes      | an existing StorageClass backed by this driver                                                                 |
| `TRUENAS_E2E_PROTOCOL`                           | yes      | `nfs`, `iscsi`, `nvme` or `smb` — selects the driver definition                                                |
| `TRUENAS_E2E_SNAPSHOTCLASS`                      | no       | an existing `VolumeSnapshotClass`. Without it the snapshot and restore tests are dropped, and the run says so. |
| `TRUENAS_E2E_VERSION`                            | no       | pin the `e2e.test` release. Default: whatever the cluster reports.                                             |
| `TRUENAS_E2E_INCLUDE_HEAVY`                      | no       | `true` drops the `[Slow]` and volume-limits skips. Expect hours more.                                          |
| `TRUENAS_E2E_EXTRA_SKIP`                         | no       | an extra ginkgo skip regex, for triaging one failure without editing the reviewed list                         |
| `TRUENAS_E2E_FOCUS`                              | no       | narrow the focus below `External.Storage`                                                                      |
| `TRUENAS_E2E_TIMEOUT`                            | no       | ginkgo timeout, default `4h`                                                                                   |
| `TRUENAS_E2E_ALLOW_SMB`                          | no       | required to run the SMB definition at all — see below                                                          |
| `TRUENAS_E2E_WORKDIR` / `_BINDIR` / `_REPORTDIR` | no       | where the binary, rendered definition and JUnit report land                                                    |
| `TRUENAS_E2E_CONTEXT`                            | no       | kubeconfig context                                                                                             |

### Why a downloaded binary and not a Go dependency

The suite lives in `k8s.io/kubernetes/test/e2e`, the one Go module nobody should
depend on: it pulls the whole of Kubernetes and every cloud provider client into
`go.mod`, and k/k publishes no usable semantically versioned module for it (its
own `go.mod` pins `k8s.io/*` at `v0.0.0` behind `replace` directives). A thin
second module would still drag that tree into every `go mod download`.

The released `e2e.test` binary is the same code, built and tested by the release
it ships with, and costs this repository one shell script. `run.sh` matches it to
the cluster's own server version, because version skew between `e2e.test` and the
API server produces failures that look exactly like driver defects. Downloads go
over `https://dl.k8s.io` with `--proto '=https' --tlsv1.2`.

## The driver definitions

One YAML per protocol, because the protocols genuinely differ:

|                                        | `nfs`          | `iscsi`       | `nvme`        | `smb`          |
| -------------------------------------- | -------------- | ------------- | ------------- | -------------- |
| `block` (volumeMode: Block)            | no             | **yes**       | **yes**       | no             |
| `RWX`                                  | **yes**        | no            | no            | **yes** ¹      |
| `fsGroup`                              | no             | **yes**       | **yes**       | no             |
| `SupportedFsType`                      | driver default | `ext4`, `xfs` | `ext4`, `xfs` | driver default |
| `onlineExpansion` / `offlineExpansion` | yes / no       | yes / no      | yes / no      | yes / no       |

¹ see "Known gaps" — this flag is deliberately set to the _storage's_ real
capability and currently disagrees with the driver.

Every flag carries a comment in the YAML naming the code it was read from, and
every flag whose value is a judgement call says so in capitals. The rule the
files follow: **getting the flags right matters more than getting them all on.**
A flag claiming a capability we do not have produces a false failure; a flag
switched off to make tests pass is a lie.

`external_test.go` re-asserts the non-judgement flags against the driver's own
constants on every `go test ./...` — no cluster, no appliance, about a second —
so a capability that changes in `internal/` and not here fails immediately, and
so does a mistyped upstream field name (upstream unmarshals these files
_strictly_, and would otherwise reject the file an hour into a live run).

## The skip list

It lives in `run.sh`, and **every entry carries a reason**. "It fails" is not a
reason: an entry is either a genuine non-capability, naming which, or a cost,
in which case it is opt-in-able rather than permanent.

| Skip                                   | Why                                                                                                                                                                                                                                                                                                                                                                   |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `\[Disruptive\]`                       | Reboots nodes, kills kubelet, severs the network. Needs a throwaway cluster and SSH to every node. It is also where an appliance-side fence would be proven, and this driver has none yet: `ControllerUnpublishVolume` is a no-op and the `CSIDriver` sets `attachRequired: false`. That is plan task 5 — a tracked gap, not something a skip list should paper over. |
| `\[Feature:.*\]`, `\[FeatureGate:.*\]` | Guarded by alpha/beta gates that must be enabled on the API server and kubelet. A property of the cluster, not the driver. Enable the gates and drop the entry to widen coverage deliberately.                                                                                                                                                                        |
| `\[Flaky\]`                            | Upstream marks these as intermittent in its own CI. A known-flaky test cannot distinguish a regression from noise, which is the only thing this harness is for.                                                                                                                                                                                                       |
| `\[Slow\]` _(opt-in)_                  | Upstream's marker for tests measured in tens of minutes. Dropped by `TRUENAS_E2E_INCLUDE_HEAVY=true`.                                                                                                                                                                                                                                                                 |
| `volume limits` _(opt-in)_             | `volumeLimits: true` is truthful — `NodeGetInfo` advertises `MaxVolumesPerNode = 128` — and the suite takes it literally, provisioning 128 zvols or datasets **per node** on a live appliance. A cost, not a gap. Dropped by `TRUENAS_E2E_INCLUDE_HEAVY=true`.                                                                                                        |

Note what is **not** in that list. Nothing is skipped for `volumeMode: Block` on
NFS, for RWX on iSCSI, for `fsGroup`, or for offline expansion. Those are
expressed as capability flags in the driver definition, which is the honest
place for them — the framework then never generates the test at all, and the
reason lives next to the flag.

## What a clean run means

A clean run is `SUCCESS! -- N Passed | 0 Failed | S Skipped`, with the JUnit XML
in `$TRUENAS_E2E_REPORTDIR`, and it means:

- Every capability the driver **claims** in `identity.go`, `controller.go` and
  `node.go` behaves the way Kubernetes expects it to, end to end, through the
  real sidecars and the real kubelet.
- Provisioning, staging, publishing, expansion, snapshot, restore and clone are
  idempotent under the CO's retries — the suite retries deliberately.
- Topology actually schedules: pods land on nodes that can mount the volume.
- Teardown is complete. The suite deletes every namespace it creates; a clean
  run that leaves datasets, zvols, extents, targets or subsystems behind on the
  appliance is **not** clean, and `test/integration`'s teardown helpers plus a
  look at the pool are the check for that.

What it does **not** mean: that the skipped areas work. A clean run with the
default skip list says nothing about node failure and fencing, and nothing about
the feature-gated paths.

Run each protocol separately. There is no combined result, deliberately: a
single pass/fail across four protocols hides which one regressed.

## Known gaps this harness makes visible

- **SMB has no node data path.** The backend provisions correctly (verified
  against TrueNAS 25.10.6), but `internal/node/node.go` knows only `nfs`,
  `iscsi` and `nvme`, so `NodeStageVolume` fails with `InvalidArgument`; and
  no node publishes a `csi.truenas.watteel.com/smb` topology label while
  `requiredTopology()` demands exactly that segment, so an SMB PVC is
  unschedulable before it is unmountable. `README.md` at the repository root
  already calls SMB a deferred protocol. `run.sh` refuses to run the SMB
  definition unless `TRUENAS_E2E_ALLOW_SMB=true`; the file is checked in so the
  capability claims are written from the backend's real behaviour rather than
  reconstructed later.
- **SMB is refused RWX by the controller.** `supportsAccessMode()` in
  `internal/csi/controller.go` special-cases only `"nfs"` and drops SMB into the
  default branch, which rejects every `MULTI_NODE_*` mode. An SMB share genuinely
  serves many nodes at once, so `testdriver-smb.yaml` sets `RWX: true` — the
  storage's real capability — and the suite will fail loudly on the controller
  bug rather than encode it as fact. The fix belongs in the controller.
