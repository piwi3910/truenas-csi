package csi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ctlWithPool builds a controller against a fake appliance whose pool reports
// the given size and free space, with the reservation the backend is configured
// with.
func ctlWithPool(t *testing.T, size, free int64, reservedBytes int64, reservedPercent float64) (csipb.ControllerServer, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	s.Handle("pool.query", func([]json.RawMessage) (any, error) {
		return []any{map[string]any{
			"name": "Pool0", "status": "ONLINE", "healthy": true,
			"free": map[string]any{"parsed": free},
			"size": map[string]any{"parsed": size},
		}}, nil
	})
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
			ReservedBytes: reservedBytes, ReservedPercent: reservedPercent},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return NewController(r, cfg), s
}

// TestCapacityReportsFreeMinusReserved is the whole point of the reservation:
// what the scheduler is told must already exclude the space TrueNAS needs for
// its own system datasets, snapshots and replication targets. Reporting the raw
// pool free space is how a pool gets filled to 100% by PVCs.
func TestCapacityReportsFreeMinusReserved(t *testing.T) {
	const size = int64(1000)
	for _, tc := range []struct {
		name    string
		free    int64
		bytes   int64
		percent float64
		want    int64
	}{
		{name: "bytes only", free: 400, bytes: 100, want: 300},
		{name: "percent only", free: 400, percent: 10, want: 300}, // 10% of 1000
		{name: "both set, larger wins", free: 400, bytes: 100, percent: 30, want: 100},
		{name: "reserve larger than free clamps to zero", free: 50, bytes: 900, want: 0},
		{name: "no reservation reports pool free", free: 400, want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := ctlWithPool(t, size, tc.free, tc.bytes, tc.percent)
			resp, err := c.GetCapacity(context.Background(), &csipb.GetCapacityRequest{
				Parameters: map[string]string{"backend": "nas1", "protocol": "nfs"}})
			if err != nil {
				t.Fatal(err)
			}
			if got := resp.GetAvailableCapacity(); got != tc.want {
				t.Fatalf("reported %d, want %d (pool free %d, size %d, reserve bytes %d percent %v)",
					got, tc.want, tc.free, size, tc.bytes, tc.percent)
			}
			if resp.GetAvailableCapacity() < 0 {
				t.Fatalf("reported a negative capacity %d", resp.GetAvailableCapacity())
			}
		})
	}
}

// TestCreateVolumeRefusesEatingIntoReserve proves the reservation is enforced
// and not merely advertised: a claim that fits in the pool but not outside the
// reserve is refused with ResourceExhausted, and the message states the three
// numbers an operator needs to understand the refusal.
func TestCreateVolumeRefusesEatingIntoReserve(t *testing.T) {
	shared = newCounting()
	// Pool of 1000 with 400 free and a 300-byte reserve: 100 bytes are usable.
	c, _ := ctlWithPool(t, 1000, 400, 300, 0)

	_, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-too-big",
		VolumeCapabilities: testCaps(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 200},
		Parameters:         params(),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("want ResourceExhausted, got %v", err)
	}
	for _, want := range []string{"400", "300", "200"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not state %s (pool free / reserve / request)", err, want)
		}
	}
	if shared.live() != 0 {
		t.Fatalf("a refused request still created %d volume(s)", shared.live())
	}

	// A claim that fits outside the reserve is created normally.
	if _, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name:               "pvc-fits",
		VolumeCapabilities: testCaps(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 100},
		Parameters:         params(),
	}); err != nil {
		t.Fatalf("a claim inside the usable capacity was refused: %v", err)
	}
}
