package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func validBackend() Backend {
	return Backend{
		Name: "nas1", Endpoint: "wss://nas/api/current", Username: "truenas_admin",
		APIKey: "8-secret", Pool: "Pool0", ParentDataset: "k8s",
	}
}

func TestRejectsPlaintextEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		wantErr  bool
	}{
		{"http://nas/api/current", true},
		{"ws://nas/api/current", true},
		{"https://nas", true},
		{"HTTP://nas", true},
		{"nas/api/current", true},
		{"", true},
		{"wss://nas/api/current", false},
		{"WSS://nas/api/current", false},
	} {
		b := validBackend()
		b.Endpoint = tc.endpoint
		c := &Config{Backends: map[string]Backend{"nas1": b}, NodeID: "n1"}
		err := c.Validate()
		if tc.wantErr && !errors.Is(err, ErrInsecureTransport) {
			t.Errorf("endpoint %q: want ErrInsecureTransport, got %v", tc.endpoint, err)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("endpoint %q: want nil, got %v", tc.endpoint, err)
		}
	}
}

func TestValidateRequiresBackendFields(t *testing.T) {
	for field, mutate := range map[string]func(*Backend){
		"endpoint":      func(b *Backend) { b.Endpoint = "" },
		"username":      func(b *Backend) { b.Username = "" },
		"apiKey":        func(b *Backend) { b.APIKey = "" },
		"pool":          func(b *Backend) { b.Pool = "" },
		"parentDataset": func(b *Backend) { b.ParentDataset = "" },
	} {
		b := validBackend()
		mutate(&b)
		c := &Config{Backends: map[string]Backend{"nas1": b}, NodeID: "n1"}
		err := c.Validate()
		if err == nil {
			t.Errorf("empty %s: want error, got nil", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("empty %s: error %q does not name the field", field, err)
		}
	}
}

func TestValidateRequiresBackendsAndNodeID(t *testing.T) {
	if err := (&Config{Backends: map[string]Backend{}, NodeID: "n1"}).Validate(); err == nil {
		t.Error("empty backends: want error")
	}
	c := &Config{Backends: map[string]Backend{"nas1": validBackend()}}
	if err := c.Validate(); err == nil {
		t.Error("empty nodeID: want error")
	}
}

func TestBackendStringRedactsKey(t *testing.T) {
	b := validBackend()
	b.APIKey = "8-jX9B9ugcrfTfOjY2YdZb01sAuq0RZKAAZp2k24xvrYI67hv3pb8exkJiF8BiAhxz"
	s := b.String()
	if strings.Contains(s, b.APIKey) {
		t.Fatalf("String() leaked the api key: %s", s)
	}
	if !strings.Contains(s, "[redacted]") {
		t.Fatalf("String() should mark the key redacted: %s", s)
	}
}

func TestLoadAppliesDefaultsAndValidates(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/good.yaml"
	if err := os.WriteFile(good, []byte(`
nodeID: worker-21
backends:
  nas1:
    endpoint: wss://192.168.10.253/api/current
    username: truenas_admin
    apiKey: 8-secret
    pool: Pool0
    parentDataset: k8s
`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(good)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Backends["nas1"].Name != "nas1" {
		t.Errorf("backend name should default to its map key, got %q", c.Backends["nas1"].Name)
	}
	if c.LogLevel != "info" || c.MetricsAddr != ":9090" || c.HealthAddr != ":9808" {
		t.Errorf("defaults not applied: %+v", c)
	}

	bad := dir + "/bad.yaml"
	if err := os.WriteFile(bad, []byte(`
nodeID: worker-21
backends:
  nas1:
    endpoint: http://192.168.10.253/api/current
    username: truenas_admin
    apiKey: 8-secret
    pool: Pool0
    parentDataset: k8s
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("Load with http endpoint: want ErrInsecureTransport, got %v", err)
	}
}
