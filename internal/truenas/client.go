// Package truenas is a client for the TrueNAS SCALE middleware, speaking
// JSON-RPC 2.0 over a websocket.
package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/piwi3910/truenas-csi/internal/config"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// MaxInFlight caps concurrent calls per connection.
//
// The appliance accepts exactly 20 in flight and rejects the rest with -32000
// (measured: 16 concurrent -> 0 errors; 32 -> 12 errors). 16 leaves headroom.
const MaxInFlight = 16

// Rate limit and circuit breaker defaults.
//
// DefaultRateLimit is a ceiling on a runaway caller, not a throughput target.
// The appliance's documented constraint is CONCURRENCY (20 in flight, measured;
// see MaxInFlight), not rate, and MaxInFlight already enforces that. What a
// rate limit adds is protection against the failure mode a semaphore cannot
// see: a loop whose calls fail instantly — a dropped socket, a breaker that is
// not yet open — issues thousands of dials a second while never holding more
// than a handful of slots. 100/s sits far above anything the provisioning path
// or the metrics poller produces in steady state (a CreateVolume is a dozen
// calls; a metrics poll is three per appliance per minute), so a healthy driver
// never waits on it, while a spinning caller is pinned to a rate the middleware
// can shrug off.
//
// DefaultRateBurst is MaxInFlight so a legitimate parallel fan-out — the
// concurrency the semaphore explicitly permits — is never delayed by the
// limiter it was already admitted past.
//
// DefaultBreakerThreshold is deliberately well above Dell's 3. A single dropped
// websocket fails EVERY call in flight at once, up to MaxInFlight of them, and
// that is a blip the existing single retry in Call already recovers from. A
// threshold of 3 would open the breaker on a routine idle-socket close and turn
// a 50ms reconnect into a 10-second outage. 10 is more than half a full
// in-flight window: reachable in a fraction of a second against a genuinely
// wedged appliance, and not reachable at all by one reconnect.
//
// DefaultBreakerReset is short on purpose. The breaker exists to stop the
// driver hammering a wedged appliance at its 20-call ceiling, not to punish it:
// 10 seconds is below the CSI sidecars' --retry-interval-max of 30s, so a
// recovered appliance is picked up within a single sidecar retry and no CSI
// call fails that would otherwise have succeeded. It is NOT backed off
// exponentially — a growing timeout is how a 30-second appliance blip becomes
// minutes of failing fast long after the appliance came back.
const (
	DefaultRateLimit        = 100.0
	DefaultRateBurst        = MaxInFlight
	DefaultBreakerThreshold = 10
	DefaultBreakerReset     = 10 * time.Second
)

const (
	dialTimeout    = 20 * time.Second
	callTimeout    = 2 * time.Minute
	backoffInitial = time.Second
	backoffMax     = 30 * time.Second
)

// Notification is an id-less server message, such as collection_update.
type Notification struct {
	Method string
	Params json.RawMessage
}

type response struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			ErrName string `json:"errname"`
			Reason  string `json:"reason"`
		} `json:"data"`
	} `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Client is a connection to one TrueNAS appliance. It is safe for concurrent use.
//
// It is the SCALE implementation of API: the typed calls come from the embedded
// Ops, which routes them straight back through this type's Call/CallJSON.
type Client struct {
	Ops

	// host is the appliance's hostname, resolved once at Dial. It is cached
	// rather than parsed from backend.Endpoint on demand because backend is
	// guarded by connMu (see below) and Host has callers that hold neither
	// lock; a cached string is also the honest model, since the endpoint is one
	// of the fields a reload refuses to change.
	host string

	sem   *semaphore.Weighted
	notif chan Notification

	// limiter and breaker are both self-contained and lock-free from this
	// type's point of view: neither is ever consulted while c.mu or c.connMu is
	// held, so neither can participate in a lock cycle.
	limiter *rate.Limiter
	breaker *breaker

	// backend and tlsConf are guarded by connMu, NOT by mu. They are written
	// only by ReloadCredentials and read only by ensureConn and login, both of
	// which run under connMu — so a rotated API key can never be half-applied
	// to a login that is already in flight.
	backend config.Backend
	tlsConf *tls.Config

	// connMu serialises connection establishment. It is NOT held while waiting
	// for the login response — readLoop needs mu to deliver that response, so
	// holding mu across the wait would deadlock.
	connMu   sync.Mutex
	mu       sync.Mutex
	conn     *websocket.Conn
	writeMu  sync.Mutex
	nextID   int64
	waiters  map[int64]chan *response
	closed   bool
	authFail error // terminal: never retry once set
}

// Dial connects to the appliance and authenticates once.
func Dial(ctx context.Context, b config.Backend) (*Client, error) {
	// Resolve the credential through the process-wide store first. A Backend
	// travels through the driver by value, so the key in b may be the one that
	// was on disk when the driver started rather than the one on disk now — and
	// presenting a superseded key is not a harmless retry on TrueNAS, it is how
	// a key gets revoked.
	b = b.Current()
	if err := ValidateBackend(b); err != nil {
		return nil, err
	}
	tc, err := TLSConfig(b)
	if err != nil {
		return nil, err
	}
	host := ""
	if u, uErr := url.Parse(b.Endpoint); uErr == nil {
		host = u.Hostname()
	}
	c := &Client{
		backend: b,
		tlsConf: tc,
		host:    host,
		sem:     semaphore.NewWeighted(MaxInFlight),
		limiter: limiterFor(b),
		breaker: breakerFor(b),
		notif:   make(chan Notification, 64),
		waiters: map[int64]chan *response{},
	}
	c.Transport = c
	if err := c.ensureConn(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// Notifications yields id-less server messages such as job progress updates.
func (c *Client) Notifications() <-chan Notification { return c.notif }

// Close shuts the client down.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// ensureConn returns a live connection, dialling and authenticating if needed.
func (c *Client) ensureConn(ctx context.Context) error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	c.mu.Lock()
	authFail, closed, conn := c.authFail, c.closed, c.conn
	c.mu.Unlock()

	if authFail != nil {
		return authFail // terminal, never retry
	}
	if closed {
		return errors.New("client is closed")
	}
	if conn != nil {
		return nil
	}

	d := websocket.Dialer{TLSClientConfig: c.tlsConf, HandshakeTimeout: dialTimeout}
	newConn, _, err := d.DialContext(ctx, c.backend.Endpoint, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.backend.Name, err)
	}

	c.mu.Lock()
	c.conn = newConn
	c.mu.Unlock()
	go c.readLoop(newConn)

	if err := c.login(ctx); err != nil {
		_ = newConn.Close()
		c.mu.Lock()
		if c.conn == newConn {
			c.conn = nil
		}
		if errors.Is(err, ErrAuthFailed) {
			c.authFail = err
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// readLoop is the single reader. Responses are routed to their waiter by id;
// id-less messages are notifications. A per-call read would make concurrent
// calls impossible, which is why this exists.
func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			c.dropConn(conn)
			return
		}
		var r response
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		if r.ID == nil {
			if r.Method != "" {
				select {
				case c.notif <- Notification{Method: r.Method, Params: r.Params}:
				default: // never block the reader on a slow consumer
				}
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.waiters[*r.ID]
		delete(c.waiters, *r.ID)
		c.mu.Unlock()
		if ok {
			rr := r
			ch <- &rr
		}
	}
}

// dropConn tears down a dead connection and fails everything waiting on it.
func (c *Client) dropConn(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
	for id, ch := range c.waiters {
		delete(c.waiters, id)
		close(ch)
	}
	_ = conn.Close()
}

// Host is the appliance's hostname or address, taken from the endpoint URL.
//
// It is the default data address for NFS exports and iSCSI portals, so a
// backend does not have to depend on a StorageClass parameter having been seen
// earlier in this process's lifetime — a cache that a controller restart would
// empty, stranding volumes that already exist.
func (c *Client) Host() string { return c.host }

// ReloadCredentials swaps this connection's credentials for the ones in b,
// without restarting the driver and without disturbing anything else.
//
// What may change is exactly the credential: username, API key, CA certificate
// and the certificate-verification decision. Endpoint, flavour, pool and parent
// dataset are IDENTITY — every PersistentVolume this driver created records
// which appliance and dataset it lives on — so changing them here would
// silently repoint live volumes at a different array. They are refused, and the
// caller is expected to say so out loud; config.CheckReloadable makes the same
// distinction for the file as a whole.
//
// The full backend validation runs again, so the transport check that stops an
// API key being presented over plaintext applies to a rotated key exactly as it
// applies to the one the driver booted with.
//
// The live connection is closed rather than re-authenticated in place: the
// middleware binds authentication to the socket, so the only way to adopt a new
// key is a new socket. The next Call reconnects, which it already knows how to
// do — see Call's single retry on ErrConnClosed.
func (c *Client) ReloadCredentials(b config.Backend) error {
	if err := ValidateBackend(b); err != nil {
		return err
	}
	tc, err := TLSConfig(b)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	cur := c.backend
	for _, f := range []struct{ name, old, new string }{
		{"endpoint", cur.Endpoint, b.Endpoint},
		{"flavour", cur.NormalisedFlavour(), b.NormalisedFlavour()},
		{"pool", cur.Pool, b.Pool},
		{"parentDataset", cur.ParentDataset, b.ParentDataset},
	} {
		if f.old != f.new {
			c.connMu.Unlock()
			return fmt.Errorf("backend %q: %s cannot be changed without restarting the driver (%q -> %q)",
				cur.Name, f.name, f.old, f.new)
		}
	}
	if cur.Credentials().Equal(b.Credentials()) {
		c.connMu.Unlock()
		return nil // nothing rotated: never drop a healthy connection for a no-op
	}

	c.backend = b
	c.tlsConf = tc

	c.mu.Lock()
	// A new credential deserves a fresh attempt. authFail is terminal for the
	// key that earned it, not for the appliance: without this, one revoked key
	// would poison the client for the process's lifetime and rotating the key
	// — the entire point of this method — would fix nothing.
	c.authFail = nil
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	c.connMu.Unlock()

	if conn != nil {
		_ = conn.Close() // readLoop sees the close and fails the waiters
	}
	// A breaker opened by the old credential's failures must not outlive it.
	c.breaker.reset()
	return nil
}
