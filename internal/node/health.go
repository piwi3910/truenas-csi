package node

// Connectivity health monitoring.
//
// When a node loses its data path to the appliance the kernel hides the loss:
// an NFS mount with `hard` hangs forever, an NFS export that went away answers
// ESTALE, and an iSCSI session whose portal is unreachable spins in the SCSI
// error handler. From Kubernetes' point of view the node is healthy and the pod
// is running, so nothing happens at all — no eviction, no failover, just I/O
// that never completes.
//
// The monitor here polls each staged volume's data path and each backend's data
// address, and turns what it finds into three signals an operator and the CO can
// act on: a Prometheus gauge per volume, a gauge per backend, and an abnormal
// volume condition surfaced through the node's volume-health RPC.
//
// The single hardest constraint is that the check must never hang. The failure
// being detected is precisely the one that makes statfs(2) block indefinitely,
// so every probe runs in its own goroutine under a context deadline and the
// monitor selects on the result: a hung syscall leaks one goroutine (it cannot
// be cancelled — only the mount going away or the kernel giving up will release
// it) but never delays the next pass, and never blocks the RPC path that reads
// the result.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// Health monitor defaults. Ten seconds between passes is frequent enough to beat
// kubelet's own five-minute volume-health poll by a wide margin; three seconds
// is long enough that a loaded NAS is not called dead, and short enough that a
// pass over a hundred volumes cannot take longer than the interval.
const (
	DefaultHealthInterval = 10 * time.Second
	DefaultHealthTimeout  = 3 * time.Second
)

// Log messages for the two transitions. They are constants so the test that
// pins "once per transition, not once per poll" cannot drift from the code.
const (
	healthLostMessage      = "volume data path is unreachable"
	healthRecoveredMessage = "volume data path recovered"
)

// ErrHealthCheckTimeout is the cause recorded when a probe exceeded its
// deadline — the signature of a hung mount rather than of a refused one.
var ErrHealthCheckTimeout = errors.New("health check timed out")

// HealthTarget is one thing the monitor watches: a staged volume, and the
// backend data address behind it.
type HealthTarget struct {
	// VolumeID is the CSI volume handle. It is logged in full and hashed for
	// metrics; it never becomes a label value.
	VolumeID string
	// Protocol is ProtocolNFS or ProtocolISCSI, and is a metric label.
	Protocol string
	// Path is the staging path, checked with a bounded statfs. Empty for a raw
	// block volume, which has no filesystem to stat.
	Path string
	// Backend names the appliance, for the backend reachability gauge.
	Backend string
	// DataAddr is the appliance's data address (host:port) — the NFS server or
	// the iSCSI portal — probed with a bounded TCP dial.
	DataAddr string
}

// HealthState is what the monitor last observed about one volume.
type HealthState struct {
	VolumeID   string
	Protocol   string
	Healthy    bool
	Message    string
	Checked    bool
	LastChange time.Time
}

// HealthMonitor polls the data path of every staged volume. One instance lives
// for the lifetime of the node plugin.
type HealthMonitor struct {
	// Interval between passes; DefaultHealthInterval when zero.
	Interval time.Duration
	// Timeout bounds one probe; DefaultHealthTimeout when zero.
	Timeout time.Duration

	// Statfs probes a mounted filesystem. It is a field so a test can supply a
	// call that never returns — the failure mode this whole file exists for.
	Statfs func(path string) error
	// Dial probes an appliance data address.
	Dial func(ctx context.Context, addr string) error
	// Now is the clock, overridable in tests.
	Now func() time.Time

	mu      sync.Mutex
	targets map[string]HealthTarget
	states  map[string]*HealthState
}

// NewHealthMonitor builds a monitor with the production probes.
func NewHealthMonitor() *HealthMonitor {
	return &HealthMonitor{
		Interval: DefaultHealthInterval,
		Timeout:  DefaultHealthTimeout,
		Statfs:   statfsProbe,
		Dial:     dialProbe,
		Now:      time.Now,
		targets:  make(map[string]HealthTarget),
		states:   make(map[string]*HealthState),
	}
}

// statfsProbe is the real filesystem check. On a healthy mount it returns in
// microseconds; on a hung one it never returns at all, which is why every call
// site runs it under a deadline.
func statfsProbe(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return err
	}
	if st.Blocks == 0 {
		return errors.New("filesystem reports no blocks")
	}
	return nil
}

// dialProbe is the real appliance check: a TCP connect, closed immediately. It
// says nothing about NFS or iSCSI health beyond reachability, which is exactly
// what it is used for — telling "the node lost the network" apart from "this one
// export is stale".
func dialProbe(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Track adds or updates a volume to watch. Called from Stage.
func (m *HealthMonitor) Track(t HealthTarget) {
	if t.VolumeID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[t.VolumeID] = t
	if _, ok := m.states[t.VolumeID]; !ok {
		m.states[t.VolumeID] = &HealthState{
			VolumeID: t.VolumeID, Protocol: t.Protocol, Healthy: true,
		}
	}
}

// Forget stops watching a volume and retires its metric series. Called from
// Unstage: an unstaged volume that kept reporting would be a permanent false
// alarm and a permanent series.
func (m *HealthMonitor) Forget(volumeID string) {
	m.mu.Lock()
	t, ok := m.targets[volumeID]
	delete(m.targets, volumeID)
	delete(m.states, volumeID)
	m.mu.Unlock()
	if ok {
		obs.ForgetVolumeHealth(volumeID, t.Protocol)
	}
}

// Condition reports whether a volume is currently abnormal, and why. An
// untracked or never-checked volume is not abnormal: absence of evidence is not
// a fault, and reporting one would make every volume look sick at startup.
func (m *HealthMonitor) Condition(volumeID string) (abnormal bool, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[volumeID]
	if !ok || !s.Checked || s.Healthy {
		return false, ""
	}
	return true, s.Message
}

// States returns a snapshot of every watched volume, ordered by volume id.
func (m *HealthMonitor) States() []HealthState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HealthState, 0, len(m.states))
	for _, s := range m.states {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VolumeID < out[j].VolumeID })
	return out
}

// Run polls until ctx is cancelled. The first pass runs immediately, so a node
// that starts with an already-dead NAS says so within one probe timeout rather
// than one interval.
func (m *HealthMonitor) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.CheckOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckOnce(ctx)
		}
	}
}

// CheckOnce runs one pass over every tracked volume and backend. It returns
// within roughly one Timeout per target however badly the data path is behaving.
func (m *HealthMonitor) CheckOnce(ctx context.Context) {
	m.mu.Lock()
	targets := make([]HealthTarget, 0, len(m.targets))
	for _, t := range m.targets {
		targets = append(targets, t)
	}
	m.mu.Unlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].VolumeID < targets[j].VolumeID })

	// One dial per backend per pass, not one per volume: fifty volumes on one
	// appliance are one network, and fifty dials would be fifty chances to be
	// rate-limited by it.
	backends := map[string]string{}
	for _, t := range targets {
		if t.Backend != "" && t.DataAddr != "" {
			backends[t.Backend] = t.DataAddr
		}
	}
	reachable := make(map[string]bool, len(backends))
	for name, addr := range backends {
		err := m.probeBackend(ctx, addr)
		reachable[name] = err == nil
		obs.SetBackendReachable(name, err == nil)
	}

	for _, t := range targets {
		err := m.probeVolume(ctx, t)
		if err == nil && t.Backend != "" {
			if ok, known := reachable[t.Backend]; known && !ok {
				err = fmt.Errorf("backend %s is unreachable from this node", t.Backend)
			}
		}
		m.record(ctx, t, err)
	}
}

// probeVolume runs the bounded filesystem check for one target.
func (m *HealthMonitor) probeVolume(ctx context.Context, t HealthTarget) error {
	if t.Path == "" {
		// A raw block volume has no filesystem to stat; the backend dial is the
		// only signal available, and it is applied by the caller.
		return nil
	}
	statfs := m.Statfs
	if statfs == nil {
		statfs = statfsProbe
	}
	return m.bounded(ctx, func() error { return statfs(t.Path) })
}

// probeBackend runs the bounded appliance dial.
func (m *HealthMonitor) probeBackend(ctx context.Context, addr string) error {
	dial := m.Dial
	if dial == nil {
		dial = dialProbe
	}
	// The dial honours the deadline itself, but it is wrapped anyway: a resolver
	// that ignores its context would otherwise be able to hang the pass.
	return m.bounded(ctx, func() error {
		dctx, cancel := context.WithTimeout(ctx, m.timeout())
		defer cancel()
		return dial(dctx, addr)
	})
}

// bounded runs fn under a deadline and returns whichever comes first: fn's
// result, or the deadline.
//
// The result channel is buffered so that the goroutine can always finish and be
// collected even after the deadline has passed and nobody is listening; an
// unbuffered channel would leak the goroutine permanently on every timeout.
// This is the load-bearing detail of the whole file: without the select, the
// monitor would block on exactly the hang it exists to report.
func (m *HealthMonitor) bounded(ctx context.Context, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("%w after %s", ErrHealthCheckTimeout, m.timeout())
	}
}

func (m *HealthMonitor) timeout() time.Duration {
	if m.Timeout <= 0 {
		return DefaultHealthTimeout
	}
	return m.Timeout
}

// record stores the result of one probe, updates the metric and logs only when
// the state changed. Logging every poll would emit one line per volume every
// interval for as long as an outage lasts — thousands of identical lines that
// bury the transition that actually mattered.
func (m *HealthMonitor) record(ctx context.Context, t HealthTarget, err error) {
	healthy := err == nil
	obs.SetVolumeHealth(t.VolumeID, t.Protocol, healthy)

	m.mu.Lock()
	s, ok := m.states[t.VolumeID]
	if !ok {
		// Forgotten mid-pass: the volume was unstaged while this probe ran.
		m.mu.Unlock()
		return
	}
	changed := !s.Checked || s.Healthy != healthy
	s.Protocol = t.Protocol
	s.Healthy = healthy
	s.Checked = true
	if err != nil {
		s.Message = err.Error()
	} else {
		s.Message = ""
	}
	if changed {
		s.LastChange = m.now()
	}
	message := s.Message
	m.mu.Unlock()

	if !changed {
		return
	}
	lg := obs.Logger(obs.WithVolume(ctx, t.VolumeID))
	if healthy {
		lg.Info(healthRecoveredMessage, "protocol", t.Protocol, "path", t.Path)
		return
	}
	lg.Error(healthLostMessage,
		"protocol", t.Protocol, "path", t.Path, "backend", t.Backend, "error", message)
}

func (m *HealthMonitor) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

// dataAddrOf derives the appliance data address from a publish context: the NFS
// server on port 2049, or the iSCSI portal with its default port filled in.
func dataAddrOf(pc map[string]string) string {
	switch protocolOf(pc) {
	case ProtocolNFS:
		if s := pc[KeyServer]; s != "" {
			return net.JoinHostPort(s, "2049")
		}
	case ProtocolISCSI:
		if p := pc[KeyPortal]; p != "" {
			if _, _, err := net.SplitHostPort(p); err == nil {
				return p
			}
			return net.JoinHostPort(p, "3260")
		}
	}
	return ""
}

// Target returns what the monitor knows about one volume, so another service in
// the process — the podmon extension — can resolve a volume id to a path
// without touching the mount table or the driver's own RPC surface.
func (m *HealthMonitor) Target(volumeID string) (HealthTarget, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.targets[volumeID]
	return t, ok
}
