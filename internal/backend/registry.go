package backend

import (
	"context"
	"fmt"
	"sync"

	"github.com/pwatteel/truenas-csi/internal/config"
	"github.com/pwatteel/truenas-csi/internal/obs"
	"github.com/pwatteel/truenas-csi/internal/truenas"
)

// Registry holds one connection per configured appliance.
//
// Connections are dialled lazily and independently: one unreachable appliance
// must not prevent startup, nor stall calls against a healthy one.
type Registry struct {
	cfg *config.Config

	mu      sync.Mutex
	clients map[string]*truenas.Client
	dialing map[string]*sync.Mutex
}

// NewRegistry prepares the registry. It does not connect; see Client.
func NewRegistry(_ context.Context, cfg *config.Config) (*Registry, error) {
	if cfg == nil || len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	r := &Registry{cfg: cfg, clients: map[string]*truenas.Client{}, dialing: map[string]*sync.Mutex{}}
	for name := range cfg.Backends {
		r.dialing[name] = &sync.Mutex{}
		obs.SetBackendUp(name, false)
	}
	return r, nil
}

// Names lists the configured appliances.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.cfg.Backends))
	for n := range r.cfg.Backends {
		out = append(out, n)
	}
	return out
}

// Backend returns the configuration for a named appliance.
func (r *Registry) Backend(name string) (config.Backend, error) {
	b, ok := r.cfg.Backends[name]
	if !ok {
		return config.Backend{}, fmt.Errorf("%w: %q (configured: %v)", ErrUnknownBackend, name, r.Names())
	}
	return b, nil
}

// Client returns a connected client for an appliance, dialling on first use.
//
// Each appliance has its own dial lock so a slow or dead one blocks only calls
// destined for it.
func (r *Registry) Client(ctx context.Context, name string) (*truenas.Client, error) {
	cfgB, err := r.Backend(name)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	if c, ok := r.clients[name]; ok {
		r.mu.Unlock()
		return c, nil
	}
	lock := r.dialing[name]
	r.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	r.mu.Lock()
	if c, ok := r.clients[name]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	c, err := truenas.Dial(ctx, cfgB)
	if err != nil {
		obs.SetBackendUp(name, false)
		return nil, fmt.Errorf("backend %q: %w", name, err)
	}
	r.mu.Lock()
	r.clients[name] = c
	r.mu.Unlock()
	obs.SetBackendUp(name, true)
	return c, nil
}

// For returns the backend serving a protocol on an appliance.
func (r *Registry) For(ctx context.Context, name, protocol string) (Backend, error) {
	cfgB, err := r.Backend(name)
	if err != nil {
		return nil, err
	}
	f, ok := factoryFor(protocol)
	if !ok {
		return nil, fmt.Errorf("%w: %q (supported: %v)", ErrUnsupportedProtocol, protocol, Protocols())
	}
	c, err := r.Client(ctx, name)
	if err != nil {
		return nil, err
	}
	return f(c, cfgB.Pool, cfgB.ParentDataset), nil
}

// Close shuts every connection down.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, c := range r.clients {
		_ = c.Close()
		delete(r.clients, name)
		obs.SetBackendUp(name, false)
	}
	return nil
}
