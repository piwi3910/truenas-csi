package skew_test

import (
	"errors"
	"path/filepath"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/piwi3910/truenas-csi/operator/internal/skew"
)

func TestCheckRefusesOldSidecars(t *testing.T) {
	err := skew.Check("0.1.0", map[string]string{
		"provisioner": "registry.k8s.io/sig-storage/csi-provisioner:v3.0.0",
		"resizer":     "registry.k8s.io/sig-storage/csi-resizer:v1.12.0",
	})
	if !errors.Is(err, skew.ErrIncompatible) {
		t.Fatalf("err = %v, want a version-skew refusal", err)
	}
	if got := err.Error(); got == "" {
		t.Error("a refusal with no message leaves an administrator with nothing to act on")
	}
}

func TestCheckRefusesUnknownDriverVersion(t *testing.T) {
	if err := skew.Check("2.0.0", nil); !errors.Is(err, skew.ErrIncompatible) {
		t.Fatalf("err = %v, want a refusal: this operator does not know what a 2.x driver expects", err)
	}
}

func TestCheckAcceptsUnparseableDriverTag(t *testing.T) {
	// A development build tagged "main" is a deliberate act; refusing it would
	// make the operator unusable for the people writing the driver.
	if err := skew.Check("main", map[string]string{
		"provisioner": "registry.k8s.io/sig-storage/csi-provisioner:v5.1.0",
	}); err != nil {
		t.Fatalf("err = %v, want acceptance", err)
	}
}

func TestCheckRefusesUnreadableSidecarTag(t *testing.T) {
	err := skew.Check("0.1.0", map[string]string{
		"provisioner": "registry.k8s.io/sig-storage/csi-provisioner:latest",
	})
	if !errors.Is(err, skew.ErrIncompatible) {
		t.Fatalf("err = %v, want a refusal: a sidecar pinned to a floating tag cannot be reasoned about", err)
	}
}

// TestChartSidecarsSatisfyTheFloor is the test that catches the pins in the
// chart drifting below what the operator will allow. Without it, someone lowers
// a sidecar pin in values.yaml and discovers the operator refuses to install at
// all only on a real cluster.
func TestChartSidecarsSatisfyTheFloor(t *testing.T) {
	ch, err := loader.LoadDir(filepath.Join("..", "..", "..", "deploy", "helm", "truenas-csi"))
	if err != nil {
		t.Fatalf("load in-repo chart: %v", err)
	}
	sidecars := skew.SidecarsFromValues(ch.Values)
	if len(sidecars) == 0 {
		t.Fatal("no sidecar images read from the chart's values")
	}
	if err := skew.Check(ch.Metadata.AppVersion, sidecars); err != nil {
		t.Fatalf("the chart the operator ships would be refused by the operator: %v", err)
	}
}
