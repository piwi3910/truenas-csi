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
	// minCapacity stands in for a backend whose appliance refuses volumes
	// below a floor, as TrueNAS does for a filesystem's refquota.
	minCapacity int64
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
	t.Cleanup(func() { _ = r.Close() })
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

// TestCreateVolumeEchoesClaimName pins the claim name into the volume context.
//
// Kubernetes hands the claim name to CreateVolume and NOT to
// NodePublishVolume, so the node's per-volume I/O metrics can only label a
// series with its PVC if the controller echoes it here. Losing this leaves the
// `pvc` label silently empty -- the metrics still work, so nothing fails, and
// every dashboard has to join through kube-state-metrics instead.
func TestCreateVolumeEchoesClaimName(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)
	p := params()
	p[volume.ParamPVCName] = "my-claim"
	p[volume.ParamPVCNamespace] = "team-a"
	resp, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-claimname", Parameters: p,
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeCapabilities: testCaps(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetVolumeContext()[node.KeyPVCName]; got != "my-claim" {
		t.Errorf("volume context %q = %q, want the claim name %q",
			node.KeyPVCName, got, "my-claim")
	}
}

// TestCreateVolumeEchoesNodeParameters fails if a StorageClass parameter the
// NODE reads stops reaching the volume context.
//
// Nothing else carries these: a backend fills the context with its own protocol
// keys, and StorageClass parameters never otherwise leave the controller. When
// the copy is missing the I/O limits still work on a STATIC PersistentVolume,
// whose volumeAttributes an operator writes by hand, and silently do nothing on
// a dynamically provisioned one -- the class is accepted, provisioning
// succeeds, and no limit is ever applied.
func TestCreateVolumeEchoesNodeParameters(t *testing.T) {
	for _, key := range node.NodeParameterKeys {
		t.Run(key, func(t *testing.T) {
			shared = newCounting()
			c, _ := ctlWith(t)
			p := params()
			p[key] = "42"
			resp, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
				Name: "pvc-echo", Parameters: p,
				CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
				VolumeCapabilities: testCaps(),
			})
			if err != nil {
				t.Fatalf("CreateVolume: %v", err)
			}
			got := resp.GetVolume().GetVolumeContext()[key]
			if got != "42" {
				t.Errorf("volume context is missing %q (got %q).\n"+
					"The node reads this parameter and nothing else carries it, so the "+
					"limit would be accepted and never applied on a dynamically "+
					"provisioned volume. Context: %v", key, got, resp.GetVolume().GetVolumeContext())
			}
		})
	}
}

func (b *countingBackend) MinimumCapacityBytes() int64 { return b.minCapacity }

// TestCreateVolumeRoundsUpToTheBackendMinimum pins the fix for a claim smaller
// than the appliance can create.
//
// TrueNAS refuses a refquota below 1 GiB, so on a live cluster every
// filesystem-backed PVC under that size stayed Pending for ever while the
// events showed a Pydantic union error naming neither the limit nor the field
// the driver had set. CSI permits provisioning MORE than required_bytes, so the
// request is rounded up and the reported capacity is what the appliance really
// applied — a PV that claims 512Mi when the dataset holds a 1Gi refquota would
// be a lie the resizer and the scheduler would both act on.
func TestCreateVolumeRoundsUpToTheBackendMinimum(t *testing.T) {
	shared = newCounting()
	shared.minCapacity = 1 << 30
	c, _ := ctlWith(t)

	resp, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-small", Parameters: params(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 512 << 20},
		VolumeCapabilities: testCaps(),
	})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != 1<<30 {
		t.Errorf("capacity = %d, want the backend minimum %d", got, int64(1)<<30)
	}
}

// TestCreateVolumeRefusesWhenTheLimitIsBelowTheMinimum covers the one case
// where rounding up is not allowed: limit_bytes is a ceiling the CO set on
// purpose, and exceeding it silently would make the PersistentVolume claim a
// size the user forbade.
func TestCreateVolumeRefusesWhenTheLimitIsBelowTheMinimum(t *testing.T) {
	shared = newCounting()
	shared.minCapacity = 1 << 30
	c, _ := ctlWith(t)

	_, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-capped", Parameters: params(),
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: 256 << 20, LimitBytes: 512 << 20,
		},
		VolumeCapabilities: testCaps(),
	})
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("code = %s, want OutOfRange (got %v)", status.Code(err), err)
	}
}

// TestExpandRequiresNodeActionForBlockDevices pins which volumes need a
// node-side step after the controller grows them.
//
// The old rule was "is this a filesystem volume?", on the reasoning that a raw
// block volume has no filesystem to resize. True, and beside the point: an
// iSCSI or NVMe initiator caches the device size, so a block volume that is
// never rescanned keeps presenting the old one. Measured on hardware -- a Block
// PVC grown 1Gi to 3Gi reported 3Gi to Kubernetes while the pod's device stayed
// at 1073741824 bytes, which is the size the application actually gets.
func TestExpandRequiresNodeActionForBlockDevices(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		want     bool
		why      string
	}{
		{"iscsi", true, "the initiator caches the device size until it is rescanned"},
		{"nvme", true, "the namespace size is cached until the controller is rescanned"},
		{"nfs", false, "the size a pod sees is the dataset refquota, changed on the appliance"},
		{"smb", false, "same as nfs"},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			if got := nodeExpansionRequired(tc.protocol); got != tc.want {
				t.Errorf("nodeExpansionRequired(%q) = %v, want %v: %s",
					tc.protocol, got, tc.want, tc.why)
			}
		})
	}
}

// TestExpandOfABlockVolumeStillAsksTheNode drives the whole RPC for a raw block
// volume, so the wiring between the capability and the answer cannot regress
// without a test noticing.
func TestExpandOfABlockVolumeStillAsksTheNode(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)
	resp, err := c.ControllerExpandVolume(context.Background(),
		&csipb.ControllerExpandVolumeRequest{
			VolumeId:      "nas1/counting/Pool0/k8s/pvc-expand",
			CapacityRange: &csipb.CapacityRange{RequiredBytes: 3 << 30},
			VolumeCapability: &csipb.VolumeCapability{
				AccessType: &csipb.VolumeCapability_Block{
					Block: &csipb.VolumeCapability_BlockVolume{}},
				AccessMode: &csipb.VolumeCapability_AccessMode{
					Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
			},
		})
	if err != nil {
		t.Fatalf("ControllerExpandVolume: %v", err)
	}
	if !resp.GetNodeExpansionRequired() {
		t.Error("a raw block volume was expanded without asking the node to rescan " +
			"the device, so the pod keeps seeing the old size")
	}
}
