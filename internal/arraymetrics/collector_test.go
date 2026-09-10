package arraymetrics

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// The figures the live appliance reports for Pool0. pool.query returns these as
// PLAIN INTEGERS, unlike pool.dataset.query which wraps sizes in {"parsed": N}.
// A collector that only understands the wrapped shape reports 0 for every pool.
const (
	poolSize = 72000831750144
	poolFree = 44861949222912
)

func poolResult() []map[string]any {
	return []map[string]any{{
		"name": "Pool0", "status": "ONLINE", "healthy": true,
		"size": poolSize, "free": poolFree,
	}}
}

func dataset(id string, used, quota int64, source string) map[string]any {
	return map[string]any{
		"id": id, "type": "FILESYSTEM", "mountpoint": "/mnt/" + id,
		"used":     map[string]any{"parsed": used},
		"refquota": map[string]any{"parsed": quota},
		"user_properties": map[string]any{
			"io.truenas.csi:managed": map[string]any{
				"value": "truenas-csi", "source": source,
			},
		},
	}
}

// serveAppliance gives the fake the three methods the collector calls.
func serveAppliance(s *fake.Server, datasets []map[string]any, sessions int) {
	s.HandleValue("pool.query", poolResult())
	s.HandleValue("pool.dataset.query", datasets)
	list := make([]map[string]any, sessions)
	for i := range list {
		list[i] = map[string]any{"initiator": "iqn.example:host"}
	}
	s.HandleValue("iscsi.global.sessions", list)
	// The NFS counterpart. Seeded unconditionally so a collector that stops
	// reading it fails on the assertion rather than on a missing method.
	s.HandleValue("nfs.client_count", nfsClientsSeed)
}

// nfsClientsSeed is the appliance-wide NFS client count the fake reports.
const nfsClientsSeed = 3

func testRegistry(t *testing.T, backends map[string]string) *backend.Registry {
	t.Helper()
	cfg := &config.Config{Backends: map[string]config.Backend{}}
	for name, endpoint := range backends {
		cfg.Backends[name] = config.Backend{
			Name: name, Endpoint: endpoint, Username: "truenas_admin",
			APIKey: "8-TestFixtureNotARealApiKey", Pool: "Pool0",
			ParentDataset: "k8s", InsecureSkipVerify: true,
		}
	}
	reg, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// gather registers the collector in its own registry and returns everything it
// exports, so the driver's global metrics cannot mask a missing series.
func gather(t *testing.T, c *Collector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return mfs
}

func value(t *testing.T, mfs []*dto.MetricFamily, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			got := map[string]string{}
			for _, lp := range m.GetLabel() {
				got[lp.GetName()] = lp.GetValue()
			}
			match := len(got) == len(labels)
			for k, v := range labels {
				if got[k] != v {
					match = false
				}
			}
			if !match {
				continue
			}
			switch {
			case m.Gauge != nil:
				return m.Gauge.GetValue(), true
			case m.Counter != nil:
				return m.Counter.GetValue(), true
			case m.Untyped != nil:
				return m.Untyped.GetValue(), true
			}
		}
	}
	return 0, false
}

func mustValue(t *testing.T, mfs []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	v, ok := value(t, mfs, name, labels)
	if !ok {
		t.Fatalf("metric %s%v was not exported", name, labels)
	}
	return v
}

func TestCollectorExportsPoolMetrics(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	serveAppliance(srv, []map[string]any{
		dataset("Pool0/k8s/pvc-1", 1<<30, 5<<30, "LOCAL"),
	}, 3)

	c := New(testRegistry(t, map[string]string{"nas1": srv.URL()}), time.Minute)
	c.CollectOnce(context.Background())

	mfs := gather(t, c)
	pool := map[string]string{"backend": "nas1", "pool": "Pool0"}

	if got := mustValue(t, mfs, "truenas_pool_size_bytes", pool); got != poolSize {
		t.Errorf("pool size = %v, want %d (plain integers from pool.query must decode)", got, int64(poolSize))
	}
	if got := mustValue(t, mfs, "truenas_pool_free_bytes", pool); got != poolFree {
		t.Errorf("pool free = %v, want %d", got, int64(poolFree))
	}
	if got := mustValue(t, mfs, "truenas_pool_used_bytes", pool); got != poolSize-poolFree {
		t.Errorf("pool used = %v, want %d", got, int64(poolSize-poolFree))
	}
	if got := mustValue(t, mfs, "truenas_pool_healthy", pool); got != 1 {
		t.Errorf("pool healthy = %v, want 1", got)
	}
	if got := mustValue(t, mfs, "truenas_iscsi_sessions", map[string]string{"backend": "nas1"}); got != 3 {
		t.Errorf("iscsi sessions = %v, want 3", got)
	}
	if _, ok := value(t, mfs, "truenas_collection_duration_seconds", map[string]string{"backend": "nas1"}); !ok {
		t.Error("collection duration was not exported")
	}
	if got := mustValue(t, mfs, "truenas_collection_errors_total", map[string]string{"backend": "nas1"}); got != 0 {
		t.Errorf("collection errors = %v, want 0 on a healthy appliance", got)
	}
}

// TestCollectorOnlyExportsOwnedDatasets: an INHERITED marker is somebody else's
// data. Exporting it leaks their capacity figures and unbounds the cardinality.
func TestCollectorOnlyExportsOwnedDatasets(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	serveAppliance(srv, []map[string]any{
		dataset("Pool0/k8s/pvc-owned", 1<<30, 5<<30, "LOCAL"),
		dataset("Pool0/k8s/somebody-else", 9<<30, 0, "INHERITED"),
	}, 0)

	c := New(testRegistry(t, map[string]string{"nas1": srv.URL()}), time.Minute)
	c.CollectOnce(context.Background())
	mfs := gather(t, c)

	owned := map[string]string{"backend": "nas1", "dataset": "Pool0/k8s/pvc-owned"}
	if got := mustValue(t, mfs, "truenas_dataset_used_bytes", owned); got != float64(int64(1)<<30) {
		t.Errorf("owned dataset used = %v, want %d", got, int64(1)<<30)
	}
	if got := mustValue(t, mfs, "truenas_dataset_quota_bytes", owned); got != float64(int64(5)<<30) {
		t.Errorf("owned dataset quota = %v, want %d", got, int64(5)<<30)
	}

	notOurs := map[string]string{"backend": "nas1", "dataset": "Pool0/k8s/somebody-else"}
	if _, ok := value(t, mfs, "truenas_dataset_used_bytes", notOurs); ok {
		t.Error("a dataset whose ownership marker is INHERITED must never be exported: " +
			"it is somebody else's data")
	}
}

// TestCollectorSurvivesUnreachableBackend: one dead appliance must not stop the
// others, and must not zero the last known values of the appliance that broke.
func TestCollectorSurvivesUnreachableBackend(t *testing.T) {
	good := fake.Start(t, fake.Options{})
	serveAppliance(good, []map[string]any{
		dataset("Pool0/k8s/pvc-1", 1<<30, 5<<30, "LOCAL"),
	}, 2)

	flaky := fake.Start(t, fake.Options{})
	serveAppliance(flaky, nil, 1)

	c := New(testRegistry(t, map[string]string{
		"nas1": good.URL(),
		"nas2": flaky.URL(),
		// port 1 is never listening: an appliance that cannot be dialled at all
		"dead": "wss://127.0.0.1:1/api/current",
	}), time.Minute)
	c.CollectOnce(context.Background())

	mfs := gather(t, c)
	if got := mustValue(t, mfs, "truenas_pool_size_bytes",
		map[string]string{"backend": "nas1", "pool": "Pool0"}); got != poolSize {
		t.Errorf("healthy backend must still be collected, got %v", got)
	}
	if got := mustValue(t, mfs, "truenas_collection_errors_total",
		map[string]string{"backend": "dead"}); got < 1 {
		t.Errorf("unreachable backend must record an error, got %v", got)
	}
	if _, ok := value(t, mfs, "truenas_pool_size_bytes",
		map[string]string{"backend": "dead", "pool": "Pool0"}); ok {
		t.Error("a backend that never answered must export no pool series")
	}

	// Now break the appliance that had already been collected once.
	flaky.Handle("pool.query", func([]json.RawMessage) (any, error) {
		return nil, &fake.RPCError{Code: -32001, ErrName: "EFAULT", Reason: "appliance is busy"}
	})
	c.CollectOnce(context.Background())

	mfs = gather(t, c)
	if got := mustValue(t, mfs, "truenas_pool_size_bytes",
		map[string]string{"backend": "nas2", "pool": "Pool0"}); got != poolSize {
		t.Errorf("a failed poll must keep the last known value, got %v (zeroing turns "+
			"a collection outage into a fake capacity alert)", got)
	}
	if got := mustValue(t, mfs, "truenas_collection_errors_total",
		map[string]string{"backend": "nas2"}); got < 1 {
		t.Errorf("a failed poll must be counted, got %v", got)
	}
}

// TestCollectorDoesNotPollOnScrape: a slow appliance must never stall a scrape.
func TestCollectorDoesNotPollOnScrape(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	serveAppliance(srv, []map[string]any{
		dataset("Pool0/k8s/pvc-1", 1<<30, 5<<30, "LOCAL"),
	}, 1)

	c := New(testRegistry(t, map[string]string{"nas1": srv.URL()}), time.Minute)
	c.CollectOnce(context.Background())
	after := srv.CallsTo("pool.query")
	if after == 0 {
		t.Fatal("the collector never polled the appliance")
	}

	for i := 0; i < 5; i++ {
		gather(t, c)
	}
	if got := srv.CallsTo("pool.query"); got != after {
		t.Errorf("scraping issued %d extra appliance calls: collection must happen "+
			"on the poll loop, never on the scrape path", got-after)
	}
}

// TestCollectorLabelsHaveBoundedCardinality: only backend, pool and dataset may
// ever be label names. A volume id or node name would be unbounded.
func TestCollectorLabelsHaveBoundedCardinality(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	serveAppliance(srv, []map[string]any{
		dataset("Pool0/k8s/pvc-1", 1<<30, 5<<30, "LOCAL"),
	}, 1)

	c := New(testRegistry(t, map[string]string{"nas1": srv.URL()}), time.Minute)
	c.CollectOnce(context.Background())

	allowed := map[string]bool{"backend": true, "pool": true, "dataset": true}
	for _, mf := range gather(t, c) {
		if !strings.HasPrefix(mf.GetName(), "truenas_") {
			t.Errorf("unexpected metric %q", mf.GetName())
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if !allowed[lp.GetName()] {
					t.Errorf("%s carries forbidden label %q: labels must stay bounded",
						mf.GetName(), lp.GetName())
				}
				if strings.Contains(lp.GetValue(), "TestFixtureNotARealApiKey") {
					t.Errorf("%s label %q leaked a credential", mf.GetName(), lp.GetName())
				}
			}
		}
	}
}

// TestStartSkipsWhenNoBackendIsReachable: the collector must decline to start
// rather than crash-loop the controller against a dead appliance.
func TestStartSkipsWhenNoBackendIsReachable(t *testing.T) {
	c := New(testRegistry(t, map[string]string{"dead": "wss://127.0.0.1:1/api/current"}), time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Start(ctx); err == nil {
		t.Fatal("Start must report that no backend was reachable")
	}
}

// TestCollectorPublishesApplianceDiagnostics closes a gap between a set of
// declared metrics and any deployment that could export them.
//
// internal/obs declares truenas_appliance_disk_healthy, _scrub_state,
// _scrub_errors and _alerts, and internal/pooladmin sets them — but the only
// caller of pooladmin.Collect was the `truenas-csi pool` CLI, a process that
// sets a gauge and immediately exits. Confirmed against a live controller:
// none of that family appeared on /metrics, on either replica.
//
// It matters most for disks. TrueNAS 25.10 exposes no SMART API at all, so an
// alert naming a disk is the only warning anyone gets, and there was no way to
// alert on it.
func TestCollectorPublishesApplianceDiagnostics(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	serveAppliance(srv, []map[string]any{
		dataset("Pool0/k8s/pvc-1", 1<<30, 5<<30, "LOCAL"),
	}, 3)
	srv.HandleValue("disk.query", []map[string]any{
		{"name": "sda", "serial": "S1", "model": "HGST", "size": 1 << 40, "pool": "Pool0"},
		{"name": "sdb", "serial": "S2", "model": "HGST", "size": 1 << 40, "pool": "Pool0"},
	})
	srv.HandleValue("alert.list", []map[string]any{{
		"id": "a1", "level": "WARNING", "klass": "SMARTUncorrectedErrors",
		"formatted": "2 uncorrectable errors reported for sdb (S2).", "dismissed": false,
	}})

	c := New(testRegistry(t, map[string]string{"nas1": srv.URL()}), time.Minute)
	c.CollectOnce(context.Background())

	// The gauges live on the default registry, which is what the driver's
	// /metrics handler serves.
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	healthy := func(disk string) float64 {
		return mustValue(t, mfs, "truenas_appliance_disk_healthy",
			map[string]string{"backend": "nas1", "disk": disk})
	}
	if got := healthy("sda"); got != 1 {
		t.Errorf("sda healthy = %v, want 1", got)
	}
	if got := healthy("sdb"); got != 0 {
		t.Errorf("sdb healthy = %v, want 0: the appliance is alerting on its serial", got)
	}
	if got := mustValue(t, mfs, "truenas_appliance_alerts",
		map[string]string{"backend": "nas1", "level": "WARNING"}); got != 1 {
		t.Errorf("WARNING alerts = %v, want 1", got)
	}
}
