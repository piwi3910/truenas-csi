package obs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMetricsAndLogAttribution catches a counter wired to the wrong label values and a
// logger that drops the volume attribution carried by the context.
func TestMetricsAndLogAttribution(t *testing.T) {
	okBefore := testutil.ToFloat64(csiCalls.WithLabelValues("CreateVolume", "false"))
	errBefore := testutil.ToFloat64(csiCalls.WithLabelValues("CreateVolume", "true"))
	mwBefore := testutil.ToFloat64(middlewareCalls.WithLabelValues("pool.dataset.create", "false"))

	ObserveCSI("CreateVolume", nil, time.Millisecond)
	ObserveMiddleware("pool.dataset.create", nil, time.Millisecond)

	if got := testutil.ToFloat64(csiCalls.WithLabelValues("CreateVolume", "false")) - okBefore; got != 1 {
		t.Errorf("truenas_csi_calls_total{method=CreateVolume,error=false} delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(csiCalls.WithLabelValues("CreateVolume", "true")) - errBefore; got != 0 {
		t.Errorf("error=true counter moved on a successful call: delta = %v", got)
	}
	if got := testutil.ToFloat64(middlewareCalls.WithLabelValues("pool.dataset.create", "false")) - mwBefore; got != 1 {
		t.Errorf("middleware counter delta = %v, want 1", got)
	}

	ObserveCSI("CreateVolume", errors.New("boom"), 2*time.Millisecond)
	if got := testutil.ToFloat64(csiCalls.WithLabelValues("CreateVolume", "true")) - errBefore; got != 1 {
		t.Errorf("truenas_csi_calls_total{method=CreateVolume,error=true} delta = %v, want 1", got)
	}

	if n := testutil.CollectAndCount(csiDuration, "truenas_csi_call_duration_seconds"); n == 0 {
		t.Error("truenas_csi_call_duration_seconds has no samples")
	}
	if n := testutil.CollectAndCount(middlewareDuration, "truenas_csi_middleware_duration_seconds"); n == 0 {
		t.Error("truenas_csi_middleware_duration_seconds has no samples")
	}

	SetBackendUp("nas1", true)
	if got := testutil.ToFloat64(backendUp.WithLabelValues("nas1")); got != 1 {
		t.Errorf("truenas_csi_backend_up{backend=nas1} = %v, want 1", got)
	}
	SetBackendUp("nas1", false)
	if got := testutil.ToFloat64(backendUp.WithLabelValues("nas1")); got != 0 {
		t.Errorf("truenas_csi_backend_up{backend=nas1} = %v, want 0", got)
	}

	SetOrphanCount("nas1", 3)
	if got := testutil.ToFloat64(orphanedVolumes.WithLabelValues("nas1")); got != 3 {
		t.Errorf("truenas_csi_orphaned_volumes{backend=nas1} = %v, want 3", got)
	}

	const volumeID = "nas1/nfs/Pool0/k8s/pvc-1"
	var buf bytes.Buffer
	SetLogOutput(&buf, slog.LevelDebug)
	t.Cleanup(func() { SetLogOutput(nil, slog.LevelInfo) })

	Logger(WithVolume(context.Background(), volumeID)).Info("created dataset")
	out := buf.String()
	if !strings.Contains(out, "volume_id") || !strings.Contains(out, volumeID) {
		t.Errorf("log record missing volume_id attribution: %q", out)
	}

	buf.Reset()
	Logger(context.Background()).Info("no volume in context")
	if strings.Contains(buf.String(), "volume_id") {
		t.Errorf("volume_id emitted without a volume in context: %q", buf.String())
	}
}

// TestReadyHandlerGatesOnBackend catches a readiness endpoint that reports ready before
// any backend has ever connected.
func TestReadyHandlerGatesOnBackend(t *testing.T) {
	ready := ReadyHandler()

	rec := httptest.NewRecorder()
	ready.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz before MarkReady = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	health := httptest.NewRecorder()
	HealthHandler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Errorf("healthz = %d, want 200", health.Code)
	}

	MarkReady()

	rec = httptest.NewRecorder()
	ready.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("readyz after MarkReady = %d, want 200", rec.Code)
	}
}
