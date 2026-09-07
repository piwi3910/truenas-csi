package health_test

import (
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/operator/internal/health"
)

// The exposition below is the shape internal/obs actually emits: a gauge per
// backend for the connection, and a gauge per backend for the orphan count the
// report-only reconciler last found.
const exposition = `
# HELP truenas_csi_backend_up 1 when the backend's websocket connection is established, 0 otherwise.
# TYPE truenas_csi_backend_up gauge
truenas_csi_backend_up{backend="nas1"} 1
truenas_csi_backend_up{backend="nas2"} 0
# HELP truenas_csi_orphaned_volumes Datasets owned by this driver with no matching PersistentVolume, by backend.
# TYPE truenas_csi_orphaned_volumes gauge
truenas_csi_orphaned_volumes{backend="nas1"} 3
truenas_csi_orphaned_volumes{backend="nas2"} 0
`

func TestParseReadsBackendHealthAndOrphans(t *testing.T) {
	got, err := health.Parse(strings.NewReader(exposition))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("parsed %d backends, want 2", len(got))
	}
	if !got["nas1"].Up {
		t.Error("nas1 should be up")
	}
	if got["nas1"].Orphans != 3 {
		t.Errorf("nas1 orphans = %d, want 3", got["nas1"].Orphans)
	}
	if got["nas2"].Up {
		t.Error("nas2 should be down")
	}
}

func TestParseIgnoresUnrelatedMetrics(t *testing.T) {
	got, err := health.Parse(strings.NewReader(`
# TYPE go_goroutines gauge
go_goroutines 42
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %v from an exposition with no driver metrics", got)
	}
}
