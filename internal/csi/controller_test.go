package csi

import (
	"context"
	"github.com/piwi3910/truenas-csi/internal/node"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// countingBackend records what the controller asks of it. It deliberately
// simulates a real appliance's idempotency: a second Create for an existing
// name returns the same volume rather than making another.
type countingBackend struct {
	mu      sync.Mutex
	creates atomic.Int64
	deletes atomic.Int64
	exist   map[string]int64
}

func newCounting() *countingBackend { return &countingBackend{exist: map[string]int64{}} }

func (b *countingBackend) Protocol() string { return "counting" }

func (b *countingBackend) Create(_ context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := r.ID.String()
	if sz, ok := b.exist[key]; ok {
		if sz != r.CapacityBytes {
			return nil, status.Error(codes.AlreadyExists, "size mismatch")
		}
		return &backend.Volume{ID: r.ID, CapacityBytes: sz}, nil
	}
	b.creates.Add(1)
	b.exist[key] = r.CapacityBytes
	return &backend.Volume{ID: r.ID, CapacityBytes: r.CapacityBytes}, nil
}

func (b *countingBackend) Delete(_ context.Context, id volume.ID) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.exist[id.String()]; ok {
		b.deletes.Add(1)
		delete(b.exist, id.String())
	}
	return nil
}

func (b *countingBackend) Expand(_ context.Context, _ volume.ID, n int64) (int64, error) {
	return n, nil
}

func (b *countingBackend) PublishContext(context.Context, volume.ID) (map[string]string, error) {
	return map[string]string{"ok": "1"}, nil
}

func (b *countingBackend) live() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.exist)
}

var shared = newCounting()

func init() {
	backend.Register("counting", func(truenas.API, backend.Options) backend.Backend { return shared })
}

func ctlWith(t *testing.T) (csipb.ControllerServer, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return NewController(r, cfg), s
}

func params() map[string]string {
	return map[string]string{"backend": "nas1", "protocol": "counting"}
}

// testCaps is the minimum a spec-conformant CreateVolume request must carry.
func testCaps() []*csipb.VolumeCapability {
	return []*csipb.VolumeCapability{{
		AccessType: &csipb.VolumeCapability_Mount{
			Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

// TestConcurrentCreateIsIdempotent is the property CSI actually demands: the
// orchestrator retries forever, so fifty identical calls must yield one dataset.
func TestConcurrentCreateIsIdempotent(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)

	var wg sync.WaitGroup
	ids := make([]string, 50)
	aborted := 0
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
				Name:               "pvc-same",
				Parameters:         params(),
				VolumeCapabilities: testCaps(), CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
			})
			mu.Lock()
			defer mu.Unlock()
			if status.Code(err) == codes.Aborted {
				aborted++ // a locked-out caller retries; that is correct behaviour
				return
			}
			if err != nil {
				t.Errorf("CreateVolume: %v", err)
				return
			}
			ids[i] = resp.GetVolume().GetVolumeId()
		}(i)
	}
	wg.Wait()

	if got := shared.creates.Load(); got != 1 {
		t.Fatalf("created %d datasets for 50 identical requests, want exactly 1", got)
	}
	var seen string
	for _, id := range ids {
		if id == "" {
			continue
		}
		if seen == "" {
			seen = id
		} else if id != seen {
			t.Fatalf("two different volume ids returned: %q and %q", seen, id)
		}
	}
	if seen != "nas1/counting/Pool0/k8s/pvc-same" {
		t.Fatalf("unexpected volume id %q", seen)
	}
}

func TestConcurrentCreateDelete(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)
	id := "nas1/counting/Pool0/k8s/pvc-race"

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
				Name: "pvc-race", Parameters: params(),
				VolumeCapabilities: testCaps(), CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}})
		}()
		go func() {
			defer wg.Done()
			_, _ = c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: id})
		}()
	}
	wg.Wait()
	if n := shared.live(); n > 1 {
		t.Fatalf("interleaved create/delete left %d volumes behind, want 0 or 1", n)
	}
}

func TestVolumeLocksReturnsAborted(t *testing.T) {
	l := NewVolumeLocks()
	release, ok := l.TryAcquire("v1")
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	if _, ok := l.TryAcquire("v1"); ok {
		t.Fatal("second acquire for the same id must fail so the caller gets ABORTED")
	}
	if _, ok := l.TryAcquire("v2"); !ok {
		t.Fatal("a different volume must not be blocked")
	}
	release()
	if _, ok := l.TryAcquire("v1"); !ok {
		t.Fatal("release should free the id")
	}
}

func TestDeleteVolumeAbsentReturnsOK(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)
	if _, err := c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{
		VolumeId: "nas1/counting/Pool0/k8s/pvc-never-existed"}); err != nil {
		t.Fatalf("deleting an absent volume must succeed, got %v", err)
	}
	// An unparseable id names nothing we created; CSI still requires success.
	if _, err := c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{
		VolumeId: "garbage"}); err != nil {
		t.Fatalf("unparseable id must succeed, got %v", err)
	}
}

// TestCreateVolumeRejectsUnconfinedID is the guard that protects ~20 TiB of
// pre-existing data: nothing may resolve outside the configured parent.
func TestCreateVolumeRejectsUnconfinedID(t *testing.T) {
	shared = newCounting()
	c, s := ctlWith(t)
	for _, name := range []string{"../../Home", "../Home", "..", ".", "a/b"} {
		_, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
			Name: name, Parameters: params(),
			VolumeCapabilities: testCaps(), CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("name %q: want InvalidArgument, got %v", name, err)
		}
	}
	if shared.creates.Load() != 0 {
		t.Fatal("a rejected name must never reach the backend")
	}
	for _, call := range s.Calls() {
		if call != "auth.login_ex" && call != "system.info" {
			t.Fatalf("a rejected name must reach no middleware call, saw %q", call)
		}
	}
}

func TestCreateVolumeRejectsMismatchedPool(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)
	p := params()
	p["pool"] = "OtherPool"
	_, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-1", Parameters: p,
		VolumeCapabilities: testCaps(), CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a storage class must not redirect the driver to another pool, got %v", err)
	}
}

func TestValidateVolumeCapabilitiesRejectsRWXOnISCSI(t *testing.T) {
	shared = newCounting()
	c, s := ctlWith(t)
	// The call now confirms the volume exists before judging its capabilities,
	// so the fake must answer for it.
	s.HandleValue("pool.dataset.query", []any{map[string]any{
		"id": "Pool0/k8s/pvc-1", "type": "VOLUME",
		"volsize": map[string]any{"parsed": 1 << 30},
	}})
	resp, err := c.ValidateVolumeCapabilities(context.Background(), &csipb.ValidateVolumeCapabilitiesRequest{
		VolumeId: "nas1/iscsi/Pool0/k8s/pvc-1",
		VolumeCapabilities: []*csipb.VolumeCapability{{
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetConfirmed() != nil {
		t.Fatal("iscsi must not confirm MULTI_NODE_MULTI_WRITER — a zvol is one block device")
	}
}

// TestTopologyExcludesIncapableNode proves the two halves of topology line up:
// what CreateVolume demands of a node must be spelled exactly as the node
// advertises it, or the requirement matches nothing and the scheduler places
// pods on nodes that cannot mount the volume.
func TestTopologyExcludesIncapableNode(t *testing.T) {
	// A node with ext4 and NFS but no xfs and no multipath tooling.
	root := t.TempDir()
	for _, dir := range []string{"sbin", "proc"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, bin := range []string{"mount.nfs", "mkfs.ext4", "iscsiadm", "iscsid"} {
		if err := os.WriteFile(filepath.Join(root, "sbin", bin), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "modules"),
		[]byte("iscsi_tcp 1 0 - Live 0x0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pf, err := node.Detect(context.Background(), root, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	published := pf.TopologyLabels()
	// The same node also publishes, per backend, whether it can reach it. This
	// node can reach nas1.
	published[node.BackendTopologyKey("nas1")] = "true"

	for _, tc := range []struct {
		name        string
		params      map[string]string
		schedulable bool
	}{
		{"nfs on a capable node", map[string]string{"protocol": "nfs"}, true},
		{"xfs on a node without xfsprogs",
			map[string]string{"protocol": "iscsi", "fsType": "xfs"}, false},
		{"multipath on a node without multipath-tools",
			map[string]string{"protocol": "iscsi", "multipath": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			required := requiredTopology("nas1", tc.params["protocol"], tc.params)
			if len(required) != 1 {
				t.Fatalf("want exactly one topology requirement, got %d", len(required))
			}
			matches := true
			for k, v := range required[0].GetSegments() {
				got, present := published[k]
				if !present {
					t.Fatalf("CreateVolume requires %q, which the node never publishes — "+
						"the requirement can never be satisfied by any node", k)
				}
				if got != v {
					matches = false
				}
			}
			if matches != tc.schedulable {
				t.Fatalf("node schedulable=%v, want %v (required %v, node publishes %v)",
					matches, tc.schedulable, required[0].GetSegments(), published)
			}
		})
	}
}
