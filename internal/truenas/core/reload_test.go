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

// TestReloadCredentialsSwapsTheKeyWithoutARestart is the point of the whole
// mechanism on CORE: an API key rotated on the appliance is adopted by a
// RUNNING driver, including after the old key has already been rejected.
//
// The sequence is the real one. The appliance's key changes, the driver notices
// only when a call comes back 401 — which is terminal, so nothing further
// reaches the wire — and the rotation has to lift that as well as change the
// header, or the driver stays broken until somebody restarts it.
func TestReloadCredentialsSwapsTheKeyWithoutARestart(t *testing.T) {
	s := corefake.Start(t, corefake.Options{})
	c := dialFake(t, s)
	ctx := context.Background()

	if _, err := c.DatasetQuery(ctx, "Pool0/k8s/vol1"); err != nil {
		t.Fatalf("first call with the original key: %v", err)
	}

	s.SetAPIKey("2-rotated")
	if _, err := c.PoolQuery(ctx, "Pool0"); !errors.Is(err, truenas.ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed once the appliance stopped accepting the old key, got %v", err)
	}

	next := backendFor(s.URL())
	next.APIKey = "2-rotated"
	if err := c.ReloadCredentials(next); err != nil {
		t.Fatalf("ReloadCredentials: %v", err)
	}

	if _, err := c.DatasetQuery(ctx, "Pool0/k8s/vol1"); err != nil {
		t.Fatalf("the rotated key was not adopted: %v", err)
	}
}

// TestReloadCredentialsRefusesIdentityChanges: endpoint, flavour, pool and
// parent dataset are recorded in every PersistentVolume this driver created.
// Changing one in place would silently repoint live volumes at a different
// appliance or dataset, so it is refused and the operator is told to restart.
func TestReloadCredentialsRefusesIdentityChanges(t *testing.T) {
	s := corefake.Start(t, corefake.Options{})

	tests := []struct {
		name   string
		mutate func(*config.Backend)
		// want is a substring the refusal must name; empty means any refusal
		// will do, because more than one rule rejects the change.
		want string
	}{
		{"endpoint", func(b *config.Backend) { b.Endpoint = "https://other.example.com/api/v2.0" }, "endpoint"},
		// A flavour change is refused by validation before the identity check
		// even sees it: a "scale" backend on an https:// endpoint is not a
		// valid configuration at all. Either refusal is the right answer.
		{"flavour", func(b *config.Backend) { b.Flavour = "scale" }, ""},
		{"pool", func(b *config.Backend) { b.Pool = "Pool1" }, "pool"},
		{"parentDataset", func(b *config.Backend) { b.ParentDataset = "other" }, "parentDataset"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := dialFake(t, s)
			next := backendFor(s.URL())
			next.APIKey = "2-rotated"
			tc.mutate(&next)

			err := c.ReloadCredentials(next)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
			// The refusal must leave the client on the credential it had, not
			// half-way between two.
			if _, err := c.PoolQuery(context.Background(), "Pool0"); errors.Is(err, truenas.ErrAuthFailed) {
				t.Fatal("a refused reload disturbed the credential the client was using")
			}
		})
	}
}

// TestReloadCredentialsRefusesAPlaintextEndpoint is the rule that cost three API
// keys: TrueNAS revokes a key presented over plaintext. A rotated configuration
// is validated exactly as the startup one is, before any byte is sent.
func TestReloadCredentialsRefusesAPlaintextEndpoint(t *testing.T) {
	s := corefake.Start(t, corefake.Options{})
	c := dialFake(t, s)

	next := backendFor("http://nas.example.com/api/v2.0")
	next.APIKey = "2-rotated"
	if err := c.ReloadCredentials(next); err == nil {
		t.Fatal("a plaintext endpoint was accepted by a credential reload")
	}
}

// TestReloadCredentialsIsANoOpWhenNothingRotated: the kubelet re-projects a
// mounted Secret on its own schedule, and an unchanged Secret must not cost a
// pool of warm connections or clear a terminal auth failure that is still true.
func TestReloadCredentialsIsANoOpWhenNothingRotated(t *testing.T) {
	s := corefake.Start(t, corefake.Options{})
	c := dialFake(t, s)
	ctx := context.Background()

	s.SetAPIKey("2-rotated")
	if _, err := c.PoolQuery(ctx, "Pool0"); !errors.Is(err, truenas.ErrAuthFailed) {
		t.Fatalf("want ErrAuthFailed, got %v", err)
	}
	before := s.Requests()

	if err := c.ReloadCredentials(backendFor(s.URL())); err != nil {
		t.Fatalf("ReloadCredentials with an unchanged credential: %v", err)
	}
	if _, err := c.PoolQuery(ctx, "Pool0"); !errors.Is(err, truenas.ErrAuthFailed) {
		t.Fatalf("a no-op reload retried a rejected key; want ErrAuthFailed, got %v", err)
	}
	if s.Requests() != before {
		t.Fatalf("a no-op reload put %d further request(s) on the wire with a key the appliance "+
			"already rejected, which is how a key gets revoked", s.Requests()-before)
	}
}

// TestCoreSatisfiesTheReloaderContract pins the shape the driver's hot-reload
// path type-asserts. Losing it does not break a build — it makes every CORE
// backend silently fall back to "restart the controller", which is the bug this
// closed.
func TestCoreSatisfiesTheReloaderContract(t *testing.T) {
	// The assertion is the assignment: it fails to COMPILE if *Client stops
	// implementing the interface the hot-reload path type-asserts. Comparing it
	// to nil afterwards asserts nothing -- the value has a concrete type, so the
	// comparison is always false and staticcheck says so.
	var reloader interface {
		ReloadCredentials(config.Backend) error
	} = &Client{}

	// What can actually fail at run time is the type assertion the driver
	// performs, so that is what this exercises. A CORE backend that fails it
	// silently falls back to "restart the controller", which is the bug this
	// closed.
	var api any = reloader
	if _, ok := api.(interface {
		ReloadCredentials(config.Backend) error
	}); !ok {
		t.Fatal("a *core.Client no longer satisfies the reloader contract, so every " +
			"CORE backend would silently need a restart to pick up a rotated key")
	}
}
