# Task 21: Helm chart

Status: open
Created: 2026-09-07

## Description

Implements plan task 21 of `.procoder/plans/truenas-csi.md`, which in turn implements
`.procoder/specs/truenas-csi.md`. Read the plan's "Task 21: Helm chart" section before starting: it
carries the literal test code, the exact command for each step, and the interface names
neighbouring tasks depend on.

Files this task owns:

- `deploy/helm/truenas-csi/Chart.yaml` — chart metadata.
- `deploy/helm/truenas-csi/values.yaml` — defaults.
- `deploy/helm/truenas-csi/templates/controller.yaml` — Deployment with sidecars.
- `deploy/helm/truenas-csi/templates/node.yaml` — privileged DaemonSet.
- `deploy/helm/truenas-csi/templates/rbac.yaml` — ServiceAccounts, Roles, Bindings.
- `deploy/helm/truenas-csi/templates/csidriver.yaml` — CSIDriver object.
- `deploy/helm/truenas-csi/templates/storageclass.yaml` — optional example classes.
- `deploy/helm/truenas-csi/templates/_helpers.tpl` — name helpers.
- `test/chart/chart_test.go` — chart tests.

Interfaces it produces for later tasks:

- Values keys: `backends`, `image.repository`, `image.tag`, `snapshotter.install`,
  `node.kubeletDir` (default `/var/lib/kubelet`), `controller.replicas` (default 2)

Done means every step below is checked, the named tests pass, the procoder gate is clean,
and the work is committed with the message the plan specifies.

Tests introduced or exercised: TestChartWithoutSnapshotCRDs, TestChartInstallAndUpgrade, TestChartRBACIsMinimal.

## Acceptance criteria

- [ ] Write `TestChartWithoutSnapshotCRDs`: render the chart with `snapshotter.install=false` and no snapshot CRDs present, deploy against a kind cluster, and assert the controller becomes Ready, logs a line containing `snapshot support disabled`, and omits `CREATE_DELETE_SNAPSHOT` from `ControllerGetCapabilities`. Run `go test ./test/chart/` — expect FAIL with "chart directory not found".
- [ ] Write `TestChartInstallAndUpgrade`: install the chart on a kind cluster, bind a PVC against a fake backend, mount it in a pod, then `helm upgrade` with a changed image tag and assert the pod is never evicted and the mount stays readable throughout.
- [ ] Write `TestChartRBACIsMinimal` asserting the controller Role grants no verbs on `secrets` beyond `get` on its own named secret, and that the node Role grants no write verbs on cluster-scoped resources.
- [ ] Create the controller Deployment with `csi-provisioner`, `csi-attacher`, `csi-resizer`, `csi-snapshotter` and `livenessprobe` sidecars, leader election enabled, `replicas: 2`, `runAsNonRoot: true`, and a read-only root filesystem.
- [ ] Create the node DaemonSet: privileged, `hostPID: true`, mount propagation `Bidirectional` on the kubelet directory, host paths for `/etc/iscsi`, `/var/lib/iscsi`, `/dev`, and `/lib/modules` read-only, plus `node-driver-registrar`.
- [ ] Create the `CSIDriver` object with `attachRequired: true`, `podInfoOnMount: true`, `storageCapacity: true`, and `fsGroupPolicy: File`.
- [ ] Add a chart note printed on install stating that the snapshot controller is a prerequisite unless `snapshotter.install` is set.
- [ ] Run `go test ./test/chart/` — expect PASS.
- [ ] Commit: "deploy: helm chart for controller, node plugin and RBAC".

## Evidence

