package csi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
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
	usable := usableCapacity(p.Free.Parsed, b.Reserve(p.Size.Parsed))
	return &csipb.GetCapacityResponse{
		AvailableCapacity: usable,
		// The largest volume this driver would actually create is the same
		// figure, and for a real reason rather than for want of a better one:
		// CreateVolume refuses anything that would eat into the reserve
		// (requireRoomOutsideReserve), so a request above this is not merely
		// unlikely to fit, it is guaranteed to be rejected. Reporting it lets
		// the scheduler leave such a claim Pending rather than bind it and
		// discover the refusal at provisioning time.
		//
		// Neither ZFS nor the middleware imposes a lower per-volume ceiling:
		// refquota and volsize are 64-bit byte counts, and a thin zvol may even
		// be created larger than the pool. That last case is exactly why the
		// reserve check, and not the appliance, is the binding constraint here.
		MaximumVolumeSize: wrapperspb.Int64(usable),
	}, nil
}

// usableCapacity is the free space this driver may hand out: the pool's free
// space less the operator's reservation, clamped at zero.
//
// The clamp is not cosmetic. A pool already inside its reserve would otherwise
// report a NEGATIVE capacity, which the external-provisioner treats as an
// enormous unsigned figure and happily schedules against.
func usableCapacity(poolFree, reserve int64) int64 {
	if avail := poolFree - reserve; avail > 0 {
		return avail
	}
	return 0
}

// requireRoomOutsideReserve refuses a volume that would eat into the operator's
// pool reservation.
//
// Reporting a reduced capacity is not enough on its own: the scheduler consults
// CSIStorageCapacity opportunistically, a claim can be made before the figure
// refreshes, and nothing stops a user creating a PVC larger than the reported
// capacity. Without this check the reserve is a suggestion, and a full pool is
// exactly the failure the reserve exists to prevent.
//
// A backend with no reservation configured is not queried at all, so the common
// case costs no extra appliance round trip.
func (c *controller) requireRoomOutsideReserve(ctx context.Context, backendName string, size int64) error {
	b, err := c.reg.Backend(backendName)
	if err != nil {
		return toStatus(err)
	}
	if b.ReservedBytes <= 0 && b.ReservedPercent <= 0 {
		return nil
	}
	cl, err := c.reg.Client(ctx, backendName)
	if err != nil {
		return toStatus(err)
	}
	p, err := cl.PoolQuery(ctx, b.Pool)
	if err != nil {
		return toStatus(err)
	}
	reserve := b.Reserve(p.Size.Parsed)
	usable := usableCapacity(p.Free.Parsed, reserve)
	if size > usable {
		return status.Errorf(codes.ResourceExhausted,
			"pool %q on backend %q has %d bytes free and reserves %d of them, leaving %d usable; "+
				"the request for %d bytes would eat into the reserve",
			b.Pool, backendName, p.Free.Parsed, reserve, usable, size)
	}
	return nil
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
			// Fall back to the historical guess only for volumes created
			// before the protocol was recorded; a zvol may be iscsi or nvme.
			fallback, size := "nfs", d.RefQuota.Parsed
			if d.Type == "VOLUME" {
				fallback, size = "iscsi", d.VolSize.Parsed
			}
			proto := volume.ProtocolOr(d.LocalProperty(volume.ProtocolProperty), fallback)
			vid := volume.ID{Backend: name, Protocol: proto, Pool: b.Pool, Parent: b.ParentDataset, Name: leaf}
			// The publish ledger is already in this dataset's user properties,
			// so LIST_VOLUMES_PUBLISHED_NODES costs nothing beyond decoding it.
			// A ledger that will not decode is reported as no published nodes
			// rather than failing the listing: the CO is required to tolerate
			// an incomplete published-node list, and is not required to
			// tolerate ListVolumes failing outright.
			grants, gErr := volume.DecodeGrants(d.LocalProperty(volume.PublishedProperty))
			if gErr != nil {
				obs.Logger(ctx).Warn("volume has an unreadable publish ledger",
					"volume", vid.String(), "error", obs.Redact(gErr.Error()))
			}
			all = append(all, volEntry{id: vid.String(), size: size, published: grants.Nodes()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].id < all[j].id })

	startIdx := 0
	if tok := req.GetStartingToken(); tok != "" {
		found := -1
		for i, e := range all {
			if e.id == tok {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, status.Errorf(codes.Aborted, "invalid starting_token %q", tok)
		}
		startIdx = found
	}
	max := int(req.GetMaxEntries())
	if max <= 0 || startIdx+max > len(all) {
		max = len(all) - startIdx
	}
	page := all[startIdx : startIdx+max]

	out := &csipb.ListVolumesResponse{}
	for _, e := range page {
		out.Entries = append(out.Entries, &csipb.ListVolumesResponse_Entry{
			Volume: &csipb.Volume{VolumeId: e.id, CapacityBytes: e.size},
			Status: &csipb.ListVolumesResponse_VolumeStatus{PublishedNodeIds: e.published},
		})
	}
	if next := startIdx + max; next < len(all) {
		out.NextToken = all[next].id
	}
	return out, nil
}

// volEntry is one owned volume discovered while listing.
type volEntry struct {
	id        string
	size      int64
	published []string // node ids holding an access grant, from the publish ledger
}

// ListSnapshots enumerates snapshots, honouring the spec's optional filters.
//
// Advertising LIST_SNAPSHOTS without implementing it makes the external
// snapshotter's periodic reconciliation fail, so this is not optional once the
// capability is claimed.
func (c *controller) ListSnapshots(ctx context.Context, req *csipb.ListSnapshotsRequest) (resp *csipb.ListSnapshotsResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ListSnapshots", err, time.Since(start)) }()

	// A specific snapshot was asked for: return it, or nothing.
	if id := req.GetSnapshotId(); id != "" {
		backendName, zfsID, parseErr := backend.SnapshotSource(id)
		if parseErr != nil {
			return &csipb.ListSnapshotsResponse{}, nil // unknown id: empty, not an error
		}
		cl, clErr := c.reg.Client(ctx, backendName)
		if clErr != nil {
			return &csipb.ListSnapshotsResponse{}, nil
		}
		snap, qErr := cl.SnapshotQuery(ctx, zfsID)
		if qErr != nil || snap == nil {
			return &csipb.ListSnapshotsResponse{}, nil
		}
		return &csipb.ListSnapshotsResponse{Entries: []*csipb.ListSnapshotsResponse_Entry{
			{Snapshot: snapshotPB(id, snap.Dataset, backendName)}}}, nil
	}

	var all []*csipb.Snapshot
	names := c.reg.Names()
	sort.Strings(names)
	for _, name := range names {
		snaps, listErr := c.reg.ListSnapshots(ctx, name)
		if listErr != nil {
			obs.Logger(ctx).Warn("skipping backend while listing snapshots", "backend", name)
			continue
		}
		for _, s := range snaps {
			all = append(all, snapshotPB(s.ID, s.SourceVolumeID, name))
		}
	}

	// Filter by source volume when asked.
	if src := req.GetSourceVolumeId(); src != "" {
		id, parseErr := volume.ParseID(src)
		if parseErr != nil {
			return &csipb.ListSnapshotsResponse{}, nil
		}
		want := id.DatasetPath()
		var kept []*csipb.Snapshot
		for _, s := range all {
			if s.GetSourceVolumeId() == want || s.GetSourceVolumeId() == src {
				kept = append(kept, s)
			}
		}
		all = kept
	}

	sort.Slice(all, func(i, j int) bool { return all[i].GetSnapshotId() < all[j].GetSnapshotId() })

	startIdx := 0
	if tok := req.GetStartingToken(); tok != "" {
		found := -1
		for i, s := range all {
			if s.GetSnapshotId() == tok {
				found = i
				break
			}
		}
		if found < 0 {
			return nil, status.Errorf(codes.Aborted, "invalid starting_token %q", tok)
		}
		startIdx = found
	}
	max := int(req.GetMaxEntries())
	if max <= 0 || startIdx+max > len(all) {
		max = len(all) - startIdx
	}

	out := &csipb.ListSnapshotsResponse{}
	for _, s := range all[startIdx : startIdx+max] {
		out.Entries = append(out.Entries, &csipb.ListSnapshotsResponse_Entry{Snapshot: s})
	}
	if next := startIdx + max; next < len(all) {
		out.NextToken = all[next].GetSnapshotId()
	}
	return out, nil
}

type backendPaths struct{ pool, parent string }

// csiRegistryBackend is set by NewController so snapshotPB can resolve a
// dataset path back to a volume id without threading the registry through.
var csiRegistryBackend = func(string) (backendPaths, error) {
	return backendPaths{}, errNoRegistry
}

var errNoRegistry = errors.New("no registry bound")

// snapshotPB builds the wire form, mapping the ZFS source dataset back to a
// driver volume id so the CO can correlate it with a PersistentVolume.
func snapshotPB(id, sourceDataset, backendName string) *csipb.Snapshot {
	source := sourceDataset
	if b, err := csiRegistryBackend(backendName); err == nil {
		prefix := b.pool + "/" + b.parent + "/"
		if strings.HasPrefix(sourceDataset, prefix) {
			source = (volume.ID{Backend: backendName, Protocol: "nfs", Pool: b.pool,
				Parent: b.parent, Name: strings.TrimPrefix(sourceDataset, prefix)}).String()
		}
	}
	return &csipb.Snapshot{
		SnapshotId: id, SourceVolumeId: source,
		CreationTime: timestamppb.New(time.Unix(0, 0)), ReadyToUse: true,
	}
}
