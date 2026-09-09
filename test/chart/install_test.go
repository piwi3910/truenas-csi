package chart_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requireInstallEnv gates the install/upgrade test on a real cluster, a real
// appliance, and an image already present on the nodes.
func requireInstallEnv(t *testing.T) (image, endpoint, apiKey, username, pool, parent, server string) {
	t.Helper()
	image = os.Getenv("TRUENAS_CHART_IMAGE")
	endpoint = os.Getenv("TRUENAS_ENDPOINT")
	apiKey = os.Getenv("TRUENAS_API_KEY")
	if image == "" || endpoint == "" || apiKey == "" {
		t.Skip("TRUENAS_CHART_IMAGE, TRUENAS_ENDPOINT and TRUENAS_API_KEY must be set " +
			"to run the chart install/upgrade test against a real cluster")
	}
	for _, bin := range []string{"helm", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not on PATH", bin)
		}
	}
	username = envOr("TRUENAS_USERNAME", "truenas_admin")
	pool = envOr("TRUENAS_POOL", "Pool0")
	parent = envOr("TRUENAS_PARENT", "csi-chart")
	server = envOr("TRUENAS_DATA_ADDRESS", "")
	return
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func run(t *testing.T, timeout time.Duration, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func mustRun(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()
	out, err := run(t, timeout, name, args...)
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return out
}

// TestChartInstallAndUpgrade installs the chart on a real cluster, binds a PVC,
// mounts it in a pod, then upgrades the release and asserts the running pod is
// neither restarted nor left with a broken mount.
//
// An upgrade that rolls the node plugin while a volume is mounted is the classic
// way a storage driver takes workloads down, so this is the check that matters.
func TestChartInstallAndUpgrade(t *testing.T) {
	image, endpoint, apiKey, username, pool, parent, server := requireInstallEnv(t)

	ns := fmt.Sprintf("truenas-csi-e2e-%d", time.Now().Unix()%100000)
	release := "truenas-csi"
	sc := ns + "-nfs"

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

	// Both workloads must actually be running, not merely created.
	waitFor(t, 5*time.Minute, "controller ready", func() bool {
		out, _ := run(t, time.Minute, "kubectl", "get", "deploy", "-n", ns,
			"-o", "jsonpath={.items[*].status.readyReplicas}")
		return strings.TrimSpace(out) != "" && !strings.Contains(out, "0")
	})
	waitFor(t, 5*time.Minute, "node plugin ready", func() bool {
		out, _ := run(t, time.Minute, "kubectl", "get", "ds", "-n", ns,
			"-o", "jsonpath={.items[*].status.numberReady}")
		n := strings.TrimSpace(out)
		return n != "" && n != "0"
	})

	// Bind a PVC and mount it in a pod.
	pvc := `
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: e2e-pvc, namespace: ` + ns + `}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: ` + sc + `
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: e2e-user, namespace: ` + ns + `}
spec:
  restartPolicy: Never
  containers:
  - name: c
    image: busybox:1.36
    command: ["sh","-c","echo chart-e2e > /data/f.txt; sleep 3600"]
    volumeMounts: [{name: v, mountPath: /data}]
  volumes:
  - name: v
    persistentVolumeClaim: {claimName: e2e-pvc}
`
	applyStdin(t, pvc)
	if out, err := run(t, 6*time.Minute, "kubectl", "wait", "-n", ns,
		"--for=condition=Ready", "pod/e2e-user", "--timeout=5m"); err != nil {
		diag(t, ns)
		t.Fatalf("the pod never became ready, so the volume did not mount: %v\n%s", err, out)
	}
	if out := mustRun(t, time.Minute, "kubectl", "exec", "-n", ns, "e2e-user",
		"--", "cat", "/data/f.txt"); !strings.Contains(out, "chart-e2e") {
		t.Fatalf("could not read back what was written to the volume: %q", out)
	}
	// Reading back what the pod itself wrote proves nothing about where it
	// landed: if the mount never reached the host, the pod reads and writes a
	// local directory and the check passes anyway. Confirm the bytes are really
	// on the appliance by looking at the volume from outside the pod.
	assertDataIsOnTheAppliance(t, ns, sc)

	uidBefore := mustRun(t, time.Minute, "kubectl", "get", "pod", "-n", ns, "e2e-user",
		"-o", "jsonpath={.metadata.uid}")
	restartsBefore := mustRun(t, time.Minute, "kubectl", "get", "pod", "-n", ns, "e2e-user",
		"-o", "jsonpath={.status.containerStatuses[0].restartCount}")

	// Upgrade the release while the volume is mounted.
	upgrade := append([]string{"upgrade", release, "./../../deploy/helm/truenas-csi",
		"-n", ns, "--wait", "--timeout", "5m", "--set", "logLevel=debug"}, set...)
	if out, err := run(t, 8*time.Minute, "helm", upgrade...); err != nil {
		diag(t, ns)
		t.Fatalf("helm upgrade failed: %v\n%s", err, out)
	}

	uidAfter := mustRun(t, time.Minute, "kubectl", "get", "pod", "-n", ns, "e2e-user",
		"-o", "jsonpath={.metadata.uid}")
	restartsAfter := mustRun(t, time.Minute, "kubectl", "get", "pod", "-n", ns, "e2e-user",
		"-o", "jsonpath={.status.containerStatuses[0].restartCount}")
	if uidBefore != uidAfter {
		t.Fatalf("the workload pod was recreated by the upgrade (uid %s -> %s)", uidBefore, uidAfter)
	}
	if restartsBefore != restartsAfter {
		t.Fatalf("the workload container restarted during the upgrade (%s -> %s)",
			restartsBefore, restartsAfter)
	}
	if out := mustRun(t, time.Minute, "kubectl", "exec", "-n", ns, "e2e-user",
		"--", "cat", "/data/f.txt"); !strings.Contains(out, "chart-e2e") {
		t.Fatalf("the mount was broken by the upgrade: %q", out)
	}

	// Tear the volume down through the driver so nothing is left on the appliance.
	// This is deliberately NOT generous: a healthy unpublish/unstage finishes in
	// seconds, and a minutes-long delete means teardown is failing and kubelet is
	// retrying. Raising this timeout would hide exactly the bug it caught once.
	mustRun(t, 2*time.Minute, "kubectl", "delete", "pod", "-n", ns, "e2e-user",
		"--wait=true", "--grace-period=30")
	mustRun(t, 2*time.Minute, "kubectl", "delete", "pvc", "-n", ns, "e2e-pvc", "--wait=true")
	waitFor(t, 3*time.Minute, "PV released", func() bool {
		out, _ := run(t, time.Minute, "kubectl", "get", "pv", "-o",
			"jsonpath={range .items[*]}{.spec.storageClassName}{\"\\n\"}{end}")
		return !strings.Contains(out, sc)
	})
}

func lastSegment(image string) string {
	if i := strings.LastIndex(image, ":"); i > 0 {
		return image[i+1:]
	}
	return "latest"
}

func applyStdin(t *testing.T, manifest string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s", err, out)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// diag prints enough state to explain a failure without a second run.
func diag(t *testing.T, ns string) {
	t.Helper()
	for _, args := range [][]string{
		{"get", "pods", "-n", ns, "-o", "wide"},
		{"get", "pvc,pv", "-n", ns},
		{"get", "events", "-n", ns, "--sort-by=.lastTimestamp"},
	} {
		out, _ := run(t, time.Minute, "kubectl", args...)
		t.Logf("kubectl %s:\n%s", strings.Join(args, " "), out)
	}
	out, _ := run(t, time.Minute, "kubectl", "logs", "-n", ns, "-l",
		"app.kubernetes.io/name=truenas-csi", "--all-containers", "--tail=60")
	t.Logf("driver logs:\n%s", out)
}

// assertDataIsOnTheAppliance mounts the volume's export directly on a node and
// checks the pod's file is visible there.
//
// This exists because a mount that never propagates to the host is invisible to
// every in-pod check: the workload happily writes and reads its own local
// directory. Only looking at the export from outside catches it.
func assertDataIsOnTheAppliance(t *testing.T, ns, sc string) {
	t.Helper()
	server := os.Getenv("TRUENAS_DATA_ADDRESS")
	if server == "" {
		t.Log("TRUENAS_DATA_ADDRESS unset: skipping the out-of-band check that " +
			"the data really reached the appliance")
		return
	}
	// Verify from the node the WORKLOAD landed on, not from a node named by the
	// environment.
	//
	// The export is fenced: ControllerPublishVolume grants exactly the node the
	// volume was published to, and nobody else. A verification pod pinned
	// elsewhere is a host outside the access list, and an NFS server answers
	// such a client with a bare "No such file or directory" — which reads like
	// the data is missing and is really the fence working correctly.
	node := strings.TrimSpace(mustRun(t, time.Minute, "kubectl", "get", "pod", "-n", ns,
		"e2e-user", "-o", "jsonpath={.spec.nodeName}"))
	if node == "" {
		t.Fatal("the workload pod reports no node, so there is nowhere to verify from")
	}
	pv := strings.TrimSpace(mustRun(t, time.Minute, "kubectl", "get", "pvc", "-n", ns,
		"e2e-pvc", "-o", "jsonpath={.spec.volumeName}"))
	export := strings.TrimSpace(mustRun(t, time.Minute, "kubectl", "get", "pv", pv,
		"-o", "jsonpath={.spec.csi.volumeAttributes.share}"))
	if export == "" {
		t.Fatalf("PV %s carries no share attribute, so the export path is unknown", pv)
	}

	name := fmt.Sprintf("verify-%d", time.Now().UnixNano()%1000000)
	script := fmt.Sprintf(`set -e
mkdir -p /tmp/%[1]s
mount -t nfs -o vers=4 %[2]s:%[3]s /tmp/%[1]s
echo "ON_APPLIANCE=$(cat /tmp/%[1]s/f.txt 2>/dev/null || echo MISSING)"
umount /tmp/%[1]s; rmdir /tmp/%[1]s`, name, server, export)

	manifest := fmt.Sprintf(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":%q,"namespace":"default"},
"spec":{"nodeName":%q,"restartPolicy":"Never","hostNetwork":true,
"tolerations":[{"operator":"Exists"}],
"volumes":[{"name":"host","hostPath":{"path":"/"}}],
"containers":[{"name":"c","image":"busybox:1.36",
"command":["chroot","/host","sh","-c",%q],
"securityContext":{"privileged":true},
"volumeMounts":[{"name":"host","mountPath":"/host"}]}]}}`, name, node, script)

	apply := exec.Command("kubectl", "apply", "-f", "-")
	apply.Stdin = strings.NewReader(manifest)
	if out, err := apply.CombinedOutput(); err != nil {
		t.Fatalf("creating the verification pod: %v\n%s", err, out)
	}
	defer func() { _ = exec.Command("kubectl", "delete", "pod", "-n", "default", name, "--wait=false").Run() }()

	waitFor(t, 3*time.Minute, "verification pod to finish", func() bool {
		out, _ := run(t, time.Minute, "kubectl", "get", "pod", "-n", "default", name,
			"-o", "jsonpath={.status.phase}")
		p := strings.TrimSpace(out)
		return p == "Succeeded" || p == "Failed"
	})
	logs, _ := run(t, time.Minute, "kubectl", "logs", "-n", "default", name)
	if !strings.Contains(logs, "ON_APPLIANCE=chart-e2e") {
		t.Fatalf("the pod's data is not on the appliance — the volume was mounted "+
			"somewhere the host cannot see, so the workload was writing to a local "+
			"directory.\nexport %s on %s gave:\n%s", export, server, logs)
	}
	t.Logf("verified out of band: the pod's data is really on %s:%s", server, export)
}
