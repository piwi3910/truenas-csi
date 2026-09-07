package backend

import (
	"context"
	"fmt"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	corefake "github.com/piwi3910/truenas-csi/internal/truenas/core/fake"
	scalefake "github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// TestRegistryPicksImplementationByFlavour proves the flavour is what selects
// the transport, and that one registry can serve both at once — an operator with
// a CORE box and a SCALE box should not need two drivers.
func TestRegistryPicksImplementationByFlavour(t *testing.T) {
	scale := scalefake.Start(t, scalefake.Options{})
	corev := corefake.Start(t, corefake.Options{})

	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas-scale": {
			Name: "nas-scale", Endpoint: scale.URL(), Username: "truenas_admin",
			APIKey: "8-secret", Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
		},
		"nas-default": {
			Name: "nas-default", Flavour: "", Endpoint: scale.URL(), Username: "truenas_admin",
			APIKey: "8-secret", Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
		},
		"nas-core": {
			Name: "nas-core", Flavour: "core", Endpoint: corev.URL(), Username: "root",
			APIKey: corefake.DefaultAPIKey, Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
		},
	}}
	cfg.Backends["nas-scale"] = withFlavour(cfg.Backends["nas-scale"], "scale")

	r, err := NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	for name, want := range map[string]string{
		"nas-scale":   "*truenas.Client",
		"nas-default": "*truenas.Client",
		"nas-core":    "*core.Client",
	} {
		c, err := r.Client(context.Background(), name)
		if err != nil {
			t.Fatalf("Client(%q): %v", name, err)
		}
		if got := fmt.Sprintf("%T", c); got != want {
			t.Errorf("backend %q: want %s, got %s", name, want, got)
		}
	}
}

func withFlavour(b config.Backend, f string) config.Backend {
	b.Flavour = f
	return b
}
