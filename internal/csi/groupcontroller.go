package csi

import (
	"context"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// groupController implements the CSI GroupController service: crash-consistent
// snapshots of several volumes at once.
//
// The service exists because snapshotting a database's data, WAL and log PVCs
// one at a time can capture each at a different instant, and a restore from
// such a set can land in a state that never existed on disk. ZFS can do better
// — but only for datasets beneath a common parent, which is why this service
// refuses a group it cannot capture atomically instead of silently degrading
// to a loop of single-volume snapshots.
type groupController struct {
	csipb.UnimplementedGroupControllerServer
	reg   *backend.Registry
	cfg   *config.Config
	locks *VolumeLocks
}

// NewGroupController builds the GroupController service.
func NewGroupController(reg *backend.Registry, cfg *config.Config) csipb.GroupControllerServer {
	return &groupController{reg: reg, cfg: cfg, locks: NewVolumeLocks()}
}

func (g *groupController) GroupControllerGetCapabilities(context.Context, *csipb.GroupControllerGetCapabilitiesRequest) (*csipb.GroupControllerGetCapabilitiesResponse, error) {
	return &csipb.GroupControllerGetCapabilitiesResponse{
		Capabilities: []*csipb.GroupControllerServiceCapability{{
			Type: &csipb.GroupControllerServiceCapability_Rpc{
				Rpc: &csipb.GroupControllerServiceCapability_RPC{
					Type: csipb.GroupControllerServiceCapability_RPC_CREATE_DELETE_GET_VOLUME_GROUP_SNAPSHOT}}},
		}}, nil
}

func (g *groupController) CreateVolumeGroupSnapshot(ctx context.Context, req *csipb.CreateVolumeGroupSnapshotRequest) (resp *csipb.CreateVolumeGroupSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("CreateVolumeGroupSnapshot", err, time.Since(start)) }()

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
	}
	if len(req.GetSourceVolumeIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "source volume ids are required")
	}
	sources := make([]volume.ID, 0, len(req.GetSourceVolumeIds()))
	for _, raw := range req.GetSourceVolumeIds() {
		id, perr := volume.ParseID(raw)
		if perr != nil {
			return nil, status.Errorf(codes.NotFound, "unknown source volume %q", raw)
		}
		sources = append(sources, id)
	}
	name := sanitiseSnapshotName(req.GetName())

	release, ok := g.locks.TryAcquire(name)
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for group snapshot %s", name)
	}
	defer release()

	// The group's shape is judged before anything is created, so a group that
	// cannot be crash-consistent leaves no half-made snapshots behind.
	if err := g.reg.CheckGroupSources(sources); err != nil {
		return nil, toStatus(err)
	}
	for _, id := range sources {
		if err := g.requireVolumeExists(ctx, id); err != nil {
			return nil, err
		}
	}

	gs, err := g.reg.CreateGroupSnapshot(ctx, sources, name)
	if err != nil {
		return nil, toStatus(err)
	}
	obs.Logger(ctx).Info("volume group snapshot created",
		"group_snapshot_id", gs.ID, "members", len(gs.Members))

	out := &csipb.VolumeGroupSnapshot{
		GroupSnapshotId: gs.ID,
		CreationTime:    timestamppb.New(gs.CreationTime),
		ReadyToUse:      gs.ReadyToUse,
	}
	for _, m := range gs.Members {
		out.Snapshots = append(out.Snapshots, &csipb.Snapshot{
			SnapshotId: m.ID, SourceVolumeId: m.SourceVolumeID,
			CreationTime: timestamppb.New(m.CreationTime), ReadyToUse: m.ReadyToUse,
			GroupSnapshotId: gs.ID,
		})
	}
	return &csipb.CreateVolumeGroupSnapshotResponse{GroupSnapshot: out}, nil
}

func (g *groupController) DeleteVolumeGroupSnapshot(ctx context.Context, req *csipb.DeleteVolumeGroupSnapshotRequest) (resp *csipb.DeleteVolumeGroupSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("DeleteVolumeGroupSnapshot", err, time.Since(start)) }()

	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	// An unparseable id cannot name a group this driver created, so there is
	// nothing to delete. CSI requires success, exactly as for DeleteSnapshot.
	if _, _, perr := backend.SnapshotSource(req.GetGroupSnapshotId()); perr != nil {
		obs.Logger(ctx).Warn("ignoring delete for unparseable group snapshot id",
			"group_snapshot_id", req.GetGroupSnapshotId())
		return &csipb.DeleteVolumeGroupSnapshotResponse{}, nil
	}

	release, ok := g.locks.TryAcquire(req.GetGroupSnapshotId())
	if !ok {
		return nil, status.Errorf(codes.Aborted,
			"another operation is in progress for group snapshot %s", req.GetGroupSnapshotId())
	}
	defer release()

	if err := g.reg.DeleteGroupSnapshot(ctx, req.GetGroupSnapshotId(), req.GetSnapshotIds()); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.DeleteVolumeGroupSnapshotResponse{}, nil
}

func (g *groupController) GetVolumeGroupSnapshot(ctx context.Context, req *csipb.GetVolumeGroupSnapshotRequest) (resp *csipb.GetVolumeGroupSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("GetVolumeGroupSnapshot", err, time.Since(start)) }()

	if req.GetGroupSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "group snapshot id is required")
	}
	gs, err := g.reg.GetGroupSnapshot(ctx, req.GetGroupSnapshotId(), req.GetSnapshotIds())
	if err != nil {
		return nil, toStatus(err)
	}
	backendName, _, _ := backend.SnapshotSource(gs.ID)
	out := &csipb.VolumeGroupSnapshot{
		GroupSnapshotId: gs.ID,
		CreationTime:    timestamppb.New(gs.CreationTime),
		ReadyToUse:      gs.ReadyToUse,
	}
	for _, m := range gs.Members {
		// The members come back carrying their ZFS source dataset; snapshotPB
		// maps that back to the volume handle the CO knows.
		s := snapshotPB(m.ID, m.SourceVolumeID, backendName)
		s.GroupSnapshotId = gs.ID
		out.Snapshots = append(out.Snapshots, s)
	}
	return &csipb.GetVolumeGroupSnapshotResponse{GroupSnapshot: out}, nil
}

// requireVolumeExists returns NotFound when a member volume is not on the
// appliance, so the CO learns which PVC it named wrongly rather than seeing a
// generic failure.
func (g *groupController) requireVolumeExists(ctx context.Context, id volume.ID) error {
	cl, err := g.reg.Client(ctx, id.Backend)
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
