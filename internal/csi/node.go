package csi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nodeServer struct {
	csipb.UnimplementedNodeServer
	n *node.Node
}

// NewNode adapts the node data path to the CSI gRPC surface.
func NewNode(n *node.Node) csipb.NodeServer { return &nodeServer{n: n} }

// stageRecordPath is where the publish context is remembered for unstage.
//
// NodeUnstageVolume carries no publish context, so without this the node cannot
// know which target to log out of. Guessing from live session state is not an
// option: Longhorn shares this node's iSCSI stack and a wrong guess would tear
// down its sessions.
func stageRecordPath(stagingPath string) string {
	return filepath.Join(filepath.Dir(stagingPath),
		"."+filepath.Base(stagingPath)+".truenas-csi.json")
}

func writeStageRecord(stagingPath string, pc map[string]string) error {
	b, err := json.Marshal(pc)
	if err != nil {
		return err
	}
	return os.WriteFile(stageRecordPath(stagingPath), b, 0o600)
}

func readStageRecord(stagingPath string) map[string]string {
	b, err := os.ReadFile(stageRecordPath(stagingPath))
	if err != nil {
		return nil
	}
	var pc map[string]string
	if json.Unmarshal(b, &pc) != nil {
		return nil
	}
	return pc
}

// mergeCtx combines the volume context with the publish context.
//
// This driver has no ControllerPublishVolume step, so everything the node needs
// travels in the volume context; the publish context is still honoured when a
// CO supplies one, and wins on conflict because it is the fresher value.
func mergeCtx(volumeCtx, publishCtx map[string]string) map[string]string {
	out := make(map[string]string, len(volumeCtx)+len(publishCtx))
	for k, v := range volumeCtx {
		out[k] = v
	}
	for k, v := range publishCtx {
		out[k] = v
	}
	return out
}

func capOf(c *csipb.VolumeCapability) node.VolumeCapability {
	out := node.VolumeCapability{}
	if c == nil {
		return out
	}
	if c.GetBlock() != nil {
		out.Block = true
		return out
	}
	if m := c.GetMount(); m != nil {
		out.FsType = m.GetFsType()
		out.MountFlags = m.GetMountFlags()
	}
	switch c.GetAccessMode().GetMode() {
	case csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
		out.Readonly = true
	}
	return out
}

// nodeErr maps the node package's sentinels onto CSI status codes.
func nodeErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, node.ErrInvalidRequest):
		return status.Error(codes.InvalidArgument, obs.Redact(err.Error()))
	case errors.Is(err, node.ErrCapabilityUnavailable):
		return status.Error(codes.FailedPrecondition, obs.Redact(err.Error()))
	case errors.Is(err, node.ErrVolumePathNotFound):
		return status.Error(codes.NotFound, obs.Redact(err.Error()))
	default:
		return status.Error(codes.Internal, obs.Redact(err.Error()))
	}
}

func (s *nodeServer) NodeGetCapabilities(context.Context, *csipb.NodeGetCapabilitiesRequest) (*csipb.NodeGetCapabilitiesResponse, error) {
	rpc := func(t csipb.NodeServiceCapability_RPC_Type) *csipb.NodeServiceCapability {
		return &csipb.NodeServiceCapability{Type: &csipb.NodeServiceCapability_Rpc{
			Rpc: &csipb.NodeServiceCapability_RPC{Type: t}}}
	}
	return &csipb.NodeGetCapabilitiesResponse{Capabilities: []*csipb.NodeServiceCapability{
		rpc(csipb.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME),
		rpc(csipb.NodeServiceCapability_RPC_EXPAND_VOLUME),
		rpc(csipb.NodeServiceCapability_RPC_GET_VOLUME_STATS),
		// GET_VOLUME_HEALTH is CSI v1.13's successor to the alpha
		// `volume_condition` field that earlier releases carried on
		// NodeGetVolumeStatsResponse: the same "this mounted volume is sick"
		// signal, now its own RPC. Advertising it is what makes the CO ask;
		// without it the driver's health monitor would talk to nobody.
		rpc(csipb.NodeServiceCapability_RPC_GET_VOLUME_HEALTH),
	}}, nil
}

func (s *nodeServer) NodeGetInfo(ctx context.Context, _ *csipb.NodeGetInfoRequest) (*csipb.NodeGetInfoResponse, error) {
	info := s.n.GetInfo(ctx)
	return &csipb.NodeGetInfoResponse{
		NodeId:            info.NodeID,
		MaxVolumesPerNode: info.MaxVolumesPerNode,
		AccessibleTopology: &csipb.Topology{
			Segments: info.AccessibleTopology,
		},
	}, nil
}

func (s *nodeServer) NodeStageVolume(ctx context.Context, req *csipb.NodeStageVolumeRequest) (resp *csipb.NodeStageVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeStageVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and staging target path are required")
	}
	ctx = obs.WithVolume(ctx, req.GetVolumeId())
	if err := s.n.Stage(ctx, node.StageRequest{
		VolumeID:         req.GetVolumeId(),
		StagingPath:      req.GetStagingTargetPath(),
		PublishContext:   mergeCtx(req.GetVolumeContext(), req.GetPublishContext()),
		VolumeCapability: capOf(req.GetVolumeCapability()),
		Secrets:          req.GetSecrets(),
	}); err != nil {
		return nil, nodeErr(err)
	}
	// Remember how to detach. A failure here is not fatal to the mount, but it
	// does mean unstage may not be able to log out, so it is logged loudly.
	if err := writeStageRecord(req.GetStagingTargetPath(),
		mergeCtx(req.GetVolumeContext(), req.GetPublishContext())); err != nil {
		obs.Logger(ctx).Warn("could not record publish context for unstage; "+
			"the iscsi session may have to be cleaned up by hand",
			"error", obs.Redact(err.Error()))
	}
	return &csipb.NodeStageVolumeResponse{}, nil
}

func (s *nodeServer) NodeUnstageVolume(ctx context.Context, req *csipb.NodeUnstageVolumeRequest) (resp *csipb.NodeUnstageVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeUnstageVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and staging target path are required")
	}
	ctx = obs.WithVolume(ctx, req.GetVolumeId())
	pc := readStageRecord(req.GetStagingTargetPath())
	if err := s.n.Unstage(ctx, node.UnstageRequest{
		VolumeID:       req.GetVolumeId(),
		StagingPath:    req.GetStagingTargetPath(),
		PublishContext: pc,
	}); err != nil {
		return nil, nodeErr(err)
	}
	_ = os.Remove(stageRecordPath(req.GetStagingTargetPath()))
	return &csipb.NodeUnstageVolumeResponse{}, nil
}

func (s *nodeServer) NodePublishVolume(ctx context.Context, req *csipb.NodePublishVolumeRequest) (resp *csipb.NodePublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodePublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	ctx = obs.WithVolume(ctx, req.GetVolumeId())
	pc := mergeCtx(req.GetVolumeContext(), req.GetPublishContext())
	if len(pc) == 0 {
		pc = readStageRecord(req.GetStagingTargetPath())
	}
	if err := s.n.Publish(ctx, node.PublishRequest{
		VolumeID:         req.GetVolumeId(),
		StagingPath:      req.GetStagingTargetPath(),
		TargetPath:       req.GetTargetPath(),
		PublishContext:   pc,
		VolumeCapability: capOf(req.GetVolumeCapability()),
		Readonly:         req.GetReadonly(),
	}); err != nil {
		return nil, nodeErr(err)
	}
	// Also record it against the target path: NodeExpandVolume is called with
	// the published volume path and may carry no staging path at all.
	if err := writeStageRecord(req.GetTargetPath(), pc); err != nil {
		obs.Logger(ctx).Warn("could not record publish context at the target path",
			"error", obs.Redact(err.Error()))
	}
	return &csipb.NodePublishVolumeResponse{}, nil
}

func (s *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csipb.NodeUnpublishVolumeRequest) (resp *csipb.NodeUnpublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeUnpublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and target path are required")
	}
	ctx = obs.WithVolume(ctx, req.GetVolumeId())
	if err := s.n.Unpublish(ctx, node.UnpublishRequest{
		VolumeID: req.GetVolumeId(), TargetPath: req.GetTargetPath(),
	}); err != nil {
		return nil, nodeErr(err)
	}
	_ = os.Remove(stageRecordPath(req.GetTargetPath()))
	return &csipb.NodeUnpublishVolumeResponse{}, nil
}

func (s *nodeServer) NodeExpandVolume(ctx context.Context, req *csipb.NodeExpandVolumeRequest) (resp *csipb.NodeExpandVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeExpandVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and volume path are required")
	}
	ctx = obs.WithVolume(ctx, req.GetVolumeId())
	if _, statErr := os.Stat(req.GetVolumePath()); statErr != nil {
		return nil, status.Errorf(codes.NotFound,
			"volume path %s does not exist on this node", req.GetVolumePath())
	}
	staging := req.GetStagingTargetPath()
	pc := readStageRecord(staging)
	if len(pc) == 0 {
		pc = readStageRecord(req.GetVolumePath())
	}
	out, err := s.n.Expand(ctx, node.ExpandRequest{
		VolumeID:         req.GetVolumeId(),
		VolumePath:       req.GetVolumePath(),
		StagingPath:      staging,
		PublishContext:   pc,
		VolumeCapability: capOf(req.GetVolumeCapability()),
		CapacityBytes:    req.GetCapacityRange().GetRequiredBytes(),
	})
	if err != nil {
		return nil, nodeErr(err)
	}
	return &csipb.NodeExpandVolumeResponse{CapacityBytes: out.CapacityBytes}, nil
}

// NodeGetVolumeHealth reports the condition the node's health monitor last
// observed for a volume. It answers from the monitor's recorded state rather
// than probing inline: the probe is the thing that can hang, and an RPC that
// hangs tells the CO nothing while also occupying one of its workers.
func (s *nodeServer) NodeGetVolumeHealth(ctx context.Context, req *csipb.NodeGetVolumeHealthRequest) (resp *csipb.NodeGetVolumeHealthResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeGetVolumeHealth", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	health := &csipb.VolumeHealth{VolumeId: req.GetVolumeId()}
	if abnormal, message := s.n.Health().Condition(req.GetVolumeId()); abnormal {
		health.HealthStatuses = []*csipb.VolumeHealth_VolumeHealthEntry{{
			Status: csipb.VolumeHealthErrorType_INACCESSIBLE,
			// A brief CamelCase reason, as the spec requires, with the detail
			// in the message.
			Reason:  "DataPathUnreachable",
			Message: obs.Redact(message),
		}}
	}
	return &csipb.NodeGetVolumeHealthResponse{VolumeHealth: health}, nil
}

func (s *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csipb.NodeGetVolumeStatsRequest) (resp *csipb.NodeGetVolumeStatsResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("NodeGetVolumeStats", err, time.Since(start)) }()

	if req.GetVolumeId() == "" || req.GetVolumePath() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id and volume path are required")
	}
	if _, statErr := os.Stat(req.GetVolumePath()); statErr != nil {
		return nil, status.Errorf(codes.NotFound,
			"volume path %s does not exist on this node", req.GetVolumePath())
	}
	out, err := s.n.Stats(ctx, node.StatsRequest{
		VolumeID: req.GetVolumeId(), VolumePath: req.GetVolumePath()})
	if err != nil {
		return nil, nodeErr(err)
	}
	resp = &csipb.NodeGetVolumeStatsResponse{}
	for _, u := range out.Usage {
		unit := csipb.VolumeUsage_BYTES
		if u.Unit == node.UnitInodes {
			unit = csipb.VolumeUsage_INODES
		}
		resp.Usage = append(resp.Usage, &csipb.VolumeUsage{
			Unit: unit, Total: u.Total, Used: u.Used, Available: u.Available})
	}
	return resp, nil
}
