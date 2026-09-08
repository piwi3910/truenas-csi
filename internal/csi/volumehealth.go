package csi

import (
	"context"
	"fmt"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ControllerGetVolume reports what the appliance knows about one volume.
//
// It answers the two questions the controller is the only participant able to
// answer: how large the volume really is on the pool right now — which is not
// necessarily what the PersistentVolume records, because an expansion that the
// CO lost track of still happened — and which nodes hold an appliance-side
// access grant on it, read from the volume's own publish ledger.
//
// It deliberately does NOT report a mount: no controller can see one.
func (c *controller) ControllerGetVolume(ctx context.Context, req *csipb.ControllerGetVolumeRequest) (resp *csipb.ControllerGetVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerGetVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	ctx = obs.WithVolume(ctx, id.String())

	ds, err := c.volumeDataset(ctx, id)
	if err != nil {
		return nil, err
	}
	grants, err := volume.DecodeGrants(ds.LocalProperty(volume.PublishedProperty))
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerGetVolumeResponse{
		Volume: &csipb.Volume{
			VolumeId:      req.GetVolumeId(),
			CapacityBytes: datasetCapacity(ds),
		},
		Status: &csipb.ControllerGetVolumeResponse_VolumeStatus{
			PublishedNodeIds: grants.Nodes(),
		},
	}, nil
}

// ControllerGetVolumeHealth reports the conditions the CONTROLLER can observe
// for a volume, which is a strictly smaller set than the node's.
//
// What the controller can see is the appliance: whether the dataset is still
// there, whether the pool under it is degraded, and whether the volume has any
// space left to be written to. What it cannot see is whether a node's mount
// still works — that is the node plugin's NodeGetVolumeHealth, fed by
// internal/node's health monitor, and inventing an equivalent here from
// appliance state would be a health signal this driver cannot observe.
//
// So an empty health_statuses list means "nothing the appliance can show me is
// wrong", never "the volume is fine everywhere".
func (c *controller) ControllerGetVolumeHealth(ctx context.Context, req *csipb.ControllerGetVolumeHealthRequest) (resp *csipb.ControllerGetVolumeHealthResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerGetVolumeHealth", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	ctx = obs.WithVolume(ctx, id.String())

	ds, err := c.volumeDataset(ctx, id)
	if err != nil {
		return nil, err
	}
	b, err := c.reg.Backend(id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	// One pool query per call rather than a cached figure: the CO asks this
	// on its own health-monitoring interval — minutes, not the attach path —
	// and a cached "the pool was healthy a while ago" is precisely the answer
	// a health check must not give.
	p, err := cl.PoolQuery(ctx, b.Pool)
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerGetVolumeHealthResponse{VolumeHealth: &csipb.VolumeHealth{
		VolumeId:       req.GetVolumeId(),
		HealthStatuses: applianceConditions(p, ds),
	}}, nil
}

// applianceConditions is every abnormal condition the appliance itself reports
// about a volume. Each one corresponds to an observed fact, never to an
// inference about a client.
func applianceConditions(p *truenas.Pool, ds *truenas.Dataset) []*csipb.VolumeHealth_VolumeHealthEntry {
	var out []*csipb.VolumeHealth_VolumeHealthEntry

	// A degraded or faulted pool is real, it affects every volume on it, and
	// no node can see it. This is the condition controller-side health exists
	// for. `healthy` is false for warnings too, so the status string carries
	// the detail rather than the flag alone.
	if p != nil && (!p.Healthy || (p.Status != "" && p.Status != "ONLINE")) {
		out = append(out, &csipb.VolumeHealth_VolumeHealthEntry{
			Status: csipb.VolumeHealthErrorType_DEGRADED,
			Reason: "PoolDegraded",
			Message: fmt.Sprintf("the pool %s backing this volume reports status %s (healthy=%t)",
				p.Name, p.Status, p.Healthy),
		})
	}

	// A filesystem volume whose refquota is reached has zero bytes available:
	// every write fails with ENOSPC while reads keep working, which is exactly
	// DEGRADED. Only checked when a quota is set, because `available` on a
	// dataset without one tracks the whole pool and says nothing about the
	// volume. A zvol is deliberately excluded: its size is preallocated by
	// volsize, so `available` there describes the pool's headroom for
	// overcommit and not the volume's own space.
	if ds != nil && ds.Type != "VOLUME" && ds.RefQuota.Parsed > 0 && ds.Available.Parsed <= 0 {
		out = append(out, &csipb.VolumeHealth_VolumeHealthEntry{
			Status: csipb.VolumeHealthErrorType_DEGRADED,
			Reason: "VolumeFull",
			Message: fmt.Sprintf("the dataset %s has no space left within its %d byte quota, "+
				"so writes to it fail", ds.ID, ds.RefQuota.Parsed),
		})
	}
	return out
}

// volumeDataset resolves a volume id to the dataset behind it, with the
// NotFound the spec requires for a volume that is no longer there.
func (c *controller) volumeDataset(ctx context.Context, id volume.ID) (*truenas.Dataset, error) {
	b, ok := c.cfg.Backends[id.Backend]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%v: %q", backend.ErrUnknownBackend, id.Backend)
	}
	// A volume id naming a path outside the backend's own parent dataset is not
	// a volume of ours to describe, whatever exists at that path.
	if err := volume.Confine(id, b.Pool, b.ParentDataset); err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %s", id)
	}
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	ds, err := cl.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return nil, toStatus(err)
	}
	if ds == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	return ds, nil
}

// datasetCapacity is the size a volume was provisioned at: volsize for a zvol,
// refquota for a filesystem, which is how each protocol's Create sets it.
func datasetCapacity(ds *truenas.Dataset) int64 {
	if ds == nil {
		return 0
	}
	if ds.Type == "VOLUME" {
		return ds.VolSize.Parsed
	}
	return ds.RefQuota.Parsed
}
