// Package truenas is a client for the TrueNAS SCALE middleware, speaking
// JSON-RPC 2.0 over a websocket.
package truenas

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pwatteel/truenas-csi/internal/config"
	"golang.org/x/sync/semaphore"
)

// MaxInFlight caps concurrent calls per connection.
//
// The appliance accepts exactly 20 in flight and rejects the rest with -32000
// (measured: 16 concurrent -> 0 errors; 32 -> 12 errors). 16 leaves headroom.
const MaxInFlight = 16

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
type Client struct {
	backend config.Backend
	tlsConf *tls.Config
	sem     *semaphore.Weighted
	notif   chan Notification

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
	if err := configValidateEndpoint(b); err != nil {
		return nil, err
	}
	tc, err := tlsConfig(b)
	if err != nil {
		return nil, err
	}
	c := &Client{
		backend: b,
		tlsConf: tc,
		sem:     semaphore.NewWeighted(MaxInFlight),
		notif:   make(chan Notification, 64),
		waiters: map[int64]chan *response{},
	}
	if err := c.ensureConn(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// configValidateEndpoint refuses a plaintext endpoint before opening a socket.
// A single plaintext connection makes TrueNAS revoke the API key outright.
func configValidateEndpoint(b config.Backend) error {
	cfg := &config.Config{Backends: map[string]config.Backend{b.Name: b}, NodeID: "x"}
	return cfg.Validate()
}

func tlsConfig(b config.Backend) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if b.InsecureSkipVerify {
		tc.InsecureSkipVerify = true
		return tc, nil
	}
	if len(b.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b.CACert) {
			return nil, errors.New("caCert is not a valid PEM certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
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
		newConn.Close()
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
	conn.Close()
}

// Host is the appliance's hostname or address, taken from the endpoint URL.
//
// It is the default data address for NFS exports and iSCSI portals, so a
// backend does not have to depend on a StorageClass parameter having been seen
// earlier in this process's lifetime — a cache that a controller restart would
// empty, stranding volumes that already exist.
func (c *Client) Host() string {
	u, err := url.Parse(c.backend.Endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
