package csi

import (
	"context"
	"fmt"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/backend"
	"github.com/pwatteel/truenas-csi/internal/config"
	"github.com/pwatteel/truenas-csi/internal/obs"
	"github.com/pwatteel/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type controller struct {
	csipb.UnimplementedControllerServer
	reg   *backend.Registry
	cfg   *config.Config
	locks *VolumeLocks
}

// NewController builds the Controller service.
func NewController(reg *backend.Registry, cfg *config.Config) csipb.ControllerServer {
	return &controller{reg: reg, cfg: cfg, locks: NewVolumeLocks()}
}

func (c *controller) ControllerGetCapabilities(context.Context, *csipb.ControllerGetCapabilitiesRequest) (*csipb.ControllerGetCapabilitiesResponse, error) {
	rpc := func(t csipb.ControllerServiceCapability_RPC_Type) *csipb.ControllerServiceCapability {
		return &csipb.ControllerServiceCapability{Type: &csipb.ControllerServiceCapability_Rpc{
			Rpc: &csipb.ControllerServiceCapability_RPC{Type: t}}}
	}
	return &csipb.ControllerGetCapabilitiesResponse{Capabilities: []*csipb.ControllerServiceCapability{
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_VOLUMES),
		rpc(csipb.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CLONE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_GET_CAPACITY),
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
	cr := backend.CreateRequest{ID: id, CapacityBytes: size, Params: req.GetParameters()}
	if src := req.GetVolumeContentSource(); src != nil {
		if s := src.GetSnapshot(); s != nil {
			cr.SourceSnapshot = s.GetSnapshotId()
		} else if v := src.GetVolume(); v != nil {
			return nil, status.Error(codes.Unimplemented,
				"cloning directly from a volume is not supported; snapshot it first")
		}
	}

	vol, err := b.Create(ctx, cr)
	if err != nil {
		return nil, toStatus(err)
	}
	obs.Logger(ctx).Info("volume created", "capacity", vol.CapacityBytes, "protocol", id.Protocol)

	out := &csipb.Volume{
		VolumeId:      vol.ID.String(),
		CapacityBytes: vol.CapacityBytes,
		VolumeContext: vol.Context,
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

func (c *controller) ControllerPublishVolume(ctx context.Context, req *csipb.ControllerPublishVolumeRequest) (*csipb.ControllerPublishVolumeResponse, error) {
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	pc, err := be.PublishContext(ctx, id)
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerPublishVolumeResponse{PublishContext: pc}, nil
}

func (c *controller) ControllerUnpublishVolume(context.Context, *csipb.ControllerUnpublishVolumeRequest) (*csipb.ControllerUnpublishVolumeResponse, error) {
	// Detach happens entirely on the node; there is no appliance-side state.
	return &csipb.ControllerUnpublishVolumeResponse{}, nil
}

func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csipb.ValidateVolumeCapabilitiesRequest) (*csipb.ValidateVolumeCapabilitiesResponse, error) {
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
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

	id, err := volume.ParseID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown source volume %q", req.GetSourceVolumeId())
	}
	name := sanitiseSnapshotName(req.GetName())
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
