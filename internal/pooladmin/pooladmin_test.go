package pooladmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

const poolsJSON = `[{
  "name":"Pool0","status":"ONLINE","healthy":true,"fragmentation":"7",
  "size":72000000000000,"free":44900000000000,
  "scan":{"function":"SCRUB","state":"FINISHED","errors":0,"percentage":100.0,
          "start_time":{"$date":1749000000000},"end_time":{"$date":1749003600000}}
}]`

const disksJSON = `[
 {"name":"sda","serial":"S1","model":"HGST","size":12000000000000,"pool":"Pool0","togglesmart":true},
 {"name":"sdb","serial":"S2","model":"HGST","size":12000000000000,"pool":"Pool0","togglesmart":true}]`

const smartJSON = `[
 {"disk":"sda","tests":[{"status":"SUCCESS","description":"Short offline","lifetime":100}]},
 {"disk":"sdb","tests":[{"status":"FAILED","description":"Short offline","lifetime":120}]}]`

const alertsJSON = `[
 {"id":"a1","level":"CRITICAL","klass":"PoolDegraded","formatted":"Pool0 is DEGRADED","dismissed":false},
 {"id":"a2","level":"WARNING","klass":"SMART","formatted":"sdb SMART failure","dismissed":false},
 {"id":"a3","level":"WARNING","klass":"Old","formatted":"already seen","dismissed":true}]`

func backendCfg(url string) config.Backend {
	return config.Backend{
		Name: "nas1", Endpoint: url, Username: "truenas_admin", APIKey: "8-x",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	}
}

func raw(t *testing.T, body string) func([]json.RawMessage) (any, error) {
	t.Helper()
	return func([]json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v, nil
	}
}

func appliance(t *testing.T) (Backend, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.query", raw(t, poolsJSON))
	s.Handle("disk.query", raw(t, disksJSON))
	s.Handle("smart.test.results", raw(t, smartJSON))
	s.Handle("alert.list", raw(t, alertsJSON))
	c, err := truenas.Dial(context.Background(), backendCfg(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return Backend{Name: "nas1", Client: c}, s
}

// readOnly classifies a middleware method independently of the package under
// test: anything that is not a recognised query/read verb counts as mutating.
func readOnly(method string) bool {
	if method == "auth.login_ex" {
		return true
	}
	verb := method
	if i := strings.LastIndex(method, "."); i >= 0 {
		verb = method[i+1:]
	}
	switch verb {
	case "query", "list", "config", "get_instance", "results", "temperatures", "get_disks":
		return true
	}
	return false
}

// TestPoolAdminIsReadOnly is the test that keeps this package honest: it drives
// every exported function and fails if the appliance ever saw a call that is not
// a query. Managing the pool is the appliance's job; the driver only reports.
func TestPoolAdminIsReadOnly(t *testing.T) {
	b, s := appliance(t)
	ctx := context.Background()

	if _, err := PoolStatus(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := DiskHealth(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := Alerts(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(ctx, b); err != nil {
		t.Fatal(err)
	}

	calls := s.Calls()
	if len(calls) == 0 {
		t.Fatal("no middleware calls were made; the test would pass vacuously")
	}
	for _, c := range calls {
		if !readOnly(c) {
			t.Fatalf("pooladmin issued a mutating middleware call %q (all calls: %v)", c, calls)
		}
	}

	// The package must also refuse a mutating method from the inside, so a
	// future edit cannot smuggle one through the shared call helper.
	if err := query(ctx, b, nil, "pool.scrub.run", "Pool0"); !errors.Is(err, ErrMutatingCall) {
		t.Fatalf("query() must refuse a mutating method, got %v", err)
	}
	if s.CallsTo("pool.scrub.run") != 0 {
		t.Fatal("a refused method must never reach the appliance")
	}
}

// TestPoolAdminReportsScrubAndAlerts catches a diagnostic that silently drops
// the fields an operator consults instead of opening the TrueNAS UI.
func TestPoolAdminReportsScrubAndAlerts(t *testing.T) {
	b, _ := appliance(t)
	ctx := context.Background()

	pools, err := PoolStatus(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 {
		t.Fatalf("want 1 pool, got %d", len(pools))
	}
	p := pools[0]
	if p.Name != "Pool0" || p.Status != "ONLINE" || !p.Healthy {
		t.Errorf("pool health lost: %+v", p)
	}
	if p.Scrub.Function != "SCRUB" || p.Scrub.State != "FINISHED" || p.Scrub.Errors != 0 {
		t.Errorf("scrub state lost: %+v", p.Scrub)
	}
	if p.Scrub.End.IsZero() {
		t.Error("last scrub end time lost")
	}
	if p.SizeBytes != 72000000000000 || p.FreeBytes != 44900000000000 {
		t.Errorf("capacity lost: %+v", p)
	}
	if p.FragmentationPercent != 7 {
		t.Errorf("fragmentation lost: %v", p.FragmentationPercent)
	}
	if p.UsedPercent <= 0 || p.UsedPercent >= 100 {
		t.Errorf("capacity trend not computed: %v", p.UsedPercent)
	}

	disks, err := DiskHealth(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(disks) != 2 {
		t.Fatalf("want 2 disks, got %d", len(disks))
	}
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}
	if !byName["sda"].Healthy {
		t.Errorf("sda should be healthy: %+v", byName["sda"])
	}
	if byName["sdb"].Healthy {
		t.Errorf("a FAILED SMART result must not read as healthy: %+v", byName["sdb"])
	}
	if byName["sdb"].SMARTStatus != "FAILED" {
		t.Errorf("SMART summary lost: %+v", byName["sdb"])
	}

	alerts, err := Alerts(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 3 {
		t.Fatalf("want 3 alerts, got %d", len(alerts))
	}
	var crit int
	for _, a := range alerts {
		if a.Level == "CRITICAL" {
			crit++
		}
	}
	if crit != 1 {
		t.Errorf("want 1 CRITICAL alert, got %d", crit)
	}

	// The results must reach the operator through the existing metrics.
	if _, err := Collect(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"truenas_appliance_alerts",
		"truenas_appliance_scrub_state",
		"truenas_appliance_disk_healthy",
	} {
		if !gaugeExists(t, name) {
			t.Errorf("metric %s was never published", name)
		}
	}
	if v, ok := gaugeValue(t, "truenas_appliance_alerts", map[string]string{"backend": "nas1", "level": "WARNING"}); !ok || v != 2 {
		t.Errorf("truenas_appliance_alerts{level=WARNING} = %v (present=%v), want 2", v, ok)
	}
}

func gather(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

func gaugeExists(t *testing.T, name string) bool {
	t.Helper()
	return gather(t, name) != nil
}

func gaugeValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	mf := gather(t, name)
	if mf == nil {
		return 0, false
	}
metrics:
	for _, m := range mf.GetMetric() {
		got := map[string]string{}
		for _, l := range m.GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		for k, v := range labels {
			if got[k] != v {
				continue metrics
			}
		}
		return m.GetGauge().GetValue(), true
	}
	return 0, false
}

// TestDiskHealthAsksForPools pins the option without which disk.query answers
// "pool": null for every disk.
//
// Verified on 25.10.6: the plain call returns the pool field present and null,
// so nothing failed and the report simply showed a blank column on an appliance
// whose disks were all in a pool.
func TestDiskHealthAsksForPools(t *testing.T) {
	b, s := appliance(t)
	var sawExtra bool
	s.Handle("disk.query", func(params []json.RawMessage) (any, error) {
		for _, p := range params {
			if strings.Contains(string(p), `"pools":true`) ||
				strings.Contains(string(p), `"pools": true`) {
				sawExtra = true
			}
		}
		var v any
		if err := json.Unmarshal([]byte(disksJSON), &v); err != nil {
			t.Fatal(err)
		}
		return v, nil
	})
	if _, err := DiskHealth(context.Background(), b); err != nil {
		t.Fatalf("DiskHealth: %v", err)
	}
	if !sawExtra {
		t.Error("disk.query was called without extra.pools, so every disk would " +
			"be reported with no pool")
	}
}

// TestDiskHealthDoesNotClaimSMARTIsDisabled covers an appliance with no SMART
// API at all, which is every TrueNAS from 25.10: the smart.* namespace was
// removed and disk.query no longer carries "togglesmart".
//
// The driver asked for both, got nothing, and rendered the nothing as
// "disabled" — a statement about the operator's hardware that had never been
// checked, on every disk of every current appliance. Absent must stay absent.
func TestDiskHealthDoesNotClaimSMARTIsDisabled(t *testing.T) {
	b, s := appliance(t)
	s.Handle("smart.test.results", func([]json.RawMessage) (any, error) {
		return nil, fmt.Errorf("jsonrpc -32601: method not found")
	})
	disks, err := DiskHealth(context.Background(), b)
	if err != nil {
		t.Fatalf("DiskHealth: %v", err)
	}
	if len(disks) == 0 {
		t.Fatal("no disks reported")
	}
	for _, d := range disks {
		if d.SMARTAvailable {
			t.Errorf("disk %s reports SMART available on an appliance with no SMART API", d.Name)
		}
		if !d.Healthy {
			t.Errorf("disk %s was marked unhealthy merely because SMART is absent", d.Name)
		}
	}
}
