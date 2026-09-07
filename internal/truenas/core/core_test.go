package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	corefake "github.com/piwi3910/truenas-csi/internal/truenas/core/fake"
)

func backendFor(endpoint string) config.Backend {
	return config.Backend{
		Name: "nas1", Flavour: "core", Endpoint: endpoint,
		Username: "root", APIKey: corefake.DefaultAPIKey,
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	}
}

func dialFake(t *testing.T, s *corefake.Server) *Client {
	t.Helper()
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestCoreRefusesPlaintextEndpoint is the credential guard. TrueNAS revokes an
// API key the moment it is presented over a plaintext transport, so an http://
// endpoint must be refused BEFORE any request is made — the same rule the SCALE
// client applies to ws://.
func TestCoreRefusesPlaintextEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://nas/api/v2.0",
		"HTTP://nas/api/v2.0",
		"ws://nas/api/current",
		"wss://nas/api/current",
		"nas/api/v2.0",
		"",
	} {
		c, err := Dial(context.Background(), backendFor(endpoint))
		if err == nil {
			_ = c.Close()
			t.Fatalf("endpoint %q: Dial must refuse a non-https endpoint", endpoint)
		}
		if !errors.Is(err, config.ErrInsecureTransport) {
			t.Errorf("endpoint %q: want ErrInsecureTransport, got %v", endpoint, err)
		}
	}
}

// TestCoreAuthFailureIsTerminal proves a 401 is never retried. A retry loop that
// re-presents a rejected key is exactly what burns the credential; the SCALE
// client makes login failure terminal and CORE must do the same.
func TestCoreAuthFailureIsTerminal(t *testing.T) {
	s := corefake.Start(t, corefake.Options{RejectAuth: true})
	c := dialFake(t, s)

	for i := 0; i < 3; i++ {
		_, err := c.DatasetQuery(context.Background(), "Pool0/k8s/vol1")
		if !errors.Is(err, truenas.ErrAuthFailed) {
			t.Fatalf("call %d: want ErrAuthFailed, got %v", i, err)
		}
	}
	// One request reached the appliance; every later call short-circuited.
	if n := s.Requests(); n != 1 {
		t.Fatalf("a rejected key must be presented exactly once, got %d requests", n)
	}
}

// TestCoreDatasetQueryAbsentReturnsNil pins CSI idempotency: a missing dataset
// is (nil, nil), whether CORE reports it as an empty list or as a 404.
func TestCoreDatasetQueryAbsentReturnsNil(t *testing.T) {
	t.Run("empty list", func(t *testing.T) {
		s := corefake.Start(t, corefake.Options{})
		c := dialFake(t, s)
		ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/nope")
		if err != nil || ds != nil {
			t.Fatalf("want (nil,nil), got (%+v,%v)", ds, err)
		}
	})
	t.Run("404 not found", func(t *testing.T) {
		s := corefake.Start(t, corefake.Options{QueryNotFound: true})
		c := dialFake(t, s)
		ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/nope")
		if err != nil || ds != nil {
			t.Fatalf("a 404 must not be an error, got (%+v,%v)", ds, err)
		}
	})
	t.Run("deleting an absent dataset succeeds", func(t *testing.T) {
		s := corefake.Start(t, corefake.Options{})
		c := dialFake(t, s)
		if err := c.DatasetDelete(context.Background(), "Pool0/k8s/nope", true, true); err != nil {
			t.Fatalf("deleting an absent dataset must succeed, got %v", err)
		}
	})
}

// TestCoreJobPolling covers the one asynchronous call the driver makes.
// filesystem.setperm answers with an integer job id which has to be polled
// through core/get_jobs; returning before the job finishes would hand a pod a
// dataset it still cannot write to.
func TestCoreJobPolling(t *testing.T) {
	t.Run("succeeds after polling", func(t *testing.T) {
		s := corefake.Start(t, corefake.Options{JobPollsBeforeDone: 2})
		c := dialFake(t, s)
		if err := c.SetPerm(context.Background(), "/mnt/Pool0/k8s/vol1", "0770", 1000, 1000); err != nil {
			t.Fatalf("SetPerm: %v", err)
		}
		if n := s.PathCount("core/get_jobs"); n < 2 {
			t.Fatalf("want the job polled at least twice, got %d polls", n)
		}
	})
	t.Run("a failed job is an error", func(t *testing.T) {
		s := corefake.Start(t, corefake.Options{JobFails: true})
		c := dialFake(t, s)
		err := c.SetPerm(context.Background(), "/mnt/Pool0/k8s/vol1", "0770", 1000, 1000)
		if err == nil || !strings.Contains(err.Error(), "FAILED") {
			t.Fatalf("want a FAILED job error, got %v", err)
		}
	})
}

// TestCoreEndpointNormalisation lets an operator configure either the bare
// appliance URL or the full API root without changing behaviour.
func TestCoreEndpointNormalisation(t *testing.T) {
	s := corefake.Start(t, corefake.Options{})
	for _, endpoint := range []string{s.URL(), s.URL() + "/api/v2.0", s.URL() + "/api/v2.0/"} {
		c, err := Dial(context.Background(), backendFor(endpoint))
		if err != nil {
			t.Fatalf("endpoint %q: %v", endpoint, err)
		}
		if _, err := c.DatasetQuery(context.Background(), "Pool0/k8s/nope"); err != nil {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
		_ = c.Close()
	}
}
