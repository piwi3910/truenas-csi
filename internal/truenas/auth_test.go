package truenas

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

func backendFor(url string) config.Backend {
	return config.Backend{
		Name: "nas1", Endpoint: url, Username: "truenas_admin", APIKey: "8-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	}
}

func TestAuthFailureIsTerminal(t *testing.T) {
	s := fake.Start(t, fake.Options{RejectAuth: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, err := Dial(ctx, backendFor(s.URL()))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("Dial: want ErrAuthFailed, got %v", err)
	}
	if c != nil {
		t.Fatal("Dial returned a client despite failed auth")
	}
	if got := s.AuthCount(); got != 1 {
		t.Fatalf("auth attempted %d times, must be exactly 1 — retrying auth can revoke the key", got)
	}
}

func TestAuthExpiredIsTerminal(t *testing.T) {
	s := fake.Start(t, fake.Options{AuthResponseType: "EXPIRED"})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed for EXPIRED, got %v", err)
	}
	if c != nil {
		t.Fatal("client returned despite EXPIRED")
	}
	if s.AuthCount() != 1 {
		t.Fatalf("auth attempted %d times, want 1", s.AuthCount())
	}
}

func TestDialRefusesPlaintextEndpoint(t *testing.T) {
	b := backendFor("ws://127.0.0.1:1/api/current")
	if _, err := Dial(context.Background(), b); !errors.Is(err, config.ErrInsecureTransport) {
		t.Fatalf("Dial with ws:// must refuse before connecting, got %v", err)
	}
}

func TestDialSucceedsAndCalls(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	var out struct {
		Version string `json:"version"`
	}
	if err := c.CallJSON(context.Background(), &out, "system.info"); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out.Version != "25.10.6" {
		t.Fatalf("got %q", out.Version)
	}
}
