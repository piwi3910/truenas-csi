// Package bundle holds no code. The test below guards the OLM bundle's
// contents, which are otherwise only checked by whoever pushes them to
// OperatorHub.
package bundle

import (
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	csvPath       = "manifests/truenas-csi-operator.clusterserviceversion.yaml"
	bundleCRDPath = "manifests/truenas.watteel.com_truenascsidrivers.yaml"
	sourceCRDPath = "../config/crd/bases/truenas.watteel.com_truenascsidrivers.yaml"
	annotations   = "metadata/annotations.yaml"
)

func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

// TestBundleCRDMatchesGenerated catches the bundle's CRD drifting from the one
// the operator's types actually produce. A bundle that installs a schema the
// operator does not implement is worse than no bundle: the API server accepts
// resources the reconciler cannot read.
func TestBundleCRDMatchesGenerated(t *testing.T) {
	inBundle := readYAML(t, bundleCRDPath)
	generated := readYAML(t, sourceCRDPath)

	a, err := yaml.Marshal(inBundle)
	if err != nil {
		t.Fatalf("marshal bundle CRD: %v", err)
	}
	b, err := yaml.Marshal(generated)
	if err != nil {
		t.Fatalf("marshal generated CRD: %v", err)
	}
	if string(a) != string(b) {
		t.Error("the bundle's CRD differs from config/crd/bases; run `make bundle`")
	}
}

// TestCSVDeclaresPrivilegedNodeAccess checks that the description an
// OperatorHub reader sees actually says the node plugin is privileged and why.
// Someone installing a CSI driver is granting host-level access to their nodes;
// finding that out from a pod spec after the fact is not consent.
func TestCSVDeclaresPrivilegedNodeAccess(t *testing.T) {
	csv := readYAML(t, csvPath)
	spec, ok := csv["spec"].(map[string]any)
	if !ok {
		t.Fatal("CSV has no spec")
	}
	description, _ := spec["description"].(string)
	if description == "" {
		t.Fatal("CSV has no description")
	}
	for _, want := range []string{"hostPID", "hostNetwork", "privileged", "iscsiadm", "/etc/iscsi"} {
		if !strings.Contains(description, want) {
			t.Errorf("the CSV description does not mention %q; an administrator would grant host access without being told", want)
		}
	}
	// And the reason plaintext endpoints are refused, since that is the failure
	// mode that destroys a credential rather than merely erroring.
	if !strings.Contains(description, "wss://") || !strings.Contains(description, "revokes") {
		t.Error("the CSV description does not explain that TrueNAS revokes an API key presented over plaintext")
	}
}

// TestCSVInstallModes checks the operator is offered only as a cluster-wide
// install. It owns a cluster-scoped CRD and creates CSIDrivers, ClusterRoles and
// StorageClasses; an OwnNamespace install would be a promise it cannot keep.
func TestCSVInstallModes(t *testing.T) {
	csv := readYAML(t, csvPath)
	spec := csv["spec"].(map[string]any)
	modes, ok := spec["installModes"].([]any)
	if !ok {
		t.Fatal("CSV declares no installModes")
	}
	supported := map[string]bool{}
	for _, m := range modes {
		entry := m.(map[string]any)
		supported[entry["type"].(string)], _ = entry["supported"].(bool)
	}
	if !supported["AllNamespaces"] {
		t.Error("AllNamespaces is not supported, so the operator cannot be installed at all")
	}
	for _, mode := range []string{"OwnNamespace", "SingleNamespace", "MultiNamespace"} {
		if supported[mode] {
			t.Errorf("%s is offered, but the operator creates cluster-scoped objects and cannot honour it", mode)
		}
	}
}

// TestBundleChannels checks the channel declaration OLM reads from the metadata
// matches the labels the bundle image is built with.
func TestBundleChannels(t *testing.T) {
	meta := readYAML(t, annotations)
	got, ok := meta["annotations"].(map[string]any)
	if !ok {
		t.Fatal("annotations.yaml has no annotations map")
	}
	channels, _ := got["operators.operatorframework.io.bundle.channels.v1"].(string)
	if channels == "" {
		t.Fatal("the bundle declares no channels")
	}
	def, _ := got["operators.operatorframework.io.bundle.channel.default.v1"].(string)
	if def == "" {
		t.Fatal("the bundle declares no default channel")
	}
	found := false
	for _, c := range strings.Split(channels, ",") {
		if strings.TrimSpace(c) == def {
			found = true
		}
	}
	if !found {
		t.Errorf("default channel %q is not one of %q", def, channels)
	}
	if def != "stable" {
		t.Errorf("default channel is %q; a storage operator should not put anyone on an untested build by default", def)
	}

	dockerfile, err := os.ReadFile("../bundle.Dockerfile")
	if err != nil {
		t.Fatalf("read bundle.Dockerfile: %v", err)
	}
	if !strings.Contains(string(dockerfile), "channels.v1="+channels) {
		t.Errorf("bundle.Dockerfile's channel labels disagree with metadata/annotations.yaml (%q)", channels)
	}
	if !strings.Contains(string(dockerfile), "channel.default.v1="+def) {
		t.Errorf("bundle.Dockerfile's default channel disagrees with metadata/annotations.yaml (%q)", def)
	}
}

// TestCSVVersionMatchesName keeps the CSV's name and version in step, which is
// what OLM uses to order an upgrade graph.
func TestCSVVersionMatchesName(t *testing.T) {
	csv := readYAML(t, csvPath)
	meta := csv["metadata"].(map[string]any)
	spec := csv["spec"].(map[string]any)
	name, _ := meta["name"].(string)
	version, _ := spec["version"].(string)
	if version == "" {
		t.Fatal("CSV has no version")
	}
	if want := "truenas-csi-operator.v" + version; name != want {
		t.Errorf("CSV name = %q, want %q: OLM orders upgrades by this name", name, want)
	}
}
