package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ctlWithServer(t *testing.T, s *fake.Server) csipb.ControllerServer {
	t.Helper()
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return NewController(r, cfg)
}

// TestCapacityMatchesWritableSpace uses the figures the validation appliance
// reported for its 12-disk RAIDZ2 Pool0.
//
// The raw pool numbers are served too, and are deliberately much larger: this
// test used to read them, and a driver that goes back to reading them would
// advertise a third more room than the pool can hold.
func TestCapacityMatchesWritableSpace(t *testing.T) {
	const (
		writableFree = int64(33352704796320) // root dataset "available"
		writableUsed = int64(21466962973168) // root dataset "used"
		rawFree      = int64(45132604108800) // pool.query "free" — includes parity
	)
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(fmt.Sprintf(
			`[{"name":"Pool0","status":"ONLINE","healthy":true,
			   "free":{"parsed":%d},"size":{"parsed":72000831750144}}]`, rawFree)), &v)
		return v, nil
	})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(fmt.Sprintf(
			`[{"id":"Pool0","type":"FILESYSTEM",
			   "available":{"parsed":%d},"used":{"parsed":%d}}]`,
			writableFree, writableUsed)), &v)
		return v, nil
	})
	c := ctlWithServer(t, s)

	resp, err := c.GetCapacity(context.Background(), &csipb.GetCapacityRequest{
		Parameters: map[string]string{"backend": "nas1", "protocol": "nfs"}})
	if err != nil {
		t.Fatal(err)
	}
	// No reservation is configured here, so the whole writable free space is
	// on offer — and nothing of the raw figure.
	if got := resp.GetAvailableCapacity(); got != writableFree {
		t.Fatalf("reported %d, want the writable free space %d (raw free is %d)",
			got, writableFree, rawFree)
	}
	if got := resp.GetMaximumVolumeSize().GetValue(); got != writableFree {
		t.Fatalf("maximum volume size %d, want %d", got, writableFree)
	}

	_, err = c.GetCapacity(context.Background(), &csipb.GetCapacityRequest{
		Parameters: map[string]string{"backend": "nope", "protocol": "nfs"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown backend: want InvalidArgument, got %v", err)
	}
}

// TestListVolumesPaginates also proves the ownership filter: an inherited
// marker is somebody else's data and must never be listed as ours.
func TestListVolumesPaginates(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		out := make([]any, 0, 251)
		for i := 0; i < 250; i++ {
			out = append(out, map[string]any{
				"id": fmt.Sprintf("Pool0/k8s/pvc-%03d", i), "type": "FILESYSTEM",
				"refquota": map[string]any{"parsed": 1 << 30},
				"user_properties": map[string]any{
					"io.truenas.csi:managed": map[string]any{"value": "truenas-csi", "source": "LOCAL"}},
			})
		}
		// Not ours: the marker is inherited from a parent, not set locally.
		out = append(out, map[string]any{
			"id": "Pool0/k8s/somebody-elses-data", "type": "FILESYSTEM",
			"refquota": map[string]any{"parsed": 1 << 30},
			"user_properties": map[string]any{
				"io.truenas.csi:managed": map[string]any{"value": "truenas-csi", "source": "INHERITED"}},
		})
		return out, nil
	})
	c := ctlWithServer(t, s)

	resp, err := c.ListVolumes(context.Background(), &csipb.ListVolumesRequest{MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetEntries()) != 100 {
		t.Fatalf("page size %d, want 100", len(resp.GetEntries()))
	}
	if resp.GetNextToken() == "" {
		t.Fatal("want a next_token with 250 volumes and a page of 100")
	}

	total := len(resp.GetEntries())
	token := resp.GetNextToken()
	for token != "" {
		page, err := c.ListVolumes(context.Background(),
			&csipb.ListVolumesRequest{MaxEntries: 100, StartingToken: token})
		if err != nil {
			t.Fatal(err)
		}
		total += len(page.GetEntries())
		token = page.GetNextToken()
	}
	if total != 250 {
		t.Fatalf("listed %d volumes across all pages, want 250 owned ones "+
			"(the 251st carries an INHERITED marker and is not ours)", total)
	}

	_, err = c.ListVolumes(context.Background(),
		&csipb.ListVolumesRequest{StartingToken: "not-a-real-token"})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("invalid token: want Aborted, got %v", err)
	}
}
