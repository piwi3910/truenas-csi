package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

type stubBackend struct{ proto string }

func (s stubBackend) Protocol() string { return s.proto }
func (s stubBackend) Create(context.Context, CreateRequest) (*Volume, error) {
	return &Volume{}, nil
}
func (s stubBackend) Delete(context.Context, volume.ID) error { return nil }
func (s stubBackend) Expand(context.Context, volume.ID, int64) (int64, error) {
	return 0, nil
}
func (s stubBackend) PublishContext(context.Context, volume.ID) (map[string]string, error) {
	return map[string]string{}, nil
}

func init() {
	Register("stub", func(truenas.API, string, string) Backend { return stubBackend{"stub"} })
}

func cfgWith(t *testing.T, endpoints map[string]string) *config.Config {
	t.Helper()
	c := &config.Config{Backends: map[string]config.Backend{}, NodeID: "worker-21"}
	for name, url := range endpoints {
		c.Backends[name] = config.Backend{
			Name: name, Endpoint: url, Username: "truenas_admin", APIKey: "8-secret",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
		}
	}
	return c
}

// TestBackendSelectionAndIsolation proves the multi-appliance promise: an
// unknown backend is named clearly, and one dead appliance neither blocks
// startup nor stalls calls to a healthy one.
func TestBackendSelectionAndIsolation(t *testing.T) {
	good := fake.Start(t, fake.Options{})
	good.HandleValue("system.info", map[string]any{"version": "25.10.6"})

	cfg := cfgWith(t, map[string]string{
		"nas1": good.URL(),
		// A port nothing listens on: dialling this must fail, not hang forever.
		"nas2": "wss://127.0.0.1:9/api/current",
	})
	r, err := NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry must succeed even with an unreachable appliance: %v", err)
	}
	defer func() { _ = r.Close() }()

	if _, err := r.For(context.Background(), "nas3", "stub"); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("unknown backend: want ErrUnknownBackend, got %v", err)
	} else if err != nil && !contains(err.Error(), "nas3") {
		t.Errorf("error should name the missing backend: %v", err)
	}

	// The healthy appliance must answer while the dead one is being dialled.
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = r.Client(ctx, "nas2")
	}()

	start := time.Now()
	if _, err := r.For(context.Background(), "nas1", "stub"); err != nil {
		t.Fatalf("healthy backend must work while another is down: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("healthy backend took %s — a dead appliance is blocking it", d)
	}
	<-dead
}

func TestRegistryUnknownProtocol(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	r, err := NewRegistry(context.Background(), cfgWith(t, map[string]string{"nas1": s.URL()}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, err = r.For(context.Background(), "nas1", "smb")
	if !errors.Is(err, ErrUnsupportedProtocol) {
		t.Fatalf("want ErrUnsupportedProtocol, got %v", err)
	}
	if !contains(err.Error(), "smb") {
		t.Errorf("error should name the protocol: %v", err)
	}
}

func TestRegistryReusesOneClientPerBackend(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	r, err := NewRegistry(context.Background(), cfgWith(t, map[string]string{"nas1": s.URL()}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for i := 0; i < 5; i++ {
		if _, err := r.Client(context.Background(), "nas1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.AuthCount(); got != 1 {
		t.Fatalf("authenticated %d times for 5 lookups — the client must be reused", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
