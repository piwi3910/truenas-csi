package chart_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

const crdName = "storageprotectiongroups.replication.truenas.io"

// TestReplicationCRDIsGatedOnTheFlag is the chart half of issue #10: the CRD
// existed in the tree and no template referenced it, so `kubectl apply` of a
// StorageProtectionGroup failed outright on a default install.
//
// Both directions matter. Installed when replication is on, because the
// reconciler cannot watch a kind the API server does not know; absent when it
// is off, because teaching a cluster a kind that nothing reconciles is how the
// object ends up applied with no controller behind it.
func TestReplicationCRDIsGatedOnTheFlag(t *testing.T) {
	on := render(t, testValues, "--set", "replication.enabled=true")
	if findCRD(on) == nil {
		t.Error("replication.enabled=true renders no CustomResourceDefinition " + crdName +
			": a StorageProtectionGroup could not even be applied")
	}

	off := render(t, testValues)
	if findCRD(off) != nil {
		t.Error("the default install renders " + crdName +
			": the cluster would learn a kind that nothing reconciles")
	}
}

// TestReplicationCRDIsUpgradeableAndSurvivesUninstall pins the two properties
// the choice of a template over Helm's crds/ directory was made for.
//
// A CRD in crds/ is installed once and never upgraded, so an operator who
// upgrades the chart keeps an old schema and the API server silently prunes
// every status field the new driver writes. A template is upgraded — at the
// cost of being deleted on uninstall, taking every StorageProtectionGroup in
// the cluster with it, which the resource-policy annotation opts out of.
func TestReplicationCRDIsUpgradeableAndSurvivesUninstall(t *testing.T) {
	crd := findCRD(render(t, testValues, "--set", "replication.enabled=true"))
	if crd == nil {
		t.Fatal("no CRD rendered")
	}
	if got := str(dig(crd, "metadata", "annotations", "helm.sh/resource-policy")); got != "keep" {
		t.Errorf("CRD helm.sh/resource-policy = %q, want keep: helm uninstall would delete the "+
			"CRD and every StorageProtectionGroup in the cluster with it", got)
	}

	// A chart-managed CRD may not also sit in the chart's crds/ directory:
	// Helm would install that copy unconditionally and then never upgrade it,
	// which is the failure this template exists to avoid.
	if entries, err := os.ReadDir(filepath.Join(chartDir(t), "crds")); err == nil && len(entries) > 0 {
		t.Errorf("the chart has a crds/ directory (%d entries); Helm installs it once and never "+
			"upgrades it, so the templated CRD would be shadowed by a schema that never changes",
			len(entries))
	}
}

// TestReplicationCRDMatchesTheCheckedInManifest keeps the rendered schema and
// the manifest an operator can apply by hand from drifting: the template reads
// the file, and this proves the read is faithful rather than a lossy re-encode.
func TestReplicationCRDMatchesTheCheckedInManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "files", "crds",
		"replication.truenas.io_storageprotectiongroups.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]any
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}

	crd := findCRD(render(t, testValues, "--set", "replication.enabled=true"))
	if crd == nil {
		t.Fatal("no CRD rendered")
	}
	if !reflect.DeepEqual(crd["spec"], onDisk["spec"]) {
		t.Error("the rendered CRD spec differs from files/crds/" +
			"replication.truenas.io_storageprotectiongroups.yaml; the chart would install a " +
			"different schema from the one the repository documents")
	}
}

// TestReplicationRBACIsGatedOnTheFlag keeps the replication grants out of a
// default install, the way the fencing grants are. TestChartRBACIsMinimal
// asserts the default controller ClusterRole and would start failing if these
// leaked into it.
func TestReplicationRBACIsGatedOnTheFlag(t *testing.T) {
	if got := replicationRules(findOne(t, render(t, testValues), "ClusterRole", "-controller")); got != 0 {
		t.Errorf("the default controller ClusterRole holds %d replication.truenas.io rules; "+
			"an install that never reconciles them must hold none", got)
	}
	on := findOne(t, render(t, testValues, "--set", "replication.enabled=true"),
		"ClusterRole", "-controller")
	if got := replicationRules(on); got == 0 {
		t.Error("replication.enabled=true grants nothing on replication.truenas.io; the " +
			"reconciler could not watch or status-update a single StorageProtectionGroup")
	}
}

// TestReplicationFlagsReachTheDriver pins the other end of the wiring: the
// reconciler starts only when main sees both flags, and refuses without the
// lease, so a chart that renders the CRD but no arguments would install a kind
// that still nothing reconciles.
func TestReplicationFlagsReachTheDriver(t *testing.T) {
	on := findOne(t, render(t, testValues, "--set", "replication.enabled=true"),
		"Deployment", "-controller")
	spec := podSpec(on)
	for _, want := range []string{"-replication", "-replication-lease="} {
		if !containerArgsContain(spec, want) {
			t.Errorf("the controller is not started with %s, so the reconciler never runs", want)
		}
	}

	off := findOne(t, render(t, testValues), "Deployment", "-controller")
	if containerArgsContain(podSpec(off), "-replication") {
		t.Error("the default install passes -replication; replication must be opt-in")
	}
}

func findCRD(docs []map[string]any) map[string]any {
	for _, doc := range docs {
		if kindOf(doc) == "CustomResourceDefinition" && nameOf(doc) == crdName {
			return doc
		}
	}
	return nil
}

func replicationRules(role map[string]any) int {
	var n int
	for _, rule := range listOf(role["rules"]) {
		if has(strings_(mapOf(rule)["apiGroups"]), "replication.truenas.io") {
			n++
		}
	}
	return n
}
