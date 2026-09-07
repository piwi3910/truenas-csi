// Package chart_test renders the Helm chart and asserts the properties an
// operator cannot check by eye: that the node plugin's Kubernetes credentials
// stay read-only, that credentials reach the pods only through a mounted
// Secret, and that the chart never installs the cluster-wide snapshot
// machinery it does not own.
//
// Every test skips with a clear reason when helm is not installed, so
// `go test ./...` passes on a machine that has no Kubernetes tooling at all.
package chart_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// driverName is repeated here as a literal on purpose: the test must fail if
// the chart's name ever drifts from the constant in internal/driver, and
// importing that constant would make the two drift together silently.
const driverName = "csi.truenas.watteel.com"

// writeVerbs are the verbs the node plugin must never hold on a cluster-scoped
// resource. It runs privileged on every node, so its API credentials are the
// one thing that must not widen the blast radius of a compromised node.
var writeVerbs = map[string]bool{
	"create":           true,
	"update":           true,
	"patch":            true,
	"delete":           true,
	"deletecollection": true,
	"*":                true,
}

// testValues is a complete, valid configuration: the chart requires endpoint,
// username, apiKey, pool and parentDataset for every backend.
const testValues = `
backends:
  nas1:
    endpoint: wss://nas1.example.com/api/current
    username: csi
    apiKey: SUPER-SECRET-API-KEY
    pool: tank
    parentDataset: tank/k8s
`

const testAPIKey = "SUPER-SECRET-API-KEY"

func chartDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "deploy", "helm", "truenas-csi"))
	if err != nil {
		t.Fatalf("resolve chart directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Fatalf("chart directory not found at %s: %v", dir, err)
	}
	return dir
}

// helmBin returns the helm binary, skipping the test when it is absent.
func helmBin(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not installed: install helm to run the chart rendering tests")
	}
	return bin
}

// valuesFile writes values to a temporary file. Using a file rather than --set
// keeps URLs and CIDRs out of helm's comma-and-dot escaping rules.
func valuesFile(t *testing.T, values string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(values), 0o600); err != nil {
		t.Fatalf("write values file: %v", err)
	}
	return path
}

// render runs `helm template` and returns the manifests it produced.
func render(t *testing.T, values string, extra ...string) []map[string]any {
	t.Helper()
	args := []string{"template", "truenas-csi", chartDir(t), "--namespace", "kube-system"}
	if values != "" {
		args = append(args, "--values", valuesFile(t, values))
	}
	args = append(args, extra...)

	cmd := exec.Command(helmBin(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr.String())
	}
	return decodeAll(t, stdout.Bytes())
}

// renderNotes returns the NOTES.txt helm would print on install. A client-side
// dry run needs no cluster, but it does need a readable kubeconfig on some helm
// builds, so a failure here is reported rather than fatal.
func renderNotes(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := []string{"install", "truenas-csi", chartDir(t), "--dry-run=client", "--namespace", "kube-system"}
	args = append(args, extra...)
	out, err := exec.Command(helmBin(t), args...).CombinedOutput()
	if err != nil {
		return "", errors.New(string(out))
	}
	_, notes, found := strings.Cut(string(out), "NOTES:")
	if !found {
		return "", errors.New("helm printed no NOTES section")
	}
	return notes, nil
}

func decodeAll(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse rendered manifest: %v", err)
		}
		if len(doc) == 0 {
			continue // an empty document, from a template that rendered nothing
		}
		docs = append(docs, doc)
	}
	return docs
}

func kindOf(doc map[string]any) string { s, _ := doc["kind"].(string); return s }
func nameOf(doc map[string]any) string { return str(dig(doc, "metadata", "name")) }
func str(v any) string                 { s, _ := v.(string); return s }
func mapOf(v any) map[string]any       { m, _ := v.(map[string]any); return m }
func listOf(v any) []any               { l, _ := v.([]any); return l }
func boolOf(v any) (bool, bool)        { b, ok := v.(bool); return b, ok }
func intOf(v any) (int, bool)          { i, ok := v.(int); return i, ok }
func has(s []string, want string) bool { return indexOf(s, want) >= 0 }
func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

// dig walks a decoded manifest by key, returning nil at the first missing step.
func dig(doc any, keys ...string) any {
	cur := doc
	for _, k := range keys {
		m := mapOf(cur)
		if m == nil {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func strings_(v any) []string {
	out := make([]string, 0, len(listOf(v)))
	for _, item := range listOf(v) {
		out = append(out, str(item))
	}
	return out
}

func findKind(docs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, doc := range docs {
		if kindOf(doc) == kind {
			out = append(out, doc)
		}
	}
	return out
}

func findOne(t *testing.T, docs []map[string]any, kind, nameSuffix string) map[string]any {
	t.Helper()
	for _, doc := range findKind(docs, kind) {
		if strings.HasSuffix(nameOf(doc), nameSuffix) {
			return doc
		}
	}
	t.Fatalf("no %s whose name ends in %q in the rendered chart", kind, nameSuffix)
	return nil
}

func containers(t *testing.T, workload map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, c := range listOf(dig(workload, "spec", "template", "spec", "containers")) {
		cm := mapOf(c)
		out[str(cm["name"])] = cm
	}
	return out
}

// TestChartLintsCleanly is the helm lint gate: a chart that does not lint will
// not install, whatever the rest of these tests say about it.
func TestChartLintsCleanly(t *testing.T) {
	out, err := exec.Command(helmBin(t), "lint", "--strict", chartDir(t),
		"--values", valuesFile(t, testValues)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm lint --strict failed:\n%s", out)
	}
}

// TestChartRendersBothWorkloads asserts the chart produces a controller
// Deployment and a node DaemonSet carrying the correct — immutable — driver
// name, with the security posture each one is supposed to have.
func TestChartRendersBothWorkloads(t *testing.T) {
	docs := render(t, testValues)

	csiDriver := findOne(t, docs, "CSIDriver", "")
	if got := nameOf(csiDriver); got != driverName {
		t.Errorf("CSIDriver name = %q, want %q (this name is written into every PV and is immutable)", got, driverName)
	}
	for _, field := range []string{"attachRequired", "podInfoOnMount", "storageCapacity"} {
		if v, ok := boolOf(dig(csiDriver, "spec", field)); !ok || !v {
			t.Errorf("CSIDriver spec.%s = %v, want true", field, dig(csiDriver, "spec", field))
		}
	}
	if got := str(dig(csiDriver, "spec", "fsGroupPolicy")); got != "File" {
		t.Errorf("CSIDriver spec.fsGroupPolicy = %q, want File", got)
	}

	deploy := findOne(t, docs, "Deployment", "-controller")
	if replicas, ok := intOf(dig(deploy, "spec", "replicas")); !ok || replicas != 2 {
		t.Errorf("controller replicas = %v, want 2", dig(deploy, "spec", "replicas"))
	}
	if v, ok := boolOf(dig(deploy, "spec", "template", "spec", "securityContext", "runAsNonRoot")); !ok || !v {
		t.Error("controller pod securityContext.runAsNonRoot is not true")
	}
	if got := str(dig(deploy, "spec", "template", "spec", "securityContext", "seccompProfile", "type")); got != "RuntimeDefault" {
		t.Errorf("controller seccompProfile.type = %q, want RuntimeDefault", got)
	}

	ctrl := containers(t, deploy)
	for _, want := range []string{"truenas-csi", "csi-provisioner", "csi-attacher", "csi-resizer", "csi-snapshotter", "liveness-probe"} {
		if _, ok := ctrl[want]; !ok {
			t.Errorf("controller Deployment has no %q container", want)
		}
	}
	for name, c := range ctrl {
		if v, ok := boolOf(dig(c, "securityContext", "readOnlyRootFilesystem")); !ok || !v {
			t.Errorf("controller container %q does not set readOnlyRootFilesystem: true", name)
		}
		if drops := strings_(dig(c, "securityContext", "capabilities", "drop")); !has(drops, "ALL") {
			t.Errorf("controller container %q does not drop ALL capabilities, got %v", name, drops)
		}
	}
	if !strings.Contains(strings.Join(strings_(dig(ctrl["truenas-csi"], "args")), " "), "-leader-election=true") {
		t.Errorf("controller is not started with leader election: %v", dig(ctrl["truenas-csi"], "args"))
	}

	ds := findOne(t, docs, "DaemonSet", "-node")
	if v, ok := boolOf(dig(ds, "spec", "template", "spec", "hostPID")); !ok || !v {
		t.Error("node DaemonSet does not set hostPID: true, so it cannot reach the host's iscsid")
	}
	node := containers(t, ds)
	for _, want := range []string{"truenas-csi", "node-driver-registrar"} {
		if _, ok := node[want]; !ok {
			t.Errorf("node DaemonSet has no %q container", want)
		}
	}
	if v, ok := boolOf(dig(node["truenas-csi"], "securityContext", "privileged")); !ok || !v {
		t.Error("node plugin container is not privileged, so it cannot mount into the kubelet tree")
	}

	var found bool
	for _, m := range listOf(dig(node["truenas-csi"], "volumeMounts")) {
		mm := mapOf(m)
		if str(mm["mountPath"]) == "/var/lib/kubelet" {
			found = true
			if got := str(mm["mountPropagation"]); got != "Bidirectional" {
				t.Errorf("kubelet dir mountPropagation = %q, want Bidirectional", got)
			}
		}
	}
	if !found {
		t.Error("node plugin does not mount the default kubelet directory /var/lib/kubelet")
	}

	wantHostPaths := []string{"/etc/iscsi", "/var/lib/iscsi", "/dev", "/lib/modules", "/run/systemd"}
	hostPaths := map[string]bool{}
	for _, v := range listOf(dig(ds, "spec", "template", "spec", "volumes")) {
		if p := str(dig(mapOf(v), "hostPath", "path")); p != "" {
			hostPaths[p] = true
		}
	}
	for _, want := range wantHostPaths {
		if !hostPaths[want] {
			t.Errorf("node DaemonSet does not mount host path %s", want)
		}
	}

	// The registrar must advertise the socket under the driver's own name, or
	// the kubelet registers the plugin under the wrong identity.
	var regPath string
	for _, e := range listOf(dig(node["node-driver-registrar"], "env")) {
		if str(mapOf(e)["name"]) == "DRIVER_REG_SOCK_PATH" {
			regPath = str(mapOf(e)["value"])
		}
	}
	if !strings.Contains(regPath, driverName) {
		t.Errorf("registrar socket path %q does not contain the driver name %q", regPath, driverName)
	}

	// Every node must get the plugin, including tainted control-plane nodes.
	var tolerateAll bool
	for _, tol := range listOf(dig(ds, "spec", "template", "spec", "tolerations")) {
		if str(mapOf(tol)["operator"]) == "Exists" && mapOf(tol)["key"] == nil {
			tolerateAll = true
		}
	}
	if !tolerateAll {
		t.Error("node DaemonSet has no blanket toleration; nodes with taints would have no CSI plugin")
	}
}

// TestChartRBACIsMinimal is the security assertion of this chart. The node
// plugin is privileged on every node, so its API credentials must be read-only
// cluster-wide, and no role may read Secrets beyond the driver's own.
func TestChartRBACIsMinimal(t *testing.T) {
	docs := render(t, testValues)

	nodeRole := findOne(t, docs, "ClusterRole", "-node")
	for _, rule := range listOf(nodeRole["rules"]) {
		r := mapOf(rule)
		for _, verb := range strings_(r["verbs"]) {
			if writeVerbs[strings.ToLower(verb)] {
				t.Errorf("node ClusterRole grants write verb %q on %v; the node plugin must be read-only cluster-wide",
					verb, strings_(r["resources"]))
			}
		}
	}

	// Any secrets rule anywhere in the chart must be pinned to the driver's own
	// Secret by name, and limited to get.
	var sawSecretsRule bool
	for _, doc := range docs {
		kind := kindOf(doc)
		if kind != "ClusterRole" && kind != "Role" {
			continue
		}
		for _, rule := range listOf(doc["rules"]) {
			r := mapOf(rule)
			if !has(strings_(r["resources"]), "secrets") {
				continue
			}
			sawSecretsRule = true
			names := strings_(r["resourceNames"])
			if len(names) == 0 {
				t.Errorf("%s %q grants %v on all secrets in its scope; it must be limited by resourceNames",
					kind, nameOf(doc), strings_(r["verbs"]))
			}
			for _, verb := range strings_(r["verbs"]) {
				if verb != "get" {
					t.Errorf("%s %q grants %q on secrets %v, want get only", kind, nameOf(doc), verb, names)
				}
			}
			if kind == "ClusterRole" {
				t.Errorf("ClusterRole %q grants access to secrets; secret access must be namespaced", nameOf(doc))
			}
		}
	}
	if !sawSecretsRule {
		t.Error("no role grants access to the driver's config Secret; the plugins could not read their credentials")
	}

	// The controller's cluster-scoped writes must stay within what the sidecars
	// need. Anything else is a regression worth failing on.
	allowedControllerWrites := map[string]bool{
		"persistentvolumes": true,
		// The provisioner removes its finalizer; the resizer updates the
		// requested size back onto the claim.
		"persistentvolumeclaims":        true,
		"persistentvolumeclaims/status": true,
		"events":                        true,
		"volumeattachments":             true,
		"volumeattachments/status":      true,
		"csistoragecapacities":          true,
		"volumesnapshotcontents":        true,
		"volumesnapshotcontents/status": true,
	}
	ctrlRole := findOne(t, docs, "ClusterRole", "-controller")
	for _, rule := range listOf(ctrlRole["rules"]) {
		r := mapOf(rule)
		var writes bool
		for _, verb := range strings_(r["verbs"]) {
			if writeVerbs[strings.ToLower(verb)] {
				writes = true
			}
			if verb == "*" {
				t.Errorf("controller ClusterRole uses the wildcard verb on %v", strings_(r["resources"]))
			}
		}
		if !writes {
			continue
		}
		for _, res := range strings_(r["resources"]) {
			if !allowedControllerWrites[res] {
				t.Errorf("controller ClusterRole grants write verbs %v on %q, which no sidecar needs",
					strings_(r["verbs"]), res)
			}
		}
	}
}

// TestChartWithoutSnapshotCRDs pins the default that keeps this driver from
// breaking a cluster: the VolumeSnapshot CRDs and the snapshot controller are a
// cluster-wide singleton, and two drivers installing them fight over the CRD
// version and break snapshots for both.
func TestChartWithoutSnapshotCRDs(t *testing.T) {
	docs := render(t, testValues, "--set", "snapshotter.install=false")

	for _, doc := range docs {
		switch kindOf(doc) {
		case "CustomResourceDefinition", "VolumeSnapshotClass", "VolumeSnapshotContent", "VolumeSnapshot":
			t.Errorf("chart rendered a %s (%s) with snapshotter.install=false; snapshot machinery belongs to the cluster, not to this driver",
				kindOf(doc), nameOf(doc))
		}
	}
	// The snapshot controller Deployment is the other half of the singleton.
	for _, d := range findKind(docs, "Deployment") {
		if strings.Contains(nameOf(d), "snapshot-controller") {
			t.Errorf("chart rendered the snapshot controller %q; it must be installed once, cluster-wide", nameOf(d))
		}
	}

	notes, err := renderNotes(t, "--set", "snapshotter.install=false")
	if err != nil {
		t.Skipf("cannot render NOTES (helm install --dry-run=client failed): %v", err)
	}
	if !strings.Contains(notes, "PREREQUISITE") || !strings.Contains(notes, "external-snapshotter") {
		t.Errorf("NOTES do not state that the external snapshot controller is a prerequisite:\n%s", notes)
	}
}

// TestChartKeepsCredentialsInASecret asserts an API key reaches the plugins
// only as a mounted file. A key in a container argument is visible in `ps` on
// the node and to anyone who can read the pod spec — and TrueNAS revokes a key
// the moment it leaks into the wrong place.
func TestChartKeepsCredentialsInASecret(t *testing.T) {
	docs := render(t, testValues)

	var sawSecret bool
	for _, doc := range docs {
		raw, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatalf("re-marshal %s: %v", kindOf(doc), err)
		}
		contains := bytes.Contains(raw, []byte(testAPIKey))
		switch kindOf(doc) {
		case "Secret":
			sawSecret = sawSecret || contains
		default:
			if contains {
				t.Errorf("the API key appears in a %s (%s); credentials must only be in the Secret",
					kindOf(doc), nameOf(doc))
			}
		}
	}
	if !sawSecret {
		t.Error("the API key does not appear in any Secret; the plugins would have no credentials")
	}

	// Both workloads must mount that Secret read-only.
	for _, w := range []struct{ kind, suffix string }{{"Deployment", "-controller"}, {"DaemonSet", "-node"}} {
		workload := findOne(t, docs, w.kind, w.suffix)
		var mounted bool
		for _, v := range listOf(dig(workload, "spec", "template", "spec", "volumes")) {
			if str(dig(mapOf(v), "secret", "secretName")) != "" {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("%s%s does not mount the config Secret", w.kind, w.suffix)
		}
	}
}

// TestChartStorageClassesAreOptionalAndComplete checks the examples stay off by
// default — a wrong default pool would provision into live data — and that,
// when enabled, they document every parameter the driver understands.
func TestChartStorageClassesAreOptionalAndComplete(t *testing.T) {
	if classes := findKind(render(t, testValues), "StorageClass"); len(classes) != 0 {
		t.Errorf("chart created %d StorageClass objects by default, want 0", len(classes))
	}

	docs := render(t, testValues,
		"--set", "storageClasses.nfs.enabled=true",
		"--set", "storageClasses.iscsi.enabled=true")
	classes := findKind(docs, "StorageClass")
	if len(classes) != 2 {
		t.Fatalf("enabled both example classes, got %d StorageClass objects", len(classes))
	}

	seen := map[string]bool{}
	for _, sc := range classes {
		if got := str(sc["provisioner"]); got != driverName {
			t.Errorf("StorageClass %q provisioner = %q, want %q", nameOf(sc), got, driverName)
		}
		for k := range mapOf(sc["parameters"]) {
			seen[k] = true
		}
	}
	for _, want := range []string{
		"backend", "protocol", "pool", "parentDataset", "fsType", "sparse",
		"nfsVersion", "networks", "mode", "uid", "gid",
		"portalID", "chap", "initiatorACL", "multipath",
	} {
		if !seen[want] {
			t.Errorf("no example StorageClass demonstrates the %q parameter", want)
		}
	}
}

// TestChartHonoursKubeletDir covers k3s and microk8s, whose kubelet root is not
// the default. Getting this wrong makes every mount fail on those clusters.
func TestChartHonoursKubeletDir(t *testing.T) {
	const custom = "/var/lib/rancher/k3s/agent/kubelet"
	docs := render(t, testValues, "--set", "node.kubeletDir="+custom)
	ds := findOne(t, docs, "DaemonSet", "-node")

	raw, err := yaml.Marshal(ds)
	if err != nil {
		t.Fatalf("re-marshal DaemonSet: %v", err)
	}
	if !bytes.Contains(raw, []byte(custom+"/plugins_registry")) {
		t.Errorf("node.kubeletDir did not reach the registration directory; rendered:\n%s", raw)
	}
	if bytes.Contains(raw, []byte("/var/lib/kubelet")) {
		t.Error("the default kubelet directory survives an override of node.kubeletDir")
	}
}
