package chart_test

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestChartPassesOnlyFlagsTheBinaryDefines catches a whole class of bug that
// otherwise only appears on a real cluster: the chart passing a flag the driver
// does not define, which makes every replica crash-loop at startup.
//
// Found exactly that way — the chart passed -leader-election to the driver,
// which belongs to the CSI sidecars, and the controller never started.
func TestChartPassesOnlyFlagsTheBinaryDefines(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm is not installed")
	}

	// The flags main.go defines, read from the source of truth: its own usage.
	out, err := exec.Command("go", "run", "../../cmd/truenas-csi", "-h").CombinedOutput()
	// -h exits non-zero by convention; the usage text is what matters.
	usage := string(out)
	if !strings.Contains(usage, "-mode") {
		t.Fatalf("could not read the driver's usage text: %v\n%s", err, usage)
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+-([a-zA-Z][-a-zA-Z0-9]*)`).FindAllStringSubmatch(usage, -1) {
		defined[m[1]] = true
	}
	if len(defined) < 3 {
		t.Fatalf("parsed only %d flags from the usage text, something is wrong:\n%s", len(defined), usage)
	}

	rendered, err := exec.Command("helm", "template", "t", "../../deploy/helm/truenas-csi",
		"--set", "backends.nas1.endpoint=wss://nas/api/current",
		"--set", "backends.nas1.username=u",
		"--set", "backends.nas1.apiKey=k",
		"--set", "backends.nas1.pool=Pool0",
		"--set", "backends.nas1.parentDataset=k8s",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, rendered)
	}

	// Single-dash args belong to the driver container; sidecars use double dashes.
	driverFlag := regexp.MustCompile(`(?m)^\s*-\s+-([a-zA-Z][-a-zA-Z0-9]*)`)
	var unknown []string
	for _, m := range driverFlag.FindAllStringSubmatch(string(rendered), -1) {
		name := m[1]
		if !defined[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("the chart passes flags the driver does not define, which crash-loops "+
			"every replica at startup: %v\ndriver defines: %v", unknown, keys(defined))
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
