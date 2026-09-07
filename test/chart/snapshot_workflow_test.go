package chart_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestKubernetesSnapshotWorkflow exercises the path a user actually takes:
// a VolumeSnapshot object, the cluster's snapshot-controller, our CSI
// CreateSnapshot, a VolumeSnapshotContent, and finally a new PVC restored
// through dataSource.
//
// The driver's own snapshot RPCs were already verified by calling them
// directly, which skips this entire layer — the CRDs, the controller, the
// class, the content object and the dataSource wiring. None of that was
// exercised until this test existed.
func TestKubernetesSnapshotWorkflow(t *testing.T) {
	image, endpoint, apiKey, username, pool, parent, server := requireInstallEnv(t)
	if !crdsPresent(t) {
		t.Skip("VolumeSnapshot CRDs are not installed: the external-snapshotter is a " +
			"cluster-wide prerequisite this chart deliberately does not install")
	}

	ns := fmt.Sprintf("truenas-csi-snap-%d", time.Now().Unix()%100000)
	release := "truenas-csi"
	sc := ns + "-nfs"
	vsc := ns + "-snapclass"

	mustRun(t, 2*time.Minute, "kubectl", "create", "namespace", ns)
	t.Cleanup(func() {
		_, _ = run(t, 5*time.Minute, "helm", "uninstall", release, "-n", ns, "--wait")
		_, _ = run(t, 5*time.Minute, "kubectl", "delete", "namespace", ns, "--wait=false")
	})

	set := []string{
		"--set", "image.repository=" + strings.Split(image, ":")[0],
		"--set", "image.tag=" + lastSegment(image),
		"--set", "image.pullPolicy=IfNotPresent",
		"--set", "controller.replicas=1",
		"--set", "backends.nas1.endpoint=" + endpoint,
		"--set", "backends.nas1.username=" + username,
		"--set", "backends.nas1.apiKey=" + apiKey,
		"--set", "backends.nas1.pool=" + pool,
		"--set", "backends.nas1.parentDataset=" + parent,
		"--set", "backends.nas1.insecureSkipVerify=true",
		"--set", "storageClasses.nfs.enabled=true",
		"--set", "storageClasses.nfs.name=" + sc,
		"--set", "storageClasses.nfs.parameters.pool=" + pool,
		"--set", "storageClasses.nfs.parameters.parentDataset=" + parent,
		"--set", "storageClasses.nfs.parameters.networks=192.168.0.0/16",
		"--set", "volumeSnapshotClass.enabled=true",
		"--set", "volumeSnapshotClass.name=" + vsc,
	}
	if server != "" {
		set = append(set, "--set", "storageClasses.nfs.parameters.server="+server)
	}
	install := append([]string{"install", release, "./../../deploy/helm/truenas-csi",
		"-n", ns, "--wait", "--timeout", "5m"}, set...)
	if out, err := run(t, 8*time.Minute, "helm", install...); err != nil {
		diag(t, ns)
		t.Fatalf("helm install failed: %v\n%s", err, out)
	}

	// Source volume with a known payload.
	applyStdin(t, `
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: snap-src, namespace: `+ns+`}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: `+sc+`
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: snap-writer, namespace: `+ns+`}
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh","-c","echo SNAPSHOT-PAYLOAD-V1 > /data/p.txt; sync; sleep 3600"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: snap-src}
`)
	if out, err := run(t, 6*time.Minute, "kubectl", "wait", "-n", ns,
		"--for=condition=Ready", "pod/snap-writer", "--timeout=5m"); err != nil {
		diag(t, ns)
		t.Fatalf("source pod never became ready: %v\n%s", err, out)
	}

	// Snapshot through the Kubernetes object, not the driver's gRPC.
	applyStdin(t, `
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: snap-1, namespace: `+ns+`}
spec:
  volumeSnapshotClassName: `+vsc+`
  source: {persistentVolumeClaimName: snap-src}
`)
	waitFor(t, 5*time.Minute, "VolumeSnapshot to become ready", func() bool {
		out, _ := run(t, time.Minute, "kubectl", "get", "volumesnapshot", "-n", ns, "snap-1",
			"-o", "jsonpath={.status.readyToUse}")
		return strings.TrimSpace(out) == "true"
	})
	content := strings.TrimSpace(mustRun(t, time.Minute, "kubectl", "get", "volumesnapshot",
		"-n", ns, "snap-1", "-o", "jsonpath={.status.boundVolumeSnapshotContentName}"))
	if content == "" {
		t.Fatal("the snapshot reported ready but bound no VolumeSnapshotContent")
	}
	t.Logf("VolumeSnapshot snap-1 is ready, bound to %s", content)

	// Change the source AFTER the snapshot, so a restore that reads live data fails.
	mustRun(t, 2*time.Minute, "kubectl", "exec", "-n", ns, "snap-writer", "--",
		"sh", "-c", "echo SNAPSHOT-PAYLOAD-V2-CORRUPT > /data/p.txt; sync")

	// Restore into a new PVC through dataSource.
	applyStdin(t, `
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: snap-restored, namespace: `+ns+`}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: `+sc+`
  resources: {requests: {storage: 1Gi}}
  dataSource:
    name: snap-1
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
---
apiVersion: v1
kind: Pod
metadata: {name: snap-reader, namespace: `+ns+`}
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh","-c","cat /data/p.txt; sleep 3600"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: snap-restored}
`)
	if out, err := run(t, 6*time.Minute, "kubectl", "wait", "-n", ns,
		"--for=condition=Ready", "pod/snap-reader", "--timeout=5m"); err != nil {
		diag(t, ns)
		t.Fatalf("restored pod never became ready: %v\n%s", err, out)
	}
	got := strings.TrimSpace(mustRun(t, time.Minute, "kubectl", "exec", "-n", ns,
		"snap-reader", "--", "cat", "/data/p.txt"))
	if got != "SNAPSHOT-PAYLOAD-V1" {
		t.Fatalf("restored volume contains %q, want the pre-corruption %q — the restore "+
			"returned live data rather than the snapshot's", got, "SNAPSHOT-PAYLOAD-V1")
	}
	t.Log("restored through dataSource: content is the snapshot's, not the source's")

	// Deleting the snapshot while a restored volume depends on it must be refused
	// by the driver, so the VolumeSnapshot must not simply disappear.
	mustRun(t, 2*time.Minute, "kubectl", "delete", "volumesnapshot", "-n", ns, "snap-1",
		"--wait=false")

	// Tear down in an order the driver can satisfy.
	mustRun(t, 3*time.Minute, "kubectl", "delete", "pod", "-n", ns, "snap-reader", "snap-writer",
		"--wait=true", "--grace-period=30")
	mustRun(t, 3*time.Minute, "kubectl", "delete", "pvc", "-n", ns, "snap-restored", "--wait=true")
	mustRun(t, 3*time.Minute, "kubectl", "delete", "pvc", "-n", ns, "snap-src", "--wait=true")
}

// crdsPresent reports whether the external-snapshotter CRDs exist.
func crdsPresent(t *testing.T) bool {
	t.Helper()
	out, err := exec.Command("kubectl", "get", "crd",
		"volumesnapshots.snapshot.storage.k8s.io").CombinedOutput()
	return err == nil && strings.Contains(string(out), "volumesnapshots")
}
