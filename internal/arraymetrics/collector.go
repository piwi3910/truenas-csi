// Package arraymetrics exports the appliance's own capacity and session figures
// alongside the driver's metrics, so an operator can see the backend a volume
// lives on without being given a TrueNAS login.
//
// Everything here is collected on a background poll loop and served from a
// snapshot. A Prometheus scrape must never reach the appliance: a slow or dead
// middleware would otherwise stall — or time out — every scrape of the driver,
// taking the driver's own metrics down with it.
package arraymetrics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/pooladmin"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/semaphore"
)

// DefaultInterval is how often each appliance is polled when the configuration
// does not say. Capacity moves slowly; a shorter interval buys nothing and
// spends the appliance's small concurrency budget.
const DefaultInterval = 60 * time.Second

// maxParallelBackends bounds the fan-out across appliances.
//
// The middleware accepts 20 in-flight calls per connection and the client caps
// itself at 16 (truenas.MaxInFlight). Collection issues a handful of calls per
// appliance and must leave that budget to the provisioning path, so appliances
// are polled a few at a time rather than all at once.
const maxParallelBackends = 4

// probeTimeout bounds the startup reachability check, so a dead appliance
// delays controller startup by seconds rather than by the dial timeout of every
// backend in series.
const probeTimeout = 30 * time.Second

// Registry is the part of backend.Registry this collector needs. Connections
// are reused: the collector never dials an appliance of its own.
type Registry interface {
	Names() []string
	Backend(name string) (config.Backend, error)
	Client(ctx context.Context, name string) (truenas.API, error)
}

// Label sets are fixed and small on purpose: backend and pool are configuration,
// and dataset is bounded by the number of volumes this driver itself created.
// A volume id, node name, initiator IQN or share path would be unbounded, and a
// credential must never reach a label at all.
var (
	// RAW bytes, which is what `zpool list` and the appliance's pool view show.
	// On a RAIDZ pool that INCLUDES PARITY and is therefore larger than the
	// data the pool can hold: a 12-disk RAIDZ2 measured 41.05 TiB raw free
	// against 30.33 TiB writable. Alert on these for the health of the pool
	// itself; for "can another volume be provisioned", the driver's
	// CSIStorageCapacity is the figure that answers it, and it is deliberately
	// smaller.
	poolSizeDesc = prometheus.NewDesc("truenas_pool_size_bytes",
		"Total RAW size of a ZFS pool in bytes, as the appliance reports it. "+
			"On RAIDZ this includes parity, so it exceeds what the pool can store.",
		[]string{"backend", "pool"}, nil)
	poolFreeDesc = prometheus.NewDesc("truenas_pool_free_bytes",
		"Free RAW space in a ZFS pool in bytes. On RAIDZ this includes parity, so "+
			"it exceeds what can still be written.",
		[]string{"backend", "pool"}, nil)
	poolUsedDesc = prometheus.NewDesc("truenas_pool_used_bytes",
		"Used RAW space in a ZFS pool, in bytes (size minus free).",
		[]string{"backend", "pool"}, nil)
	poolHealthyDesc = prometheus.NewDesc("truenas_pool_healthy",
		"1 when the appliance reports the pool healthy, 0 otherwise.",
		[]string{"backend", "pool"}, nil)

	datasetUsedDesc = prometheus.NewDesc("truenas_dataset_used_bytes",
		"Space used by a dataset this driver owns, in bytes.",
		[]string{"backend", "dataset"}, nil)
	datasetQuotaDesc = prometheus.NewDesc("truenas_dataset_quota_bytes",
		"Provisioned capacity of a dataset this driver owns: refquota for a "+
			"filesystem, volsize for a zvol.",
		[]string{"backend", "dataset"}, nil)

	iscsiSessionsDesc = prometheus.NewDesc("truenas_iscsi_sessions",
		"Open iSCSI sessions on the appliance, across every target.",
		[]string{"backend"}, nil)

	// The NFS counterpart of iscsiSessionsDesc. Both are appliance-wide: the
	// middleware counts clients and sessions for the whole box, never per share
	// or per dataset, so neither can be attributed to a volume. Per-volume I/O
	// is measured node-side instead -- see internal/podmon.
	nfsClientsDesc = prometheus.NewDesc("truenas_nfs_clients",
		"NFS clients currently holding a mount on the appliance, across every export.",
		[]string{"backend"}, nil)

	collectionDurationDesc = prometheus.NewDesc("truenas_collection_duration_seconds",
		"Duration of the last array metrics collection for a backend.",
		[]string{"backend"}, nil)
	collectionErrorsDesc = prometheus.NewDesc("truenas_collection_errors_total",
		"Failed array metrics collections for a backend since the driver started.",
		[]string{"backend"}, nil)
)

// datasetUsage is one owned dataset's figures.
type datasetUsage struct {
	id    string
	used  float64
	quota float64
}

// snapshot is one appliance's last successful collection. It is replaced whole
// on success and left untouched on failure — see Collector.collectBackend.
type snapshot struct {
	pool       string
	size       float64
	free       float64
	healthy    bool
	datasets   []datasetUsage
	sessions   float64
	nfsClients float64
	duration   float64
}

// Collector polls every configured appliance and serves the results from cache.
//
// It implements prometheus.Collector, but Collect only reads the cache; the
// appliance is touched from Run and Start.
type Collector struct {
	reg      Registry
	interval time.Duration

	mu   sync.RWMutex
	last map[string]*snapshot
	errs map[string]float64
}

// New builds a collector over an existing registry. A non-positive interval
// means DefaultInterval.
func New(reg Registry, interval time.Duration) *Collector {
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Collector{
		reg:      reg,
		interval: interval,
		last:     map[string]*snapshot{},
		errs:     map[string]float64{},
	}
}

// Interval is how often Run polls.
func (c *Collector) Interval() time.Duration { return c.interval }

// Start performs the first collection and reports whether the collector is
// worth running at all.
//
// It returns an error when no configured appliance could be reached, so the
// caller can log the reason and carry on. Registering a collector that can
// never produce a value only adds a permanently empty scrape, and failing the
// process outright would crash-loop the controller over an appliance outage
// that does not affect provisioning of already-attached volumes.
func (c *Collector) Start(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	c.CollectOnce(probeCtx)

	c.mu.RLock()
	n := len(c.last)
	c.mu.RUnlock()
	if n == 0 {
		return errors.New("no configured appliance answered: array metrics unavailable")
	}
	return nil
}

// Run polls on the configured interval until the context ends.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.CollectOnce(ctx)
		}
	}
}

// CollectOnce polls every appliance once, a few at a time.
//
// Each appliance is independent: one that is unreachable records an error and
// leaves its previous snapshot in place. Zeroing a gauge because the collection
// failed would turn a middleware outage into a capacity alert, which is exactly
// the wrong page to send at 3am.
func (c *Collector) CollectOnce(ctx context.Context) {
	names := c.reg.Names()
	sort.Strings(names)

	sem := semaphore.NewWeighted(maxParallelBackends)
	var wg sync.WaitGroup
	for _, name := range names {
		if err := sem.Acquire(ctx, 1); err != nil {
			// Context ended: stop starting work, but still wait for what is
			// already in flight rather than leaving goroutines writing into
			// the snapshot behind our back.
			break
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			defer sem.Release(1)
			c.collectBackend(ctx, name)
		}(name)
	}
	wg.Wait()
}

func (c *Collector) collectBackend(ctx context.Context, name string) {
	started := time.Now()
	snap, err := c.pollBackend(ctx, name)
	if err != nil {
		c.mu.Lock()
		c.errs[name]++
		c.mu.Unlock()
		// Deliberately keeping the previous snapshot: see CollectOnce.
		obs.Logger(ctx).Warn("array metrics collection failed; serving last known values",
			"backend", name, "error", obs.Redact(err.Error()))
		return
	}
	snap.duration = time.Since(started).Seconds()

	c.mu.Lock()
	c.last[name] = snap
	if _, seen := c.errs[name]; !seen {
		c.errs[name] = 0
	}
	c.mu.Unlock()

	c.collectDiagnostics(ctx, name)
}

// collectDiagnostics publishes the appliance's own health: disk health, scrub
// state and errors, active alert counts.
//
// It runs here because nothing else ever ran it. internal/obs declares those
// gauges and internal/pooladmin sets them, but the only caller of
// pooladmin.Collect was the `truenas-csi pool` CLI — a process that sets a
// gauge and immediately exits, so the series could never be scraped. They were
// declared metrics that no deployment could ever export, and the appliance's
// disk health in particular has no other route out: 25.10 exposes no SMART API,
// so an alert against a disk is the only warning anyone gets.
//
// A failure is logged, not counted as a collection error: the pool and dataset
// metrics above are the ones the cache serves, and losing the appliance's
// self-report must not make a healthy poll look failed.
//
// Note that pooladmin also sets truenas_appliance_pool_* , which overlaps the
// truenas_pool_* gauges this collector serves from its own cache. The two are
// left as they are rather than reconciled here: they come from the same
// pool.query and agree, and choosing which name survives is a decision for
// whoever owns the dashboards.
func (c *Collector) collectDiagnostics(ctx context.Context, name string) {
	cl, err := c.reg.Client(ctx, name)
	if err != nil {
		return
	}
	if _, err := pooladmin.Collect(ctx, pooladmin.Backend{Name: name, Client: cl}); err != nil {
		obs.Logger(ctx).Warn("appliance diagnostics could not be collected; disk health, "+
			"scrub state and alert counts will hold their last values",
			"backend", name, "error", obs.Redact(err.Error()))
	}
}

// pollBackend issues the appliance calls for one backend, in sequence. Three
// serial calls per appliance keep well inside the middleware's concurrency
// budget while the provisioning path is using the same connection.
func (c *Collector) pollBackend(ctx context.Context, name string) (*snapshot, error) {
	b, err := c.reg.Backend(name)
	if err != nil {
		return nil, err
	}
	cl, err := c.reg.Client(ctx, name)
	if err != nil {
		return nil, err
	}

	p, err := cl.PoolQuery(ctx, b.Pool)
	if err != nil {
		return nil, fmt.Errorf("pool %q: %w", b.Pool, err)
	}
	snap := &snapshot{
		pool: p.Name,
		// pool.query reports size and free as PLAIN INTEGERS, unlike
		// pool.dataset.query which wraps them in {"parsed": N}. Both shapes are
		// handled by the client's decoder; reading only the wrapper once made
		// every pool look empty.
		size:    float64(p.Size.Parsed),
		free:    float64(p.Free.Parsed),
		healthy: p.Healthy,
	}

	prefix := b.Pool + "/" + b.ParentDataset + "/"
	datasets, err := cl.DatasetList(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("datasets under %q: %w", prefix, err)
	}
	for i := range datasets {
		d := &datasets[i]
		// Ownership requires the marker to be LOCAL. ZFS user properties are
		// inherited, so a merely present marker can belong to somebody else's
		// dataset — their data, and unbounded cardinality besides.
		if !d.Owned(volume.OwnerProperty, volume.OwnerValue) {
			continue
		}
		quota := d.RefQuota.Parsed
		if d.Type == "VOLUME" {
			quota = d.VolSize.Parsed
		}
		snap.datasets = append(snap.datasets, datasetUsage{
			id: d.ID, used: float64(d.Used.Parsed), quota: float64(quota),
		})
	}

	sessions, err := cl.ISCSISessionCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("iscsi sessions: %w", err)
	}
	snap.sessions = float64(sessions)

	clients, err := cl.NFSClientCount(ctx)
	if err != nil {
		return nil, fmt.Errorf("nfs clients: %w", err)
	}
	snap.nfsClients = float64(clients)
	return snap, nil
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		poolSizeDesc, poolFreeDesc, poolUsedDesc, poolHealthyDesc,
		datasetUsedDesc, datasetQuotaDesc, iscsiSessionsDesc, nfsClientsDesc,
		collectionDurationDesc, collectionErrorsDesc,
	} {
		ch <- d
	}
}

// Collect implements prometheus.Collector. It reads the cache only — the
// appliance is never contacted from a scrape.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for name, errCount := range c.errs {
		ch <- prometheus.MustNewConstMetric(collectionErrorsDesc,
			prometheus.CounterValue, errCount, name)
	}

	for name, s := range c.last {
		ch <- prometheus.MustNewConstMetric(poolSizeDesc, prometheus.GaugeValue, s.size, name, s.pool)
		ch <- prometheus.MustNewConstMetric(poolFreeDesc, prometheus.GaugeValue, s.free, name, s.pool)
		used := s.size - s.free
		if used < 0 {
			used = 0
		}
		ch <- prometheus.MustNewConstMetric(poolUsedDesc, prometheus.GaugeValue, used, name, s.pool)
		healthy := 0.0
		if s.healthy {
			healthy = 1
		}
		ch <- prometheus.MustNewConstMetric(poolHealthyDesc, prometheus.GaugeValue, healthy, name, s.pool)
		ch <- prometheus.MustNewConstMetric(iscsiSessionsDesc, prometheus.GaugeValue, s.sessions, name)
		ch <- prometheus.MustNewConstMetric(nfsClientsDesc, prometheus.GaugeValue, s.nfsClients, name)
		ch <- prometheus.MustNewConstMetric(collectionDurationDesc, prometheus.GaugeValue, s.duration, name)

		for _, d := range s.datasets {
			ch <- prometheus.MustNewConstMetric(datasetUsedDesc, prometheus.GaugeValue, d.used, name, d.id)
			if d.quota > 0 {
				ch <- prometheus.MustNewConstMetric(datasetQuotaDesc, prometheus.GaugeValue, d.quota, name, d.id)
			}
		}
	}
}
