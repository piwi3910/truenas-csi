package csi

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
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

// fencingBackend records the grants and revokes the controller asks for, so a
// test can assert that the fence actually reached a backend rather than that
// the RPC merely returned success.
type fencingBackend struct {
	mu        sync.Mutex
	published map[string][]string // volume id -> node ids currently granted
	revoked   []string            // node ids, in the order they were revoked
}

func newFencing() *fencingBackend {
	return &fencingBackend{published: map[string][]string{}}
}

func (b *fencingBackend) Protocol() string { return "fencing" }

func (b *fencingBackend) Create(_ context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	return &backend.Volume{ID: r.ID, CapacityBytes: r.CapacityBytes}, nil
}
func (b *fencingBackend) Delete(context.Context, volume.ID) error { return nil }
func (b *fencingBackend) Expand(_ context.Context, _ volume.ID, n int64) (int64, error) {
	return n, nil
}
func (b *fencingBackend) PublishContext(context.Context, volume.ID) (map[string]string, error) {
	return map[string]string{"share": "/mnt/Pool0/k8s/x"}, nil
}

func (b *fencingBackend) Publish(_ context.Context, id volume.ID, node backend.NodeRef) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range b.published[id.String()] {
		if n == node.ID {
			return map[string]string{"share": "/mnt/Pool0/k8s/x", "node": node.ID}, nil
		}
	}
	b.published[id.String()] = append(b.published[id.String()], node.ID)
	return map[string]string{"share": "/mnt/Pool0/k8s/x", "node": node.ID}, nil
}

func (b *fencingBackend) Unpublish(_ context.Context, id volume.ID, node backend.NodeRef) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked = append(b.revoked, node.ID)
	kept := b.published[id.String()][:0]
	for _, n := range b.published[id.String()] {
		if n != node.ID {
			kept = append(kept, n)
		}
	}
	b.published[id.String()] = kept
	return nil
}

func (b *fencingBackend) revokes() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.revoked...)
}

var fencing = newFencing()

func init() {
	f := func(truenas.API, backend.Options) backend.Backend { return fencing }
	backend.Register("fencing", f)
	// Registered under "nfs" as well because supportsAccessMode keys on the
	// protocol name: only nfs admits a MULTI_NODE mode, and the single-writer
	// rule has to be shown NOT to apply there.
	backend.Register("nfs", f)
}

// testResolver stands in for the cluster's Node objects.
type testResolver map[string]backend.NodeRef

func (r testResolver) Resolve(_ context.Context, nodeID string) (backend.NodeRef, error) {
	n, ok := r[nodeID]
	if !ok {
		return backend.NodeRef{}, fmt.Errorf("%w: %s", backend.ErrNodeNotFound, nodeID)
	}
	return n, nil
}

// fencingCtl builds a controller over a fake appliance that stores the volume's
// user properties, which is where the publish ledger lives.
func fencingCtl(t *testing.T) csipb.ControllerServer {
	t.Helper()
	fencing = newFencing()

	s := fake.Start(t, fake.Options{})
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})

	var mu sync.Mutex
	props := map[string]map[string]any{}
	exists := map[string]bool{"Pool0/k8s/pvc-p": true, "Pool0/k8s/pvc-q": true}

	s.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		var filters [][]any
		_ = json.Unmarshal(p[0], &filters)
		id, _ := filters[0][2].(string)
		mu.Lock()
		defer mu.Unlock()
		if !exists[id] {
			return []any{}, nil
		}
		if props[id] == nil {
			props[id] = map[string]any{}
		}
		return []any{map[string]any{"id": id, "type": "FILESYSTEM",
			"mountpoint": "/mnt/" + id, "user_properties": props[id]}}, nil
	})
	s.Handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		var id string
		var patch map[string]any
		_ = json.Unmarshal(p[0], &id)
		_ = json.Unmarshal(p[1], &patch)
		mu.Lock()
		defer mu.Unlock()
		if props[id] == nil {
			props[id] = map[string]any{}
		}
		if raw, ok := patch["user_properties_update"].([]any); ok {
			for _, e := range raw {
				m, _ := e.(map[string]any)
				props[id][fmt.Sprint(m["key"])] = map[string]any{
					"value": m["value"], "source": "LOCAL"}
			}
		}
		return map[string]any{"id": id, "user_properties": props[id]}, nil
	})

	cfg := &config.Config{NodeID: "worker-1", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	r, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	c := NewController(r, cfg)
	c.(*controller).nodes = testResolver{
		"worker-1": {ID: "worker-1", Addrs: []string{"10.0.0.1"}},
		"worker-2": {ID: "worker-2", Addrs: []string{"10.0.0.2"}},
	}
	return c
}

func pubReq(volID, nodeID string, mode csipb.VolumeCapability_AccessMode_Mode) *csipb.ControllerPublishVolumeRequest {
	return &csipb.ControllerPublishVolumeRequest{
		VolumeId: volID, NodeId: nodeID,
		VolumeCapability: &csipb.VolumeCapability{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"}},
			AccessMode: &csipb.VolumeCapability_AccessMode{Mode: mode},
		},
	}
}

const (
	fencedVol  = "nas1/fencing/Pool0/k8s/pvc-p"
	sharedVol  = "nas1/nfs/Pool0/k8s/pvc-q"
	writerMode = csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
)

// TestPublishUnpublishIsAdvertised: the CO only calls
// ControllerUnpublishVolume for a driver that declares the capability, so
// without it the fence is unreachable however well it is implemented.
func TestPublishUnpublishIsAdvertised(t *testing.T) {
	c := fencingCtl(t)
	caps, err := c.ControllerGetCapabilities(context.Background(),
		&csipb.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities: %v", err)
	}
	for _, cap := range caps.GetCapabilities() {
		if cap.GetRpc().GetType() == csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME {
			return
		}
	}
	t.Fatal("PUBLISH_UNPUBLISH_VOLUME must be advertised, or nothing ever calls the fence")
}

// TestPublishArgumentValidation covers the codes csi-sanity checks, in the
// order it checks them: a malformed request is InvalidArgument even when the
// volume and node it names do not exist.
func TestPublishArgumentValidation(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()

	cases := []struct {
		name string
		req  *csipb.ControllerPublishVolumeRequest
		want codes.Code
	}{
		{"no volume id", &csipb.ControllerPublishVolumeRequest{NodeId: "worker-1"}, codes.InvalidArgument},
		{"no node id", &csipb.ControllerPublishVolumeRequest{VolumeId: fencedVol}, codes.InvalidArgument},
		{"no capability", &csipb.ControllerPublishVolumeRequest{
			VolumeId: fencedVol, NodeId: "worker-1"}, codes.InvalidArgument},
		{"unsupported access mode", pubReq(fencedVol, "worker-1",
			csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER), codes.InvalidArgument},
		{"unknown volume", pubReq("nas1/fencing/Pool0/k8s/pvc-ghost", "worker-1", writerMode), codes.NotFound},
		{"malformed volume id", pubReq("not-a-handle", "worker-1", writerMode), codes.NotFound},
		{"unknown node", pubReq(fencedVol, "worker-99", writerMode), codes.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.ControllerPublishVolume(ctx, tc.req)
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
}

// TestPublishIsIdempotentAndRecordsTheGrant: the CO retries freely, and the
// ledger is what a later single-writer check and a later revoke both read.
func TestPublishIsIdempotentAndRecordsTheGrant(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()

	first, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-1", writerMode))
	if err != nil {
		t.Fatalf("ControllerPublishVolume: %v", err)
	}
	if first.GetPublishContext()["share"] == "" {
		t.Fatalf("the node's publish context must be returned, got %v", first.GetPublishContext())
	}
	second, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-1", writerMode))
	if err != nil {
		t.Fatalf("a repeated publish to the same node must succeed, got %v", err)
	}
	if second.GetPublishContext()["node"] != "worker-1" {
		t.Fatalf("publish context = %v", second.GetPublishContext())
	}
}

// TestSingleNodeVolumeRefusesASecondNode is the rule the whole ledger exists
// for: the appliance cannot say which node holds a shared target's LUN, so
// without it a SINGLE_NODE volume would silently be reachable from two nodes.
func TestSingleNodeVolumeRefusesASecondNode(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()

	if _, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-1", writerMode)); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	_, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-2", writerMode))
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition (err %v)", got, err)
	}

	// And it becomes publishable again once the first node is fenced.
	if _, err := c.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: fencedVol, NodeId: "worker-1"}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if _, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-2", writerMode)); err != nil {
		t.Fatalf("after the fence the volume must be publishable elsewhere, got %v", err)
	}
}

// TestMultiNodeVolumeAdmitsSeveralNodes: the single-writer rule follows the
// requested access mode, not the driver's convenience — an RWX NFS volume must
// still reach every node that asks for it.
func TestMultiNodeVolumeAdmitsSeveralNodes(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()
	const multi = csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER

	for _, nodeID := range []string{"worker-1", "worker-2"} {
		if _, err := c.ControllerPublishVolume(ctx, pubReq(sharedVol, nodeID, multi)); err != nil {
			t.Fatalf("publish to %s: %v", nodeID, err)
		}
	}
}

// TestUnpublishSemantics: CSI requires the CO to be able to retire an
// attachment whose backing objects have already gone, so almost everything is
// success — but a missing volume id is still a malformed request.
func TestUnpublishSemantics(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()

	if _, err := c.ControllerUnpublishVolume(ctx,
		&csipb.ControllerUnpublishVolumeRequest{NodeId: "worker-1"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a missing volume id must be InvalidArgument, got %v", err)
	}
	cases := []struct {
		name string
		req  *csipb.ControllerUnpublishVolumeRequest
	}{
		{"never published", &csipb.ControllerUnpublishVolumeRequest{
			VolumeId: fencedVol, NodeId: "worker-1"}},
		{"volume no longer exists", &csipb.ControllerUnpublishVolumeRequest{
			VolumeId: "nas1/fencing/Pool0/k8s/pvc-ghost", NodeId: "worker-1"}},
		{"unparseable volume id", &csipb.ControllerUnpublishVolumeRequest{
			VolumeId: "not-a-handle", NodeId: "worker-1"}},
		{"node this driver never heard of", &csipb.ControllerUnpublishVolumeRequest{
			VolumeId: fencedVol, NodeId: "worker-99"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.ControllerUnpublishVolume(ctx, tc.req); err != nil {
				t.Fatalf("want success, got %v", err)
			}
		})
	}
}

// TestUnpublishFencesANodeThatIsGone is the case the ledger exists for on the
// revoke side. Fencing usually happens BECAUSE the node has been deleted, so a
// revoke that first resolved the Node object would fail exactly when it matters
// and leave the node's access in place forever.
func TestUnpublishFencesANodeThatIsGone(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()

	if _, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-2", writerMode)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// The node object disappears, exactly as it does when a node is deleted.
	c.(*controller).nodes = testResolver{"worker-1": {ID: "worker-1", Addrs: []string{"10.0.0.1"}}}

	if _, err := c.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: fencedVol, NodeId: "worker-2"}); err != nil {
		t.Fatalf("fencing a deleted node must succeed, got %v", err)
	}
	if got := fencing.revokes(); len(got) != 1 || got[0] != "worker-2" {
		t.Fatalf("the backend must have been asked to revoke worker-2, got %v", got)
	}
	// The grant is gone from the ledger too, so the volume is publishable again.
	if _, err := c.ControllerPublishVolume(ctx, pubReq(fencedVol, "worker-1", writerMode)); err != nil {
		t.Fatalf("after the fence the volume must be publishable, got %v", err)
	}
}

// TestUnpublishWithNoNodeFencesEveryNode: the spec says an empty node id means
// "from every node it is published to", which is also what a fencing controller
// wants when it no longer trusts any of them.
func TestUnpublishWithNoNodeFencesEveryNode(t *testing.T) {
	c := fencingCtl(t)
	ctx := context.Background()
	const multi = csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER

	for _, nodeID := range []string{"worker-1", "worker-2"} {
		if _, err := c.ControllerPublishVolume(ctx, pubReq(sharedVol, nodeID, multi)); err != nil {
			t.Fatalf("publish to %s: %v", nodeID, err)
		}
	}
	if _, err := c.ControllerUnpublishVolume(ctx,
		&csipb.ControllerUnpublishVolumeRequest{VolumeId: sharedVol}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if got := fencing.revokes(); len(got) != 2 {
		t.Fatalf("every published node must be revoked, got %v", got)
	}
}

// TestPublisherIsRequiredOfEveryBackend: a backend that cannot revoke would
// publish successfully and then be unfenceable forever. The controller has to
// refuse it at publish time rather than discover it at fencing time, when the
// node it cannot revoke is already the emergency.
func TestPublisherIsRequiredOfEveryBackend(t *testing.T) {
	// "counting" is the harness backend from controller_test.go: a complete
	// backend.Backend that implements no Publisher.
	if _, ok := any(shared).(backend.Publisher); ok {
		t.Skip("the counting backend now implements Publisher; this test needs another one")
	}
	c := fencingCtl(t)
	_, err := c.ControllerPublishVolume(context.Background(),
		pubReq("nas1/counting/Pool0/k8s/pvc-p", "worker-1", writerMode))
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal (err %v)", got, err)
	}
}

func (*fencingBackend) MinimumCapacityBytes() int64 { return 0 }

func (*fencingBackend) AcceptedParameters() []string { return nil }
