package backend

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/core"
)

// Registry holds one connection per configured appliance.
//
// Connections are dialled lazily and independently: one unreachable appliance
// must not prevent startup, nor stall calls against a healthy one.
type Registry struct {
	cfg *config.Config

	mu      sync.Mutex
	clients map[string]truenas.API
	dialing map[string]*sync.Mutex
}

// NewRegistry prepares the registry. It does not connect; see Client.
func NewRegistry(_ context.Context, cfg *config.Config) (*Registry, error) {
	if cfg == nil || len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	r := &Registry{cfg: cfg, clients: map[string]truenas.API{}, dialing: map[string]*sync.Mutex{}}
	for name := range cfg.Backends {
		r.dialing[name] = &sync.Mutex{}
		obs.SetBackendUp(name, false)
	}
	return r, nil
}

// dial opens the client the backend's flavour calls for.
//
// This is the ONLY place in the driver that knows a CORE appliance exists.
// Everything above it holds a truenas.API and cannot tell the two apart, which
// is what keeps CORE from leaking flavour checks through the provisioning code.
func dial(ctx context.Context, b config.Backend) (truenas.API, error) {
	if b.NormalisedFlavour() == config.FlavourCORE {
		return core.Dial(ctx, b)
	}
	return truenas.Dial(ctx, b)
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
func (r *Registry) Client(ctx context.Context, name string) (truenas.API, error) {
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

	c, err := dial(ctx, cfgB)
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
	return f(c, r.Options(cfgB)), nil
}

// Options resolves one appliance's configuration into what a backend needs.
//
// It is exported because the reaper is built from the same policy the backends
// dispose through, and the two must not be able to disagree about where the
// graveyard is or how long the grace period lasts.
func (r *Registry) Options(b config.Backend) Options {
	return Options{
		Pool:      b.Pool,
		Parent:    b.ParentDataset,
		Retention: retention.PolicyFor(b),
	}
}

// RetentionTargets is the reaper's view of the configured appliances: every
// backend whose delete protection is on, with a connected client.
//
// An appliance that cannot be dialled is omitted rather than reported: the
// reaper's only action is destructive, and "I could not reach the box" must
// mean "destroy nothing", never "assume the graveyard is empty".
func (r *Registry) RetentionTargets(ctx context.Context) []retention.Target {
	names := r.Names()
	sort.Strings(names)
	out := make([]retention.Target, 0, len(names))
	for _, name := range names {
		cfgB, err := r.Backend(name)
		if err != nil {
			continue
		}
		policy := retention.PolicyFor(cfgB)
		if !policy.On() {
			continue
		}
		c, err := r.Client(ctx, name)
		if err != nil {
			obs.Logger(ctx).Warn("reaper skipping unreachable backend",
				"backend", name, "error", obs.Redact(err.Error()))
			continue
		}
		out = append(out, retention.Target{Name: name, Client: c, Policy: policy})
	}
	return out
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
