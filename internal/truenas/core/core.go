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

	backend config.Backend
	base    string
	http    *http.Client
	sem     *semaphore.Weighted

	mu       sync.Mutex
	authFail error // terminal: never retry once set
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
	c := &Client{
		backend: b,
		base:    normaliseBase(b.Endpoint),
		http: &http.Client{
			Timeout:   requestTimeout,
			Transport: &http.Transport{TLSClientConfig: tc},
		},
		sem: semaphore.NewWeighted(truenas.MaxInFlight),
	}
	c.Ops.Transport = c
	return c, nil
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
func (c *Client) Host() string {
	u, err := url.Parse(c.backend.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

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
	c.mu.Lock()
	authFail, closed := c.authFail, c.closed
	c.mu.Unlock()
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

	raw, err := c.do(ctx, method, rt)
	if err != nil {
		return nil, err
	}
	if len(rt.filters) > 0 {
		return applyFilters(raw, rt.filters), nil
	}
	return raw, nil
}

func (c *Client) do(ctx context.Context, method string, rt request) (json.RawMessage, error) {
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
	req.Header.Set("Authorization", "Bearer "+c.backend.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", truenas.ErrConnClosed, method, err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("%w: reading %s: %v", truenas.ErrConnClosed, method, readErr)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		authErr := fmt.Errorf("%w: %s answered %d (backend %s, user %s)",
			truenas.ErrAuthFailed, method, resp.StatusCode, c.backend.Name, c.backend.Username)
		c.mu.Lock()
		c.authFail = authErr
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
