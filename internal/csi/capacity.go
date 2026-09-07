package csi

import (
	"context"
	"sort"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/backend"
	"github.com/pwatteel/truenas-csi/internal/obs"
	"github.com/pwatteel/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetCapacity reports the real free space of the pool behind a StorageClass, so
// the scheduler leaves an oversized claim Pending instead of failing at attach.
func (c *controller) GetCapacity(ctx context.Context, req *csipb.GetCapacityRequest) (resp *csipb.GetCapacityResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("GetCapacity", err, time.Since(start)) }()

	params := req.GetParameters()
	name := params["backend"]
	if name == "" {
		if len(c.cfg.Backends) != 1 {
			return nil, status.Error(codes.InvalidArgument,
				"storage class must set the 'backend' parameter when several backends are configured")
		}
		for n := range c.cfg.Backends {
			name = n
		}
	}
	b, ok := c.cfg.Backends[name]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "%v: %q", backend.ErrUnknownBackend, name)
	}
	cl, err := c.reg.Client(ctx, name)
	if err != nil {
		return nil, toStatus(err)
	}
	p, err := cl.PoolQuery(ctx, b.Pool)
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.GetCapacityResponse{AvailableCapacity: p.Free.Parsed}, nil
}

// ListVolumes returns only volumes this driver owns, paginated by dataset id.
//
// Ownership is checked with source == "LOCAL": a dataset that merely inherited
// the marker from a parent is somebody else's data and must not be listed as
// ours, let alone deleted later.
func (c *controller) ListVolumes(ctx context.Context, req *csipb.ListVolumesRequest) (resp *csipb.ListVolumesResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ListVolumes", err, time.Since(start)) }()

	var all []volEntry

	names := c.reg.Names()
	sort.Strings(names)
	for _, name := range names {
		b, cfgErr := c.reg.Backend(name)
		if cfgErr != nil {
			continue
		}
		cl, clErr := c.reg.Client(ctx, name)
		if clErr != nil {
			// One unreachable appliance must not fail the whole listing.
			obs.Logger(ctx).Warn("skipping unreachable backend while listing", "backend", name)
			continue
		}
		prefix := b.Pool + "/" + b.ParentDataset + "/"
		datasets, dsErr := cl.DatasetList(ctx, prefix)
		if dsErr != nil {
			return nil, toStatus(dsErr)
		}
		for i := range datasets {
			d := &datasets[i]
			if !d.Owned(volume.OwnerProperty, volume.OwnerValue) {
				continue
			}
			leaf := d.ID[len(prefix):]
			proto := "nfs"
			size := d.RefQuota.Parsed
			if d.Type == "VOLUME" {
				proto, size = "iscsi", d.VolSize.Parsed
			}
			vid := volume.ID{Backend: name, Protocol: proto, Pool: b.Pool, Parent: b.ParentDataset, Name: leaf}
			all = append(all, volEntry{id: vid.String(), size: size})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })

	startIdx := 0
	if t := req.GetStartingToken(); t != "" {
		startIdx = sort.SearchStrings(idsOf(all), t)
		if startIdx >= len(all) || all[startIdx].id != t {
			return nil, status.Errorf(codes.Aborted, "invalid starting_token %q", t)
		}
	}
	max := int(req.GetMaxEntries())
	if max <= 0 || startIdx+max > len(all) {
		max = len(all) - startIdx
	}
	page := all[startIdx : startIdx+max]

	out := &csipb.ListVolumesResponse{}
	for _, e := range page {
		out.Entries = append(out.Entries, &csipb.ListVolumesResponse_Entry{
			Volume: &csipb.Volume{VolumeId: e.id, CapacityBytes: e.size}})
	}
	if next := startIdx + max; next < len(all) {
		out.NextToken = all[next].id
	}
	return out, nil
}

// volEntry is one owned volume discovered while listing.
type volEntry struct {
	id   string
	size int64
}

func idsOf(es []volEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.id
	}
	return out
}
