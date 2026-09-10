package config

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ErrRestartRequired means the new configuration differs in a field that cannot
// be swapped underneath a running driver.
//
// The reloadable set is deliberately tiny, and it is the credential:
//
//	reloadable   username, apiKey, caCert, insecureSkipVerify
//	restart      endpoint, flavour, pool, parentDataset, the set of backend
//	             names, nodeID, metricsAddr, metricsInterval,
//	             reservedBytes, reservedPercent, namespaceQuotas,
//	             deleteProtection, rate limit and breaker settings
//
// Everything in the second group is identity or wiring that other components
// captured at startup: the backend registry copied each Backend by value, the
// metrics collector fixed its ticker, and every PersistentVolume this driver
// created records which appliance and dataset it lives on. Swapping an endpoint
// in place would silently repoint live volumes at a different appliance and
// leave the old one holding data nobody is tracking any more. Refusing loudly
// and continuing on the old configuration is the only safe answer.
var ErrRestartRequired = errors.New("configuration change requires a driver restart")

// FieldChange is one difference between the running configuration and a
// proposed replacement.
type FieldChange struct {
	// Backend is the appliance the field belongs to, or "" for a driver-wide
	// field.
	Backend string
	Field   string
}

// String renders the change for a log line. Values are deliberately absent: one
// of these fields is next to an API key, and a diff that prints values is one
// refactor away from printing that key.
func (c FieldChange) String() string {
	if c.Backend == "" {
		return c.Field
	}
	return c.Backend + "." + c.Field
}

// CheckReloadable reports whether next may replace cur without restarting.
// It returns an error wrapping ErrRestartRequired naming every offending field,
// so an operator sees the whole reason in one log line rather than one field
// per edit-and-retry cycle.
func CheckReloadable(cur, next *Config) error {
	var changed []FieldChange
	add := func(backend, field string, differs bool) {
		if differs {
			changed = append(changed, FieldChange{Backend: backend, Field: field})
		}
	}

	add("", "nodeID", cur.NodeID != next.NodeID)
	add("", "metricsAddr", cur.MetricsAddr != next.MetricsAddr)
	add("", "metricsInterval", cur.MetricsPollInterval() != next.MetricsPollInterval())

	for name, cb := range cur.Backends {
		nb, ok := next.Backends[name]
		if !ok {
			add(name, "(removed)", true)
			continue
		}
		add(name, "name", cb.Name != nb.Name)
		add(name, "endpoint", cb.Endpoint != nb.Endpoint)
		add(name, "flavour", cb.NormalisedFlavour() != nb.NormalisedFlavour())
		add(name, "pool", cb.Pool != nb.Pool)
		add(name, "parentDataset", cb.ParentDataset != nb.ParentDataset)
		add(name, "reservedBytes", cb.ReservedBytes != nb.ReservedBytes)
		add(name, "reservedPercent", cb.ReservedPercent != nb.ReservedPercent)
		add(name, "rateLimit", cb.RateLimit != nb.RateLimit)
		add(name, "breakerThreshold", cb.BreakerThreshold != nb.BreakerThreshold)
		add(name, "breakerResetTimeout", cb.BreakerReset() != nb.BreakerReset())
		// Layout, not credential: the controller captured this configuration at
		// startup, and switching the layout under a running driver would put
		// two volumes of the same namespace in two different places depending
		// on which side of the reload they were created.
		add(name, "namespaceQuotas", !cb.NamespaceQuotas.Equal(nb.NamespaceQuotas))
		// Startup wiring, not credential: the reaper is started once, from the
		// configuration the controller booted with, and it is the one thing in
		// this driver that destroys data. Adopting a shortened grace period
		// live would make datasets reapable that an operator's earlier
		// configuration had promised to keep.
		add(name, "deleteProtection", !cb.DeleteProtection.Equal(nb.DeleteProtection))
	}
	for name := range next.Backends {
		if _, ok := cur.Backends[name]; !ok {
			add(name, "(added)", true)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	names := make([]string, 0, len(changed))
	for _, c := range changed {
		names = append(names, c.String())
	}
	sort.Strings(names)
	return fmt.Errorf("%w: %s", ErrRestartRequired, strings.Join(names, ", "))
}

// RotatedCredentials returns the backends whose credential differs between cur
// and next, as a name-keyed map. It is only meaningful once CheckReloadable has
// passed, so both configurations describe the same set of appliances.
func RotatedCredentials(cur, next *Config) map[string]Credential {
	out := map[string]Credential{}
	for name, nb := range next.Backends {
		cb, ok := cur.Backends[name]
		if !ok {
			continue
		}
		if !cb.Credentials().Equal(nb.Credentials()) {
			out[name] = nb.Credentials()
		}
	}
	return out
}

// Reloader re-reads a mounted configuration file and adopts it only when the
// new content is valid and changes nothing that needs a restart.
//
// The running configuration is never mutated in place. Callers take the new
// *Config from Reload (or from Run's callback) and apply the credential to
// their own clients; the configuration the rest of the driver captured at
// startup keeps describing the appliance it always described.
type Reloader struct {
	path string

	mu  sync.RWMutex
	cur *Config
}

// NewReloader watches path on behalf of the already-loaded configuration cur.
func NewReloader(path string, cur *Config) *Reloader {
	return &Reloader{path: path, cur: cur}
}

// Path is the configuration file being watched.
func (r *Reloader) Path() string { return r.path }

// Current is the configuration in force.
func (r *Reloader) Current() *Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cur
}

// Reload reads the file and, if it is valid and reloadable, adopts it.
//
// It returns the rotated credentials and the adopted configuration. On ANY
// error — unreadable file, unparsable YAML, a plaintext endpoint, a field that
// needs a restart — the running configuration is left exactly as it was and the
// error is returned for the caller to log. A bad edit to a mounted Secret must
// never take the driver down, and it must never half-apply.
//
// The validation is the same Load the driver started with, so the transport
// check that keeps an API key from being presented over plaintext runs again on
// every reload rather than only at boot.
func (r *Reloader) Reload() (map[string]Credential, *Config, error) {
	next, err := Load(r.path)
	if err != nil {
		return nil, nil, fmt.Errorf("reload %s: %w", r.path, err)
	}

	r.mu.RLock()
	cur := r.cur
	r.mu.RUnlock()

	// NodeID arrives from a flag on the node plugin and overrides the file, so
	// a file that never carried one must not read as a change.
	if next.NodeID == "" {
		next.NodeID = cur.NodeID
	}
	if err := CheckReloadable(cur, next); err != nil {
		return nil, nil, fmt.Errorf("reload %s: %w", r.path, err)
	}

	rotated := RotatedCredentials(cur, next)

	r.mu.Lock()
	r.cur = next
	r.mu.Unlock()
	return rotated, next, nil
}

// Run watches the configuration file and calls onRotate for every accepted
// reload that changed at least one credential. It blocks until ctx ends.
//
// A rejected reload is reported through onError and nothing else happens: the
// driver keeps running on the configuration it has. onError may be nil.
func (r *Reloader) Run(ctx context.Context, onRotate func(map[string]Credential, *Config), onError func(error)) error {
	return WatchFile(ctx, r.path, func() {
		rotated, cfg, err := r.Reload()
		if err != nil {
			if onError != nil {
				onError(err)
			}
			return
		}
		if len(rotated) == 0 {
			return
		}
		if onRotate != nil {
			onRotate(rotated, cfg)
		}
	})
}
