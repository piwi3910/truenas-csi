package chart_test

import (
	"strings"
	"testing"
)

// vacAllowlist is the closed set of ZFS properties the driver will change on a
// live volume, restated here on purpose. internal/csi enforces it; this test
// makes sure the chart's own examples cannot drift ahead of it and ship a class
// that every claim using it would be refused for.
var vacAllowlist = map[string]bool{
	"sync": true, "compression": true, "atime": true, "recordsize": true,
}

// TestChartVolumeAttributesClassIsOffByDefault.
//
// The feature is opt-in in three places at once, and all three have to stay
// off together: an example class that exists by default is one somebody
// attaches to a claim without reading that it can lose acknowledged writes, and
// a feature gate on a sidecar that the API server does not serve is a
// crash-loop rather than a no-op.
func TestChartVolumeAttributesClassIsOffByDefault(t *testing.T) {
	docs := render(t, testValues)

	if n := len(findKind(docs, "VolumeAttributesClass")); n != 0 {
		t.Errorf("chart created %d VolumeAttributesClass objects by default, want 0", n)
	}
	for _, rule := range listOf(findOne(t, docs, "ClusterRole", "-controller")["rules"]) {
		for _, res := range strings_(mapOf(rule)["resources"]) {
			if res == "volumeattributesclasses" {
				t.Error("the default install holds RBAC on volumeattributesclasses; " +
					"the grant must be rendered only when the feature is enabled")
			}
		}
	}
	for name, c := range containers(t, findOne(t, docs, "Deployment", "-controller")) {
		for _, arg := range strings_(c["args"]) {
			if strings.Contains(arg, "VolumeAttributesClass") {
				t.Errorf("sidecar %s carries %q by default; the gate must follow the "+
					"feature, not lead it", name, arg)
			}
		}
	}
}

// TestChartVolumeAttributesClassWiring.
//
// The failure this catches is silent, which is why it is worth a test: with the
// sidecar feature gate missing, a VolumeAttributesClass attached to a claim
// does nothing at all — no event, no error, no property change. Two sidecars
// need it for different halves of the feature, and the RBAC must be there or
// they cannot read the class they were told to apply.
func TestChartVolumeAttributesClassWiring(t *testing.T) {
	docs := render(t, testValues, "--set", "volumeAttributesClass.enabled=true")

	cs := containers(t, findOne(t, docs, "Deployment", "-controller"))
	for _, sidecar := range []string{"csi-provisioner", "csi-resizer"} {
		c, ok := cs[sidecar]
		if !ok {
			t.Fatalf("no %s container in the controller Deployment", sidecar)
		}
		gated := false
		for _, arg := range strings_(c["args"]) {
			if strings.HasPrefix(arg, "--feature-gates=") &&
				strings.Contains(arg, "VolumeAttributesClass=true") {
				gated = true
			}
		}
		if !gated {
			t.Errorf("%s does not enable the VolumeAttributesClass feature gate, so a class "+
				"on a claim would be ignored without any error: args %v",
				sidecar, strings_(c["args"]))
		}
	}
	// The provisioner's existing Topology gate has to survive being joined by
	// the new one: --feature-gates is a single flag and a second copy wins.
	topology := false
	for _, arg := range strings_(cs["csi-provisioner"]["args"]) {
		if strings.HasPrefix(arg, "--feature-gates=") && strings.Contains(arg, "Topology=true") {
			topology = true
		}
	}
	if !topology {
		t.Error("enabling VolumeAttributesClass dropped the provisioner's Topology gate, " +
			"which would stop the scheduler honouring accessible topology")
	}

	// Read-only, and only that: the driver must never be able to widen the set
	// of properties it may change by writing a class of its own.
	verbs := map[string]bool{}
	found := false
	for _, rule := range listOf(findOne(t, docs, "ClusterRole", "-controller")["rules"]) {
		r := mapOf(rule)
		for _, res := range strings_(r["resources"]) {
			if res != "volumeattributesclasses" {
				continue
			}
			found = true
			for _, v := range strings_(r["verbs"]) {
				verbs[v] = true
			}
		}
	}
	if !found {
		t.Fatal("no RBAC on volumeattributesclasses; the sidecars cannot read the class " +
			"a claim names")
	}
	for v := range verbs {
		if writeVerbs[v] {
			t.Errorf("the controller holds %q on volumeattributesclasses; read-only is enough", v)
		}
	}
}

// TestChartVolumeAttributesClassExamples checks the shipped examples against
// the driver's own rules, so a class that could only ever be refused cannot be
// shipped as an example of how to use the feature.
func TestChartVolumeAttributesClassExamples(t *testing.T) {
	docs := render(t, testValues,
		"--set", "volumeAttributesClass.enabled=true",
		"--set", "volumeAttributesClass.classes.fastWrites.enabled=true",
		"--set", "volumeAttributesClass.classes.durable.enabled=true",
		"--set", "volumeAttributesClass.classes.archive.enabled=true")

	classes := findKind(docs, "VolumeAttributesClass")
	if len(classes) != 3 {
		t.Fatalf("enabled all three example classes, got %d objects", len(classes))
	}
	seen := map[string]bool{}
	sawSyncDisabled := false
	for _, vac := range classes {
		if got := str(vac["driverName"]); got != driverName {
			t.Errorf("VolumeAttributesClass %q driverName = %q, want %q",
				nameOf(vac), got, driverName)
		}
		params := mapOf(vac["parameters"])
		if len(params) == 0 {
			t.Errorf("VolumeAttributesClass %q sets no parameters", nameOf(vac))
		}
		for k, v := range params {
			if !vacAllowlist[k] {
				t.Errorf("VolumeAttributesClass %q sets %q, which the driver refuses with "+
					"InvalidArgument — an example must be one that works", nameOf(vac), k)
			}
			seen[k] = true
			if k == "sync" && strings.EqualFold(str(v), "disabled") {
				sawSyncDisabled = true
			}
		}
	}
	// The examples exist to show the trade the benchmark makes concrete, so the
	// one that gives up durability has to be among them — and it has to be
	// documented where an operator reads it before enabling it.
	if !sawSyncDisabled {
		t.Error("no example class demonstrates sync=disabled, which is the reason this " +
			"capability exists")
	}
	for _, want := range []string{"sync", "compression", "atime", "recordsize"} {
		if !seen[want] {
			t.Errorf("no example class demonstrates the %q attribute", want)
		}
	}
}
