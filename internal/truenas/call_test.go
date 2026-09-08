package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// clock is a breaker's time source under test control, so a reset timeout can
// be exercised without sleeping through it.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func testBreaker(threshold int, reset time.Duration) (*breaker, *clock) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	b := newBreaker(threshold, reset)
	b.now = c.now
	return b, c
}

// TestBreakerOpensAndRecovers is the core of the contract: it must open on a
// sustained failure, fail fast while open, admit exactly one probe when the
// timeout expires, and close on that probe's success.
func TestBreakerOpensAndRecovers(t *testing.T) {
	b, clk := testBreaker(3, 10*time.Second)

	for i := 0; i < 2; i++ {
		b.failure()
		if err := b.allow(); err != nil {
			t.Fatalf("the breaker opened after %d failures, threshold is 3: %v", i+1, err)
		}
	}
	b.failure()
	if !b.isOpen() {
		t.Fatal("the breaker did not open at its threshold")
	}
	err := b.allow()
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("an open breaker must reject calls, got %v", err)
	}

	// Still inside the reset window: rejected, and the caller is told when to
	// come back.
	clk.add(9 * time.Second)
	if err := b.allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("the breaker reopened early: %v", err)
	}

	// Timeout expired: exactly ONE caller becomes the probe.
	clk.add(2 * time.Second)
	if err := b.allow(); err != nil {
		t.Fatalf("the probe was not admitted: %v", err)
	}
	if err := b.allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("a second caller was admitted alongside the probe: a recovering " +
			"appliance must see one call, not a herd")
	}

	b.success()
	if b.isOpen() {
		t.Fatal("a successful probe must close the breaker")
	}
	if err := b.allow(); err != nil {
		t.Fatalf("a closed breaker rejected a call: %v", err)
	}
}

// TestBreakerProbeFailureReArmsTheSameTimeout: a failed probe must not extend
// the outage beyond one more reset window. A backed-off timeout is how a short
// appliance blip becomes minutes of failing fast after it has recovered.
func TestBreakerProbeFailureReArmsTheSameTimeout(t *testing.T) {
	b, clk := testBreaker(1, 10*time.Second)
	b.failure()

	clk.add(11 * time.Second)
	if err := b.allow(); err != nil {
		t.Fatalf("probe not admitted: %v", err)
	}
	b.failure() // the probe failed

	if err := b.allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatal("a failed probe must leave the breaker open")
	}
	clk.add(11 * time.Second)
	if err := b.allow(); err != nil {
		t.Fatalf("the reset window grew after a failed probe: %v", err)
	}
}

// TestBreakerCountsConsecutiveFailuresOnly: unrelated failures spread over
// hours must never add up to an outage.
func TestBreakerCountsConsecutiveFailuresOnly(t *testing.T) {
	b, _ := testBreaker(3, 10*time.Second)
	for i := 0; i < 10; i++ {
		b.failure()
		b.failure()
		b.success()
	}
	if b.isOpen() {
		t.Fatal("non-consecutive failures opened the breaker")
	}
}

// TestBreakerAbandonedProbeDoesNotWedgeIt: allow() hands out the probe slot
// before the call is made, so a caller that gives up in between must release it.
// Without that, one cancelled context while the breaker was half-open would
// reject every call for the life of the process.
func TestBreakerAbandonedProbeDoesNotWedgeIt(t *testing.T) {
	b, clk := testBreaker(1, 10*time.Second)
	b.failure()
	clk.add(11 * time.Second)

	if err := b.allow(); err != nil {
		t.Fatalf("probe not admitted: %v", err)
	}
	b.abandon() // the caller never reached the appliance

	if err := b.allow(); err != nil {
		t.Fatalf("the abandoned probe slot was never released: %v", err)
	}
	if !b.isOpen() {
		t.Fatal("abandoning a probe must not close the breaker: nothing was proven")
	}
}

func TestBreakerCanBeDisabled(t *testing.T) {
	b := newBreaker(-1, time.Second)
	for i := 0; i < 100; i++ {
		b.failure()
	}
	if b.isOpen() || b.allow() != nil {
		t.Fatal("a negative threshold must disable the breaker entirely")
	}
}

// TestCountsAsFailure pins the distinction that keeps ordinary operation from
// looking like an outage.
func TestCountsAsFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection closed", fmt.Errorf("%w during system.info", ErrConnClosed), true},
		{"dial failure", errors.New("dial nas1: connection refused"), true},
		{"too many concurrent", fmt.Errorf("%w (pool.dataset.query)", ErrTooManyConcurrent), true},
		{"method error", &CallError{Method: "pool.dataset.query", Code: -32001,
			Reason: "[ENOENT] dataset does not exist"}, false},
		{"wrapped method error", fmt.Errorf("looking up a dataset: %w",
			&CallError{Method: "pool.dataset.query"}), false},
		{"auth failure", fmt.Errorf("%w: AUTH_ERR", ErrAuthFailed), false},
		{"context cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, false},
		{"breaker open", fmt.Errorf("%w (retry in 3s)", ErrCircuitOpen), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countsAsFailure(tc.err); got != tc.want {
				t.Fatalf("countsAsFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func breakerBackend(url string, threshold int, reset string) config.Backend {
	return config.Backend{
		Name: "nas-breaker", Endpoint: url, Username: "truenas_admin", APIKey: "8-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
		BreakerThreshold: threshold, BreakerResetTimeout: reset,
	}
}

// TestClientBreakerOpensOnADeadApplianceAndRecovers exercises the breaker
// through a real client: a middleware that has stopped answering must stop
// being called, and a middleware that comes back must be used again.
func TestClientBreakerOpensOnADeadApplianceAndRecovers(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})

	c, err := Dial(context.Background(), breakerBackend(s.URL(), 2, "50ms"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// A method error is NOT a transport failure: the appliance answered.
	s.Handle("boom", func([]json.RawMessage) (any, error) {
		return nil, &fake.RPCError{Code: -32001, ErrName: "EINVAL", Reason: "[ENOENT] nope"}
	})
	for i := 0; i < 10; i++ {
		if _, err := c.Call(context.Background(), "boom"); err == nil {
			t.Fatal("the fake should have returned a method error")
		}
	}
	if c.breaker.isOpen() {
		t.Fatal("method-level errors opened the breaker: a healthy appliance " +
			"answering \"not found\" is not an outage")
	}

	// Now make the appliance unreachable. Failing calls must open the breaker
	// and then be refused without a dial.
	forceFailures(t, c)
	if !c.breaker.isOpen() {
		t.Fatal("a dead appliance did not open the breaker")
	}
	if _, err := c.Call(context.Background(), "system.info"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("an open breaker must fail fast, got %v", err)
	}

	// After the reset timeout, the probe reaches the (now healthy) fake and the
	// breaker closes.
	time.Sleep(80 * time.Millisecond)
	c.connMu.Lock()
	c.backend = breakerBackend(s.URL(), 2, "50ms")
	c.connMu.Unlock()
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("the breaker did not recover once the appliance answered again: %v", err)
	}
	if c.breaker.isOpen() {
		t.Fatal("a successful probe left the breaker open")
	}
}

// forceFailures points the client at an address nothing answers on and calls
// until the breaker opens.
func forceFailures(t *testing.T, c *Client) {
	t.Helper()
	c.connMu.Lock()
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()
	// 127.0.0.1:1 refuses immediately, so this fails fast rather than waiting
	// out the dial timeout.
	c.backend.Endpoint = "wss://127.0.0.1:1/api/current"
	c.connMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 10 && !c.breaker.isOpen(); i++ {
		_, _ = c.Call(ctx, "system.info")
	}
}

// TestReloadCredentialsSwapsTheKeyWithoutARestart is the point of the whole
// hot-reload path: a rotated API key must reach the appliance on a new
// connection, without the process restarting.
func TestReloadCredentialsSwapsTheKeyWithoutARestart(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	b := backendFor(s.URL())
	b.Name = "nas-reload"

	c, err := Dial(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatal(err)
	}
	authsBefore := s.AuthCount()

	next := b
	next.APIKey = "9-rotated"
	if err := c.ReloadCredentials(next); err != nil {
		t.Fatalf("a credential-only change must be accepted: %v", err)
	}
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatalf("call after a credential reload: %v", err)
	}
	if s.AuthCount() <= authsBefore {
		t.Fatal("the connection was not re-established: the new key never reached the appliance")
	}
	c.connMu.Lock()
	got := c.backend.APIKey
	c.connMu.Unlock()
	if got != "9-rotated" {
		t.Fatalf("the client still holds %q", got)
	}
}

// TestReloadCredentialsRefusesIdentityChanges: endpoint, flavour, pool and
// parent dataset describe WHICH appliance and dataset every PersistentVolume
// lives on. Swapping one under a running driver would silently repoint live
// volumes at a different array.
func TestReloadCredentialsRefusesIdentityChanges(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	b := backendFor(s.URL())
	b.Name = "nas-identity"
	c, err := Dial(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for _, tc := range []struct {
		name   string
		mutate func(*config.Backend)
		wantIn string
	}{
		{"endpoint", func(n *config.Backend) { n.Endpoint = "wss://other.example.com/api/current" }, "endpoint"},
		{"pool", func(n *config.Backend) { n.Pool = "Pool1" }, "pool"},
		{"parentDataset", func(n *config.Backend) { n.ParentDataset = "other" }, "parentDataset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := b
			tc.mutate(&next)
			err := c.ReloadCredentials(next)
			if err == nil {
				t.Fatal("this change must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("the error should name %s, got %v", tc.wantIn, err)
			}
			if _, err := c.Call(context.Background(), "system.info"); err != nil {
				t.Fatalf("a refused reload disturbed the live connection: %v", err)
			}
		})
	}
}

// TestReloadCredentialsRefusesAPlaintextEndpoint is the rule that cost three
// API keys, applied to the reload path: revalidation is not optional just
// because the driver already started successfully once.
func TestReloadCredentialsRefusesAPlaintextEndpoint(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	b := backendFor(s.URL())
	b.Name = "nas-plaintext"
	c, err := Dial(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	next := b
	next.Endpoint = "http://192.168.10.253/api/current"
	err = c.ReloadCredentials(next)
	if err == nil || !errors.Is(err, config.ErrInsecureTransport) {
		t.Fatalf("a plaintext endpoint must be refused on reload too, got %v", err)
	}
}

// TestReloadCredentialsIsANoOpWhenNothingRotated: a kubelet re-projection that
// changed nothing must not drop a healthy connection.
func TestReloadCredentialsIsANoOpWhenNothingRotated(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	b := backendFor(s.URL())
	b.Name = "nas-noop"
	c, err := Dial(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatal(err)
	}
	authsBefore := s.AuthCount()

	if err := c.ReloadCredentials(b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), "system.info"); err != nil {
		t.Fatal(err)
	}
	if s.AuthCount() != authsBefore {
		t.Fatal("an unchanged credential dropped and re-authenticated a healthy connection")
	}
}

// TestDialUsesTheRotatedCredential covers the case that matters most: an
// appliance that was unreachable while its key was rotated. The first dial
// after it comes back must present the NEW key, because presenting the old one
// gets it revoked.
func TestDialUsesTheRotatedCredential(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	b := backendFor(s.URL())
	b.Name = "nas-stale-copy"

	config.SetCredential(b.Name, config.Credential{
		Username: "truenas_admin", APIKey: "9-rotated", InsecureSkipVerify: true,
	})
	t.Cleanup(func() { config.ForgetCredential(b.Name) })

	c, err := Dial(context.Background(), b) // b still carries the OLD key
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.connMu.Lock()
	got := c.backend.APIKey
	c.connMu.Unlock()
	if got != "9-rotated" {
		t.Fatalf("Dial used %q: a stale copy of the config would present a superseded key", got)
	}
}

func TestLimiterAndBreakerDefaults(t *testing.T) {
	b := config.Backend{Name: "nas1"}
	if got := limiterFor(b).Limit(); float64(got) != DefaultRateLimit {
		t.Fatalf("default rate limit = %v, want %v", got, DefaultRateLimit)
	}
	if got := limiterFor(b).Burst(); got != DefaultRateBurst {
		t.Fatalf("default burst = %v, want %v", got, DefaultRateBurst)
	}
	br := breakerFor(b)
	if br.threshold != DefaultBreakerThreshold || br.resetAfter != DefaultBreakerReset {
		t.Fatalf("breaker defaults = %d/%v", br.threshold, br.resetAfter)
	}

	b.RateLimit = 7
	b.BreakerThreshold = 4
	b.BreakerResetTimeout = "3s"
	if got := limiterFor(b).Limit(); float64(got) != 7 {
		t.Fatalf("configured rate limit ignored: %v", got)
	}
	br = breakerFor(b)
	if br.threshold != 4 || br.resetAfter != 3*time.Second {
		t.Fatalf("configured breaker ignored: %d/%v", br.threshold, br.resetAfter)
	}
}
