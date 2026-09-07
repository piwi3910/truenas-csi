package chart_test

import (
	"strings"
	"testing"
)

// TestHelmPodmonSidecarPresent pins the chart's side of the podmon extension:
// off by default (it opens an extra listener, which an operator must opt into),
// and when enabled it appears on both the node DaemonSet and the controller
// Deployment with a named port and the driver flag that serves it.
func TestHelmPodmonSidecarPresent(t *testing.T) {
	off := render(t, testValues)
	for _, spec := range []map[string]any{
		podSpec(findOne(t, off, "DaemonSet", "-node")),
		podSpec(findOne(t, off, "Deployment", "-controller")),
	} {
		if containerArgsContain(spec, "-podmon-addr") {
			t.Error("the podmon extension listener must be off by default")
		}
	}

	on := render(t, testValues+"\npodmon:\n  enabled: true\n  port: 9820\n")
	for _, target := range []struct{ kind, suffix string }{
		{"DaemonSet", "-node"}, {"Deployment", "-controller"},
	} {
		spec := podSpec(findOne(t, on, target.kind, target.suffix))
		if !containerArgsContain(spec, "-podmon-addr") {
			t.Errorf("%s%s does not start the podmon listener when it is enabled",
				target.kind, target.suffix)
		}
		if !containerPortNamed(spec, "podmon") {
			t.Errorf("%s%s exposes no named podmon port", target.kind, target.suffix)
		}
	}
}

func podSpec(doc map[string]any) map[string]any {
	return mapOf(dig(doc, "spec", "template", "spec"))
}

func driverContainer(spec map[string]any) map[string]any {
	for _, c := range listOf(spec["containers"]) {
		if str(mapOf(c)["name"]) == "truenas-csi" {
			return mapOf(c)
		}
	}
	return nil
}

func containerArgsContain(spec map[string]any, want string) bool {
	for _, a := range strings_(driverContainer(spec)["args"]) {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

func containerPortNamed(spec map[string]any, name string) bool {
	for _, p := range listOf(driverContainer(spec)["ports"]) {
		if str(mapOf(p)["name"]) == name {
			return true
		}
	}
	return false
}
