package csi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type controller struct {
	csipb.UnimplementedControllerServer
	reg   *backend.Registry
	cfg   *config.Config
	locks *VolumeLocks
	nodes backend.NodeResolver
}

// NewController builds the Controller service.
func NewController(reg *backend.Registry, cfg *config.Config) csipb.ControllerServer {
	csiRegistryBackend = func(name string) (backendPaths, error) {
		b, err := reg.Backend(name)
		if err != nil {
			return backendPaths{}, err
		}
		return backendPaths{pool: b.Pool, parent: b.ParentDataset}, nil
	}
	return &controller{reg: reg, cfg: cfg, locks: NewVolumeLocks(),
		nodes: backend.NewNodeResolver(cfg.NodeID)}
}

func (c *controller) ControllerGetCapabilities(context.Context, *csipb.ControllerGetCapabilitiesRequest) (*csipb.ControllerGetCapabilitiesResponse, error) {
	rpc := func(t csipb.ControllerServiceCapability_RPC_Type) *csipb.ControllerServiceCapability {
		return &csipb.ControllerServiceCapability{Type: &csipb.ControllerServiceCapability_Rpc{
			Rpc: &csipb.ControllerServiceCapability_RPC{Type: t}}}
	}
	return &csipb.ControllerGetCapabilitiesResponse{Capabilities: []*csipb.ControllerServiceCapability{
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_VOLUMES),
		rpc(csipb.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CLONE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_GET_CAPACITY),
		// PUBLISH_UNPUBLISH_VOLUME is what gives this driver a fence at all:
		// without it the CO never calls ControllerUnpublishVolume, and nothing
		// ever revokes a node's appliance-side access to a volume.
		rpc(csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
	}}, nil
}

// resolveID builds the volume id from the request and the backend's configured
// pool and parent dataset.
//
// A StorageClass MAY restate pool and parentDataset, but they are operator
// policy: a mismatch is rejected rather than honoured, so a user-writable
// StorageClass cannot point the driver at another part of the pool.
func (c *controller) resolveID(name string, params map[string]string) (volume.ID, error) {
	backendName := params["backend"]
	if backendName == "" {
		if len(c.cfg.Backends) != 1 {
			return volume.ID{}, status.Error(codes.InvalidArgument,
				"storage class must set the 'backend' parameter when several backends are configured")
		}
		for n := range c.cfg.Backends {
			backendName = n
		}
	}
	b, ok := c.cfg.Backends[backendName]
	if !ok {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"%v: %q", backend.ErrUnknownBackend, backendName)
	}
	if p := params["pool"]; p != "" && p != b.Pool {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"storage class pool %q does not match backend %q pool %q", p, backendName, b.Pool)
	}
	if p := params["parentDataset"]; p != "" && p != b.ParentDataset {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"storage class parentDataset %q does not match backend %q parentDataset %q",
			p, backendName, b.ParentDataset)
	}
	proto := params["protocol"]
	if proto == "" {
		return volume.ID{}, status.Error(codes.InvalidArgument,
			"storage class must set the 'protocol' parameter (nfs or iscsi)")
	}
	id := volume.ID{Backend: backendName, Protocol: proto, Pool: b.Pool, Parent: b.ParentDataset, Name: name}
	if err := volume.Confine(id, b.Pool, b.ParentDataset); err != nil {
		return volume.ID{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return id, nil
}

func (c *controller) CreateVolume(ctx context.Context, req *csipb.CreateVolumeRequest) (resp *csipb.CreateVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("CreateVolume", err, time.Since(start)) }()

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	id, err := c.resolveID(req.GetName(), req.GetParameters())
	if err != nil {
		return nil, err
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	b, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}

	size := req.GetCapacityRange().GetRequiredBytes()
	if size <= 0 {
		size = 1 << 30 // 1 GiB default, as CSI permits when no range is given
	}
	if err := c.requireRoomOutsideReserve(ctx, id.Backend, size); err != nil {
		return nil, err
	}
	cr := backend.CreateRequest{ID: id, CapacityBytes: size, Params: req.GetParameters()}
	if src := req.GetVolumeContentSource(); src != nil {
		if s := src.GetSnapshot(); s != nil {
			if err := c.requireSnapshotExists(ctx, s.GetSnapshotId()); err != nil {
				return nil, err
			}
			srcBackend, zfsID, sErr := backend.SnapshotSource(s.GetSnapshotId())
			if sErr != nil {
				return nil, status.Errorf(codes.NotFound, "snapshot %q not found", s.GetSnapshotId())
			}
			if srcBackend != id.Backend {
				return nil, status.Errorf(codes.InvalidArgument,
					"snapshot %q lives on backend %q but the volume would be created on %q",
					s.GetSnapshotId(), srcBackend, id.Backend)
			}
			// The backend talks to ZFS, which knows nothing of the backend
			// prefix the CSI id carries.
			cr.SourceSnapshot = zfsID
		} else if v := src.GetVolume(); v != nil {
			// Cloning a volume is a snapshot plus a clone. The intermediate
			// snapshot is named after the new volume so a retry finds it again
			// rather than making a second one.
			srcID, perr := volume.ParseID(v.GetVolumeId())
			if perr != nil {
				return nil, status.Errorf(codes.NotFound, "source volume %q not found", v.GetVolumeId())
			}
			if err := c.requireVolumeExists(ctx, srcID); err != nil {
				return nil, err
			}
			snap, serr := c.reg.CreateSnapshot(ctx, srcID, "csi-clone-"+sanitiseSnapshotName(req.GetName()))
			if serr != nil {
				return nil, toStatus(serr)
			}
			_, zfsID, sErr := backend.SnapshotSource(snap.ID)
			if sErr != nil {
				return nil, toStatus(sErr)
			}
			cr.SourceSnapshot = zfsID
		}
	}

	vol, err := b.Create(ctx, cr)
	if err != nil {
		return nil, toStatus(err)
	}
	obs.Logger(ctx).Info("volume created", "capacity", vol.CapacityBytes, "protocol", id.Protocol)

	// Everything the node can be told before an attach travels in the volume
	// context, so a PersistentVolume carries the server, share and identity of
	// its volume without a round trip. What is decided per attachment — the
	// iSCSI LUN, above all — is deliberately NOT here: it does not exist yet,
	// and ControllerPublishVolume returns it. PublishContext is read-only, so a
	// volume it cannot describe yet is skipped rather than provisioned open.
	vctx := map[string]string{}
	for k, v := range vol.Context {
		vctx[k] = v
	}
	if pc, pcErr := b.PublishContext(ctx, vol.ID); pcErr == nil {
		for k, v := range pc {
			vctx[k] = v
		}
	}
	out := &csipb.Volume{
		VolumeId:           vol.ID.String(),
		CapacityBytes:      vol.CapacityBytes,
		VolumeContext:      vctx,
		AccessibleTopology: requiredTopology(id.Backend, id.Protocol, req.GetParameters()),
	}
	if cr.SourceSnapshot != "" {
		out.ContentSource = &csipb.VolumeContentSource{Type: &csipb.VolumeContentSource_Snapshot{
			Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: cr.SourceSnapshot}}}
	}
	return &csipb.CreateVolumeResponse{Volume: out}, nil
}

func (c *controller) DeleteVolume(ctx context.Context, req *csipb.DeleteVolumeRequest) (resp *csipb.DeleteVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("DeleteVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		// An unparseable id cannot name anything we created, so there is
		// nothing to delete. CSI requires success here.
		obs.Logger(ctx).Warn("ignoring delete for unparseable volume id", "error", err)
		return &csipb.DeleteVolumeResponse{}, nil
	}
	ctx = obs.WithVolume(ctx, id.String())

	b, ok := c.cfg.Backends[id.Backend]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "%v: %q", backend.ErrUnknownBackend, id.Backend)
	}
	if err := volume.Confine(id, b.Pool, b.ParentDataset); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := be.Delete(ctx, id); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.DeleteVolumeResponse{}, nil
}

func (c *controller) ControllerExpandVolume(ctx context.Context, req *csipb.ControllerExpandVolumeRequest) (resp *csipb.ControllerExpandVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerExpandVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	ctx = obs.WithVolume(ctx, id.String())
	want := req.GetCapacityRange().GetRequiredBytes()
	if want <= 0 {
		return nil, status.Error(codes.InvalidArgument, "required_bytes must be positive")
	}

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	got, err := be.Expand(ctx, id, want)
	if err != nil {
		return nil, toStatus(err)
	}
	// Filesystem volumes need a node-side grow; raw block volumes do not.
	needsNode := req.GetVolumeCapability().GetBlock() == nil
	return &csipb.ControllerExpandVolumeResponse{CapacityBytes: got, NodeExpansionRequired: needsNode}, nil
}

// ControllerPublishVolume grants one node appliance-side access to a volume.
//
// The grant is recorded in the volume's own publish ledger as well as made on
// the appliance, because the appliance cannot answer "which node holds this".
// A shared iSCSI target has no per-node LUN, and an export's host list does not
// say which entry belongs to whom — so the single-writer rule below, and the
// revoke that has to work after the Node object is deleted, both read from the
// ledger rather than from appliance state.
func (c *controller) ControllerPublishVolume(ctx context.Context, req *csipb.ControllerPublishVolumeRequest) (resp *csipb.ControllerPublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerPublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node id is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	mode := req.GetVolumeCapability().GetAccessMode().GetMode()
	if !supportsAccessMode(id.Protocol, mode) {
		return nil, status.Errorf(codes.InvalidArgument,
			"protocol %s does not support access mode %s", id.Protocol, mode)
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	if err := c.requireVolumeExists(ctx, id); err != nil {
		return nil, err
	}
	node, err := c.nodes.Resolve(ctx, req.GetNodeId())
	if err != nil {
		if errors.Is(err, backend.ErrNodeNotFound) {
			return nil, status.Errorf(codes.NotFound, "node %q does not exist", req.GetNodeId())
		}
		return nil, status.Errorf(codes.Internal, "resolving node %q: %v", req.GetNodeId(), obs.Redact(err.Error()))
	}

	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	grants, err := backend.ReadGrants(ctx, cl, id)
	if err != nil {
		return nil, toStatus(err)
	}
	if others := grants.Others(node.ID); len(others) > 0 && singleNode(mode) {
		// The whole point of the fence: a SINGLE_NODE volume must not be
		// reachable from two nodes at once, and the second publisher is the one
		// that has to be refused. FailedPrecondition rather than AlreadyExists
		// is what tells the CO to keep retrying until the first node's
		// ControllerUnpublishVolume lands.
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is published to %v with access mode %s and cannot also be published to %s",
			id, others, mode, node.ID)
	}

	pub, err := c.publisher(ctx, id)
	if err != nil {
		return nil, err
	}
	pc, err := pub.Publish(ctx, id, node)
	if err != nil {
		return nil, toStatus(err)
	}

	// Recorded only after the grant succeeded: a ledger entry for access that
	// was never made would make the next single-writer check refuse a healthy
	// publish.
	grants[node.ID] = node.Addrs
	if err := backend.SaveGrants(ctx, cl, id, grants); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerPublishVolumeResponse{PublishContext: pc}, nil
}

// ControllerUnpublishVolume revokes a node's appliance-side access — the fence.
//
// It is deliberately forgiving about everything except the revoke itself: an
// unparseable id, a deleted volume and a node that was never published are all
// success, because CSI requires the CO to be able to retire an attachment whose
// backing objects have already gone. What it will NOT do is report success
// while the appliance still serves the data.
func (c *controller) ControllerUnpublishVolume(ctx context.Context, req *csipb.ControllerUnpublishVolumeRequest) (resp *csipb.ControllerUnpublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerUnpublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		// An unparseable id cannot name anything this driver published.
		obs.Logger(ctx).Warn("ignoring unpublish for unparseable volume id", "error", err)
		return &csipb.ControllerUnpublishVolumeResponse{}, nil
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	grants, err := backend.ReadGrants(ctx, cl, id)
	if err != nil {
		if errors.Is(err, backend.ErrVolumeGone) {
			return &csipb.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, toStatus(err)
	}

	// An empty node id means "from every node", which is what the CSI spec says
	// and also what a fencing controller wants when it no longer trusts any of
	// them.
	targets := []string{req.GetNodeId()}
	if req.GetNodeId() == "" {
		targets = grants.Nodes()
	}
	if len(targets) == 0 {
		return &csipb.ControllerUnpublishVolumeResponse{}, nil
	}

	pub, err := c.publisher(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range targets {
		// The ledger, not the Node object, is what the revoke is written from.
		// Fencing a node most often happens because that node is gone, and a
		// resolve that fails then would leave its addresses on the share
		// forever — the exact hole this call exists to close.
		node := backend.NodeRef{ID: nodeID, Addrs: grants[nodeID]}
		if live, rErr := c.nodes.Resolve(ctx, nodeID); rErr == nil {
			node.IQN, node.NQN = live.IQN, live.NQN
			node.Addrs = union(node.Addrs, live.Addrs)
		}
		if err := pub.Unpublish(ctx, id, node); err != nil {
			return nil, toStatus(err)
		}
		delete(grants, nodeID)
	}
	if err := backend.SaveGrants(ctx, cl, id, grants); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerUnpublishVolumeResponse{}, nil
}

// publisher returns the backend for a volume as a Publisher.
func (c *controller) publisher(ctx context.Context, id volume.ID) (backend.Publisher, error) {
	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	pub, ok := be.(backend.Publisher)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"protocol %s cannot grant or revoke node access", id.Protocol)
	}
	return pub, nil
}

// singleNode reports whether an access mode admits at most one node.
func singleNode(m csipb.VolumeCapability_AccessMode_Mode) bool {
	switch m {
	case csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
		return true
	}
	return false
}

// union merges two address lists without duplicates, in a stable order.
func union(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		if v != "" {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csipb.ValidateVolumeCapabilitiesRequest) (*csipb.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	if err := c.requireVolumeExists(ctx, id); err != nil {
		return nil, err
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !supportsAccessMode(id.Protocol, cap.GetAccessMode().GetMode()) {
			return &csipb.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf("protocol %s does not support access mode %s",
					id.Protocol, cap.GetAccessMode().GetMode()),
			}, nil
		}
	}
	return &csipb.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csipb.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities()},
	}, nil
}

// supportsAccessMode reflects the storage reality: a zvol behind iSCSI is a
// single block device and cannot be safely shared, while an NFS export can.
func supportsAccessMode(protocol string, m csipb.VolumeCapability_AccessMode_Mode) bool {
	switch protocol {
	case "nfs":
		return true
	default:
		switch m {
		case csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
			csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
			csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:
			return true
		}
		return false
	}
}

func (c *controller) CreateSnapshot(ctx context.Context, req *csipb.CreateSnapshotRequest) (resp *csipb.CreateSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("CreateSnapshot", err, time.Since(start)) }()

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source volume id is required")
	}
	id, err := volume.ParseID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown source volume %q", req.GetSourceVolumeId())
	}
	name := sanitiseSnapshotName(req.GetName())
	if err := c.rejectSnapshotNameReuse(ctx, id, name); err != nil {
		return nil, err
	}
	s, err := c.reg.CreateSnapshot(ctx, id, name)
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.CreateSnapshotResponse{Snapshot: &csipb.Snapshot{
		SnapshotId: s.ID, SourceVolumeId: req.GetSourceVolumeId(),
		CreationTime: timestamppb.New(s.CreationTime), ReadyToUse: s.ReadyToUse,
	}}, nil
}

func (c *controller) DeleteSnapshot(ctx context.Context, req *csipb.DeleteSnapshotRequest) (resp *csipb.DeleteSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("DeleteSnapshot", err, time.Since(start)) }()

	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	// An unparseable id cannot name a snapshot this driver created, so there is
	// nothing to delete. CSI requires success, exactly as for DeleteVolume.
	if _, _, err := backend.SnapshotSource(req.GetSnapshotId()); err != nil {
		obs.Logger(ctx).Warn("ignoring delete for unparseable snapshot id",
			"snapshot_id", req.GetSnapshotId())
		return &csipb.DeleteSnapshotResponse{}, nil
	}
	if err := c.reg.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.DeleteSnapshotResponse{}, nil
}

// sanitiseSnapshotName keeps a CSI snapshot name usable as a ZFS snapshot name.
func sanitiseSnapshotName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.', r == ':':
			return r
		}
		return '-'
	}, n)
	if len(n) > 200 {
		n = n[:200]
	}
	return n
}

// toStatus preserves an already-typed gRPC status and otherwise maps unknown
// failures to Internal. Middleware errnames are deliberately not consulted:
// they report EINVAL for conditions whose real errno is something else.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		if status.Code(err) != codes.Unknown {
			return err
		}
	}
	return status.Error(codes.Internal, obs.Redact(err.Error()))
}

// requireVolumeExists returns NotFound when the dataset behind a volume id is
// absent. Several CSI calls must distinguish "you named nothing" from a real
// failure, and only a query can tell them apart — the middleware's errname
// cannot.
func (c *controller) requireVolumeExists(ctx context.Context, id volume.ID) error {
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return toStatus(err)
	}
	ds, err := cl.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return toStatus(err)
	}
	if ds == nil {
		return status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	return nil
}

// requireSnapshotExists returns NotFound for a snapshot that is not there.
func (c *controller) requireSnapshotExists(ctx context.Context, snapshotID string) error {
	backendName, zfsID, err := backend.SnapshotSource(snapshotID)
	if err != nil {
		return status.Errorf(codes.NotFound, "snapshot %q does not exist", snapshotID)
	}
	cl, err := c.reg.Client(ctx, backendName)
	if err != nil {
		return toStatus(err)
	}
	snap, err := cl.SnapshotQuery(ctx, zfsID)
	if err != nil {
		return toStatus(err)
	}
	if snap == nil {
		return status.Errorf(codes.NotFound, "snapshot %q does not exist", snapshotID)
	}
	return nil
}

// rejectSnapshotNameReuse enforces the CSI rule that one snapshot name may not
// refer to two different source volumes.
func (c *controller) rejectSnapshotNameReuse(ctx context.Context, source volume.ID, name string) error {
	cl, err := c.reg.Client(ctx, source.Backend)
	if err != nil {
		return toStatus(err)
	}
	b, err := c.reg.Backend(source.Backend)
	if err != nil {
		return toStatus(err)
	}
	snaps, err := cl.SnapshotList(ctx, b.Pool+"/"+b.ParentDataset)
	if err != nil {
		return toStatus(err)
	}
	want := source.DatasetPath()
	for _, s := range snaps {
		if snapshotNameOf(s.ID) != name {
			continue
		}
		if s.Dataset != want {
			return status.Errorf(codes.AlreadyExists,
				"snapshot name %q already exists for source volume %s", name, s.Dataset)
		}
	}
	return nil
}

// snapshotNameOf returns the part after the @ in a ZFS snapshot id.
func snapshotNameOf(id string) string {
	if i := strings.LastIndex(id, "@"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// requiredTopology names the node capabilities a volume needs, so the scheduler
// will not place a pod on a node that cannot attach or mount it.
//
// Publishing capability labels from the node is only half of topology: without
// the matching requirement here, every node looks equally able and the pod is
// scheduled somewhere that then fails to mount.
func requiredTopology(backendName, protocol string, params map[string]string) []*csipb.Topology {
	segments := map[string]string{
		node.TopologyKey(node.Capability(protocol)): "true",
	}
	// Tooling is not enough: a node can have every binary this volume needs and
	// no route to the appliance it lives on. Requiring the backend's own
	// reachability label keeps the scheduler off those nodes.
	if backendName != "" {
		segments[node.BackendTopologyKey(backendName)] = "true"
	}
	if fs := params["fsType"]; fs != "" && fs != "ext4" {
		segments[node.TopologyKey(node.Capability(fs))] = "true"
	} else if protocol == "iscsi" || protocol == "nvme" {
		segments[node.TopologyKey(node.CapExt4)] = "true"
	}
	if params["multipath"] == "true" {
		segments[node.TopologyKey(node.CapMultipath)] = "true"
	}
	return []*csipb.Topology{{Segments: segments}}
}
