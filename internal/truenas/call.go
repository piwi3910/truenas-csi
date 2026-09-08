package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"golang.org/x/time/rate"
)

// ErrCircuitOpen means the circuit breaker rejected a call without contacting
// the appliance, because the appliance has been failing.
//
// It is a fast, honest failure rather than another call queued against a
// middleware that is not answering. The CSI sidecars retry, so a call refused
// here is retried after the breaker's reset timeout by the layer that is
// supposed to do the retrying.
var ErrCircuitOpen = errors.New("truenas circuit breaker is open: the appliance is failing, calls are rejected without contacting it")

// limiterFor builds the per-connection rate limiter. A configured rate of zero
// means DefaultRateLimit; the burst is always DefaultRateBurst, because it
// exists to let an already-admitted fan-out through rather than to be tuned.
func limiterFor(b config.Backend) *rate.Limiter {
	r := b.RateLimit
	if r <= 0 {
		r = DefaultRateLimit
	}
	return rate.NewLimiter(rate.Limit(r), DefaultRateBurst)
}

// breakerFor builds the per-connection circuit breaker. A negative threshold
// disables it, which exists for the operator who would rather keep hammering a
// sick appliance than fail fast; zero means DefaultBreakerThreshold.
func breakerFor(b config.Backend) *breaker {
	threshold := b.BreakerThreshold
	if threshold == 0 {
		threshold = DefaultBreakerThreshold
	}
	reset := b.BreakerReset()
	if reset <= 0 {
		reset = DefaultBreakerReset
	}
	return newBreaker(threshold, reset)
}

// breaker is a three-state circuit breaker: closed, open, and half-open.
//
// CLOSED counts CONSECUTIVE transport-level failures and opens at threshold. A
// single success — including a call the appliance answered with an application
// error, which proves it is alive — resets the count to zero, so unrelated
// failures spread over an hour never accumulate into an outage.
//
// OPEN rejects every call for resetAfter, then admits exactly ONE probe. Admitting
// one and not a thundering herd is the whole point: the appliance's ceiling is
// 20 in-flight calls, and letting a backlog of waiting callers all retry the
// instant the timer expires is precisely the load that keeps a recovering
// middleware wedged.
//
// HALF-OPEN (probing) ends either way on the probe's result: success closes the
// breaker and clears the failure count; failure re-arms the same fixed reset
// timeout. The timeout is never backed off — see DefaultBreakerReset for why.
type breaker struct {
	threshold  int
	resetAfter time.Duration
	now        func() time.Time // test seam

	mu       sync.Mutex
	failures int
	open     bool
	openedAt time.Time
	probing  bool
}

// newBreaker returns a breaker, or a disabled one when threshold is negative.
func newBreaker(threshold int, reset time.Duration) *breaker {
	return &breaker{threshold: threshold, resetAfter: reset, now: time.Now}
}

// enabled reports whether this breaker ever rejects anything.
func (b *breaker) enabled() bool { return b != nil && b.threshold > 0 }

// allow reports whether a call may proceed. The returned error wraps
// ErrCircuitOpen and names how long is left on the timer.
func (b *breaker) allow() error {
	if !b.enabled() {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return nil
	}
	left := b.resetAfter - b.now().Sub(b.openedAt)
	if left > 0 {
		return fmt.Errorf("%w (retry in %s)", ErrCircuitOpen, left.Round(time.Millisecond))
	}
	if b.probing {
		// Another caller is already the probe. Everyone else keeps failing
		// fast so the recovering appliance sees one call, not a herd.
		return fmt.Errorf("%w (a probe is already in flight)", ErrCircuitOpen)
	}
	b.probing = true
	return nil
}

// success records a completed round trip: the appliance answered.
func (b *breaker) success() {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.open = false
	b.probing = false
}

// failure records a transport-level failure.
func (b *breaker) failure() {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.probing {
		// The half-open probe failed: stay open and re-arm the same timeout.
		b.probing = false
		b.openedAt = b.now()
		return
	}
	b.failures++
	if !b.open && b.failures >= b.threshold {
		b.open = true
		b.openedAt = b.now()
	}
}

// abandon gives up a half-open probe slot without judging the appliance.
//
// It exists because allow() hands out the probe BEFORE the call is made: if the
// caller then never reaches the appliance — its context ended while it waited
// on the rate limiter, say — nothing would ever record an outcome, and the
// breaker would sit in "a probe is in flight" forever, rejecting every call
// until the process restarted.
func (b *breaker) abandon() {
	if !b.enabled() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
}

// reset returns the breaker to its closed, no-failures state.
func (b *breaker) reset() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures, b.open, b.probing = 0, false, false
}

// isOpen reports whether the breaker is currently rejecting calls. It ignores
// the reset timer, so it describes state rather than admission.
func (b *breaker) isOpen() bool {
	if !b.enabled() {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.open
}

// countsAsFailure reports whether err says the APPLIANCE is unhealthy, as
// opposed to the request being wrong.
//
// This is the distinction that keeps the breaker from turning ordinary
// operation into an outage. A CallError means the middleware received the call,
// executed it and answered — "dataset does not exist" is a perfectly healthy
// appliance, and CreateVolume idempotency checks produce those by design.
// Context cancellation belongs to the caller, not the appliance. A rejected
// login is already terminal and must not also open a breaker that would then
// reject the very reconnect a rotated credential needs.
//
// What is left — dial failures, ErrConnClosed, call timeouts and -32000 — is
// exactly the set that means "stop calling this appliance for a moment".
func countsAsFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrCircuitOpen) {
		return false
	}
	var ce *CallError
	return !errors.As(err, &ce)
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

// Call invokes a middleware method and returns its raw result.
//
// A dropped connection is retried once, because the appliance closes idle
// sockets and a caller should not see that. An authentication failure is never
// retried — see ErrAuthFailed.
//
// Two admission checks run before the appliance is touched: the circuit
// breaker, which refuses outright while the appliance is failing, and the rate
// limiter. Both come before ensureConn, so a call the breaker rejects does not
// even dial.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	if params == nil {
		params = []any{}
	}
	if err := c.breaker.allow(); err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		// Wait, not Allow: backpressure should slow a caller down, not
		// manufacture an error the CSI layer would report as a volume failure.
		// The caller's context still bounds the wait.
		if err := c.limiter.Wait(ctx); err != nil {
			// This call never reached the appliance, so it says nothing about
			// the appliance's health — but it may be holding a half-open probe
			// slot, which must not leak.
			c.breaker.abandon()
			return nil, fmt.Errorf("%s: rate limiter: %w", method, err)
		}
		raw, err := c.call(ctx, method, params)
		if err == nil {
			c.breaker.success()
			return raw, nil
		}
		if countsAsFailure(err) {
			c.breaker.failure()
		} else {
			// The appliance answered, even if the answer was an error: that is
			// proof of life, and it closes a half-open breaker.
			c.breaker.success()
		}
		lastErr = err
		if errors.Is(err, ErrAuthFailed) || ctx.Err() != nil {
			return nil, err
		}
		if !errors.Is(err, ErrConnClosed) {
			return nil, err
		}
		// connection dropped: reconnect and try once more
	}
	return nil, lastErr
}

func (c *Client) call(ctx context.Context, method string, params []any) (json.RawMessage, error) {
	if err := c.ensureConn(ctx); err != nil {
		return nil, err
	}
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)

	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, ErrConnClosed
	}
	conn := c.conn
	c.nextID++
	id := c.nextID
	ch := make(chan *response, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	cleanup := func() {
		c.mu.Lock()
		delete(c.waiters, id)
		c.mu.Unlock()
	}

	c.writeMu.Lock()
	err := conn.WriteJSON(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	c.writeMu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: writing %s: %v", ErrConnClosed, method, err)
	}

	timer := time.NewTimer(callTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-timer.C:
		cleanup()
		return nil, fmt.Errorf("%s: timed out after %s", method, callTimeout)
	case r, ok := <-ch:
		if !ok || r == nil {
			return nil, fmt.Errorf("%w during %s", ErrConnClosed, method)
		}
		if r.Error != nil {
			ce := &CallError{Method: method, Code: r.Error.Code}
			if r.Error.Data != nil {
				ce.ErrName = r.Error.Data.ErrName
				ce.Reason = r.Error.Data.Reason
			}
			if ce.Code == -32000 {
				return nil, fmt.Errorf("%w (%s)", ErrTooManyConcurrent, method)
			}
			return nil, ce
		}
		return r.Result, nil
	}
}

// CallJSON invokes a method and unmarshals its result into out.
func (c *Client) CallJSON(ctx context.Context, out any, method string, params ...any) error {
	raw, err := c.Call(ctx, method, params...)
	if err != nil {
		return err
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode result: %w", method, err)
	}
	return nil
}
