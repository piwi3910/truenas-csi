// Package core is the TrueNAS CORE implementation of truenas.API, speaking the
// legacy REST v2 API over HTTPS.
//
// UNVERIFIED AGAINST REAL HARDWARE. No CORE appliance was available while this
// was written: the routing and payload shapes come from the documented REST v2
// surface, and the only thing exercising them is the recorded fake in
// ./fake. The typed operations themselves are NOT reimplemented here — they are
// the same truenas.Ops the SCALE client uses, so the two flavours cannot drift
// in behaviour. What lives in this package is exactly the transport: how a
// middleware method name and its parameters become an HTTP request.
//
// If CORE misbehaves in the field, suspect this file — not the backends.
package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"golang.org/x/sync/semaphore"
)

const (
	apiPath        = "/api/v2.0"
	requestTimeout = 2 * time.Minute
)

// Client is a TrueNAS CORE appliance.
//
// CORE has no persistent session: every call is one HTTPS request carrying
// "Authorization: Bearer <apikey>". There is therefore no login, no reconnect
// and no notification stream — but the credential rule is identical, so a
// rejected key is terminal here too.
type Client struct {
	truenas.Ops

	// base and host are fixed at Dial. Both derive from the endpoint, which is
	// identity rather than credential and is refused by ReloadCredentials — so
	// caching them keeps Host and every request path lock-free.
	base string
	host string
	sem  *semaphore.Weighted

	// backend and http are guarded by mu, not fixed at Dial: a rotated
	// credential replaces the API key the next request presents, and a rotated
	// CA certificate or verification decision replaces the whole HTTP transport.
	// Every read of them therefore goes through a locked accessor, so a rotation
	// cannot be half-applied to a request already being built.
	mu       sync.Mutex
	backend  config.Backend
	http     *http.Client
	authFail error // terminal for the key that earned it; cleared by a rotation
	closed   bool
}

// Dial prepares a CORE client. It performs no request: there is no session to
// establish, and an unnecessary probe call would spend privilege for nothing.
func Dial(_ context.Context, b config.Backend) (*Client, error) {
	if b.NormalisedFlavour() != config.FlavourCORE {
		return nil, fmt.Errorf("backend %q has flavour %q, not %q",
			b.Name, b.NormalisedFlavour(), config.FlavourCORE)
	}
	// Refuse a plaintext endpoint BEFORE the key is ever sent: TrueNAS revokes
	// an API key presented over insecure transport, so a bad scheme destroys the
	// credential rather than merely failing.
	if err := truenas.ValidateBackend(b); err != nil {
		return nil, err
	}
	tc, err := truenas.TLSConfig(b)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("backend %q endpoint: %w", b.Name, err)
	}
	c := &Client{
		backend: b,
		base:    normaliseBase(b.Endpoint),
		host:    u.Hostname(),
		http:    httpClient(tc),
		sem:     semaphore.NewWeighted(truenas.MaxInFlight),
	}
	c.Transport = c
	return c, nil
}

// httpClient builds the HTTP client for one TLS configuration.
//
// A rotation that changes the CA certificate or the verification decision needs
// a NEW transport: an http.Transport caches connections it has already
// negotiated, so mutating its TLS config in place would leave live connections
// verified against the old trust decision.
func httpClient(tc *tls.Config) *http.Client {
	return &http.Client{
		Timeout:   requestTimeout,
		Transport: &http.Transport{TLSClientConfig: tc},
	}
}

// ReloadCredentials swaps this client's credentials for the ones in b, without
// restarting the driver.
//
// This is the CORE counterpart of truenas.Client.ReloadCredentials and makes
// the same distinction: username, API key, CA certificate and the
// verification decision are credential; endpoint, flavour, pool and parent
// dataset are IDENTITY, because every PersistentVolume this driver created
// records which appliance and dataset it lives on. Identity changes are refused
// here and reported by the caller.
//
// The full backend validation runs again, so the transport check that stops an
// API key being presented over plaintext applies to a rotated key exactly as it
// applies to the one the driver booted with.
//
// CORE has no session to re-establish: the credential rides on every request as
// a Bearer header, so the next call after this returns already presents the new
// key. What still has to be replaced is the HTTP client, because its pooled
// connections were negotiated against the previous TLS trust decision.
//
// UNVERIFIED: like the rest of this package, against real CORE hardware. What
// is exercised is the swap itself — which key the next request carries, which
// trust decision it is made under, and that a previously rejected key no longer
// blocks the client — against the recorded fake. What a hardware run would add
// is confirmation that CORE answers a rotated-but-not-yet-active key with 401
// rather than something the error mapping reads differently.
func (c *Client) ReloadCredentials(b config.Backend) error {
	if err := truenas.ValidateBackend(b); err != nil {
		return err
	}
	tc, err := truenas.TLSConfig(b)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.backend
	for _, f := range []struct{ name, old, new string }{
		{"endpoint", cur.Endpoint, b.Endpoint},
		{"flavour", cur.NormalisedFlavour(), b.NormalisedFlavour()},
		{"pool", cur.Pool, b.Pool},
		{"parentDataset", cur.ParentDataset, b.ParentDataset},
	} {
		if f.old != f.new {
			return fmt.Errorf("backend %q: %s cannot be changed without restarting the driver (%q -> %q)",
				cur.Name, f.name, f.old, f.new)
		}
	}
	if cur.Credentials().Equal(b.Credentials()) {
		// Nothing rotated. A kubelet re-projecting an unchanged Secret must not
		// cost a pool of warm connections.
		return nil
	}

	c.backend = b
	old := c.http
	c.http = httpClient(tc)
	// A rejected key is terminal for THAT key, not for the appliance. Without
	// this, one revoked credential would poison the client for the process's
	// lifetime and rotating the key — the entire point of this method — would
	// fix nothing.
	c.authFail = nil
	// Only after the new client is in place, so nothing in flight loses its
	// connection mid-request; idle sockets are all this drops.
	old.CloseIdleConnections()
	return nil
}

// current reports the backend this client is presenting right now, plus whether
// it is still usable. Everything on the request path reads the credential
// through here rather than from the struct, so a rotation is all-or-nothing
// from any one call's point of view.
func (c *Client) current() (b config.Backend, hc *http.Client, closed bool, authFail error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.backend, c.http, c.closed, c.authFail
}

// normaliseBase accepts either the bare appliance URL or one that already names
// the API root, so an operator's obvious guess works either way.
func normaliseBase(endpoint string) string {
	base := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(base, apiPath) {
		return base
	}
	return base + apiPath
}

// Host is the appliance's address, used as the default NFS/iSCSI data address.
//
// It is resolved once at Dial: the endpoint is identity, which a credential
// reload refuses to change, so nothing can make this stale.
func (c *Client) Host() string { return c.host }

// Close releases idle connections. There is no session to tear down.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.http.CloseIdleConnections()
	return nil
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

// Call maps a middleware method onto a REST v2 request and returns its result.
func (c *Client) Call(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	// One snapshot for the whole call: the credential, the transport and the
	// terminal state that go with it are read together, so a rotation landing
	// mid-call cannot make this request present one key over another's
	// connection.
	backend, hc, closed, authFail := c.current()
	if authFail != nil {
		// Terminal. Re-presenting a rejected key is what gets it revoked, so the
		// call never reaches the wire again.
		return nil, authFail
	}
	if closed {
		return nil, fmt.Errorf("client is closed")
	}

	rt, err := route(method, params)
	if err != nil {
		return nil, err
	}
	if err := c.sem.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer c.sem.Release(1)

	raw, err := c.do(ctx, backend, hc, method, rt)
	if err != nil {
		return nil, err
	}
	if len(rt.filters) > 0 {
		return applyFilters(raw, rt.filters), nil
	}
	return raw, nil
}

// do performs one request with the credential and transport its caller
// snapshotted. Both are passed in rather than read from the struct so that a
// credential rotation is never observed halfway through a single request.
func (c *Client) do(ctx context.Context, backend config.Backend, hc *http.Client,
	method string, rt request) (json.RawMessage, error) {
	var body io.Reader
	if rt.body != nil {
		encoded, err := json.Marshal(rt.body)
		if err != nil {
			return nil, fmt.Errorf("%s: encode request: %w", method, err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, rt.verb, c.base+"/"+rt.path, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+backend.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", truenas.ErrConnClosed, method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", truenas.ErrConnClosed, method, readErr)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		authErr := fmt.Errorf("%w: %s answered %d (backend %s, user %s)",
			truenas.ErrAuthFailed, method, resp.StatusCode, backend.Name, backend.Username)
		c.mu.Lock()
		// Only if the rejected credential is still the current one. A rotation
		// that landed while this request was in flight has already cleared
		// authFail deliberately, and re-setting it here would make the new key
		// terminal for a rejection the OLD key earned.
		if c.backend.Credentials().Equal(backend.Credentials()) {
			c.authFail = authErr
		}
		c.mu.Unlock()
		return nil, authErr
	case resp.StatusCode == http.StatusNotFound:
		// Absence, expressed the way truenas.IsNotFound reads it. CORE answers a
		// missing object with 404 where SCALE hides ENOENT inside a reason
		// string; both must mean the same thing to a caller.
		return nil, &truenas.CallError{
			Method: method, Code: resp.StatusCode, ErrName: "ENOENT",
			Reason: "[ENOENT] " + message(raw),
		}
	case resp.StatusCode >= 400:
		reason := message(raw)
		if strings.Contains(reason, "does not exist") && !strings.HasPrefix(reason, "[") {
			reason = "[ENOENT] " + reason
		}
		return nil, &truenas.CallError{
			Method: method, Code: resp.StatusCode, ErrName: "EINVAL", Reason: reason,
		}
	}
	return raw, nil
}

// message pulls the human-readable part out of a CORE error body, which is
// either {"message": ...} or a bare string.
func message(raw []byte) string {
	var envelope struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Message != "" {
		return envelope.Message
	}
	return strings.TrimSpace(string(raw))
}
