package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestHealthCheckIsBounded is the whole point of the monitor: the failure it
// exists to detect — a hung NFS server — is exactly the failure that makes a
// naive statfs never return. A monitor that blocks on the thing it monitors
// reports nothing, forever.
func TestHealthCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	const id = "nas1/nfs/tank/k8s/pvc-bounded"
	m := NewHealthMonitor()
	m.Timeout = 50 * time.Millisecond
	m.Statfs = func(string) error {
		<-release // never returns while the test runs, like a hung mount
		return nil
	}
	m.Track(HealthTarget{VolumeID: id, Protocol: ProtocolNFS, Path: t.TempDir()})
	defer m.Forget(id)

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.CheckOnce(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("CheckOnce blocked on a hung filesystem call: the health check " +
			"must run under a deadline and give up on it, not wait for it")
	}

	abnormal, msg := m.Condition(id)
	if !abnormal {
		t.Fatal("a health check that timed out must mark the volume abnormal")
	}
	if !strings.Contains(msg, "timed out") {
		t.Errorf("condition message should say the check timed out, got %q", msg)
	}
}

// TestHealthTransitionLogsOnce pins the log volume. A node with 100 staged
// volumes and a dead NAS polling every 10s would emit 36,000 identical lines an
// hour; only the transitions carry information.
func TestHealthTransitionLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	obs.SetLogOutput(&buf, slog.LevelDebug)
	t.Cleanup(func() { obs.SetLogOutput(nil, slog.LevelInfo) })

	const id = "nas1/nfs/tank/k8s/pvc-transition"
	var mu sync.Mutex
	broken := true
	m := NewHealthMonitor()
	m.Timeout = time.Second
	m.Statfs = func(string) error {
		mu.Lock()
		defer mu.Unlock()
		if broken {
			return errors.New("stale file handle")
		}
		return nil
	}
	m.Track(HealthTarget{VolumeID: id, Protocol: ProtocolNFS, Path: t.TempDir()})
	defer m.Forget(id)

	for i := 0; i < 4; i++ {
		m.CheckOnce(context.Background())
	}
	if n := strings.Count(buf.String(), healthLostMessage); n != 1 {
		t.Errorf("four failing polls logged the loss %d times, want exactly 1:\n%s",
			n, buf.String())
	}

	mu.Lock()
	broken = false
	mu.Unlock()
	for i := 0; i < 4; i++ {
		m.CheckOnce(context.Background())
	}
	if n := strings.Count(buf.String(), healthRecoveredMessage); n != 1 {
		t.Errorf("four healthy polls logged the recovery %d times, want exactly 1:\n%s",
			n, buf.String())
	}
	if n := strings.Count(buf.String(), healthLostMessage); n != 1 {
		t.Errorf("the loss line was re-emitted after recovery (%d times)", n)
	}
}

// TestHealthMetricsHaveBoundedCardinality guards the rule that a volume id must
// never become a Prometheus label: one series per PVC ever created would kill
// the scrape target long before an operator noticed.
func TestHealthMetricsHaveBoundedCardinality(t *testing.T) {
	m := NewHealthMonitor()
	m.Timeout = time.Second
	m.Statfs = func(string) error { return nil }
	m.Dial = func(context.Context, string) error { return nil }

	const volumes = 50
	ids := make([]string, 0, volumes)
	for i := 0; i < volumes; i++ {
		id := fmt.Sprintf("nas1/nfs/tank/k8s/pvc-cardinality-%d-0123456789abcdef", i)
		ids = append(ids, id)
		m.Track(HealthTarget{
			VolumeID: id, Protocol: ProtocolNFS, Path: t.TempDir(),
			Backend: "nas1", DataAddr: "192.0.2.10:2049",
		})
	}
	// Several passes: the series count must not grow with the poll count.
	for i := 0; i < 3; i++ {
		m.CheckOnce(context.Background())
	}

	hashes := map[string]bool{}
	for _, id := range ids {
		hashes[obs.HashVolumeID(id)] = true
	}

	hashRE := regexp.MustCompile(`^[0-9a-f]{12}$`)
	series := healthSeries(t)
	mine := 0
	for _, s := range series {
		labels := labelsOf(s)
		hash := labels["volume_id_hash"]
		if !hashRE.MatchString(hash) {
			t.Errorf("volume_id_hash %q is not a short stable hash", hash)
		}
		for k, v := range labels {
			for _, id := range ids {
				if strings.Contains(v, id) {
					t.Fatalf("label %s carries a raw volume id: %q", k, v)
				}
			}
		}
		if hashes[hash] {
			mine++
		}
	}
	if mine != volumes {
		t.Errorf("volume health exported %d series for %d volumes: one series per "+
			"volume is the bound, not one per poll", mine, volumes)
	}

	if got := len(backendSeries(t)); got == 0 {
		t.Error("truenas_csi_node_backend_reachable exported no series")
	}
	for _, s := range backendSeries(t) {
		if _, ok := labelsOf(s)["backend"]; !ok {
			t.Error("backend reachability must be labelled by backend name only")
		}
		if len(s.GetLabel()) != 1 {
			t.Errorf("backend reachability has %d labels, want just backend", len(s.GetLabel()))
		}
	}

	// Forgetting a volume must retire its series, or an evicted PVC leaks one
	// forever.
	for _, id := range ids {
		m.Forget(id)
	}
	for _, s := range healthSeries(t) {
		if hashes[labelsOf(s)["volume_id_hash"]] {
			t.Fatal("a forgotten volume kept its health series")
		}
	}
}

func gather(t *testing.T, name string) []*dto.Metric {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()
		}
	}
	return nil
}

func healthSeries(t *testing.T) []*dto.Metric {
	t.Helper()
	return gather(t, "truenas_csi_volume_health")
}

func backendSeries(t *testing.T) []*dto.Metric {
	t.Helper()
	return gather(t, "truenas_csi_node_backend_reachable")
}

func labelsOf(m *dto.Metric) map[string]string {
	out := map[string]string{}
	for _, l := range m.GetLabel() {
		out[l.GetName()] = l.GetValue()
	}
	return out
}

// TestHealthConditionSurfacedInStats checks the node data path itself carries
// the condition, before any gRPC adapter is involved.
func TestHealthConditionSurfacedInStats(t *testing.T) {
	const id = "nas1/nfs/tank/k8s/pvc-stats"
	dir := t.TempDir()
	n := NewNode("worker-1", &Preflight{}, nil)
	n.Health().Timeout = time.Second
	n.Health().Statfs = func(string) error { return errors.New("stale file handle") }
	n.Health().Track(HealthTarget{VolumeID: id, Protocol: ProtocolNFS, Path: dir})
	defer n.Health().Forget(id)
	n.Health().CheckOnce(context.Background())

	out, err := n.Stats(context.Background(), StatsRequest{VolumeID: id, VolumePath: dir})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if !out.Abnormal {
		t.Error("an unhealthy volume must be reported as abnormal in its stats")
	}
	if !strings.Contains(out.Message, "stale file handle") {
		t.Errorf("condition message should carry the cause, got %q", out.Message)
	}
	if len(out.Usage) == 0 {
		t.Error("usage must still be reported for an unhealthy volume")
	}
}
