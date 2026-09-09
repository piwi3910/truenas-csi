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
//
// It reports POOL capacity even when per-namespace quotas are in force, and
// deliberately says nothing about them. GetCapacityRequest carries the
// StorageClass parameters, the topology and the volume capabilities — it does
// not carry a namespace, and it cannot: the external-provisioner produces one
// CSIStorageCapacity object per StorageClass and topology segment, which every
// namespace in the cluster then reads. There is no per-namespace answer to
// give, and folding the smallest or the largest namespace quota into a
// cluster-wide figure would be a number that is wrong for every namespace but
// one. So the scheduler keeps being told what the pool can hold, and the
// namespace ceiling is enforced where it can be answered honestly:
// requireRoomInNamespaceQuota, and the ZFS quota underneath it. The visible
// consequence is that a PVC over its namespace quota binds and then fails
// provisioning with ResourceExhausted, rather than staying Pending.
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

// requireRoomInNamespaceQuota reconciles the namespace's parent dataset and
// refuses a volume that would take the namespace past its quota.
//
// It refuses rather than reports, for the same reason requireRoomOutsideReserve
// does: GetCapacity is advisory, and nothing stops a user creating a PVC larger
// than any figure the scheduler saw. Unlike the pool reserve, though, this is
// the driver's SECOND line rather than its only one — the ZFS quota on the
// namespace dataset is the first, and it is the one that holds against bytes
// the driver never saw. What this check adds is a clear ResourceExhausted at
// provisioning time instead of an EDQUOT surfacing as a middleware error, and
// a ceiling on THIN volumes, whose provisioned size ZFS does not charge against
// the quota until the data is actually written — which is why the namespace is
// measured against the LARGER of its usage and its provisioned bytes, and not
// against usage alone. See NamespaceDataset.Room.
//
// A flat volume — the feature off, or no namespace in the request — returns
// immediately and costs no appliance round trip.
func (c *controller) requireRoomInNamespaceQuota(ctx context.Context, id volume.ID, size int64) error {
	if id.Namespace == "" {
		return nil
	}
	b, err := c.reg.Backend(id.Backend)
	if err != nil {
		return toStatus(err)
	}
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return toStatus(err)
	}
	quota := b.NamespaceQuotas.QuotaFor(id.Namespace)
	ns, err := backend.EnsureNamespace(ctx, cl, id.Pool, id.Parent, id.Namespace, quota)
	if err != nil {
		return toStatus(err)
	}
	room, limited := ns.Room()
	if !limited || size <= room {
		return nil
	}
	// The deferred case is called out because the operator's own quota edit is
	// the reason the namespace has no room, and the message is the only place
	// they will see that the ZFS quota still reads the old, higher figure.
	deferred := ""
	if ns.QuotaDeferred {
		deferred = " (this quota is below current usage and was therefore not applied to ZFS, " +
			"so existing workloads keep writing while new volumes are refused)"
	}
	// Both figures are named, because which one binds decides what the operator
	// does next: over USED means delete data or raise the quota, over
	// PROVISIONED means delete a claim nobody is filling.
	return status.Errorf(codes.ResourceExhausted,
		"namespace %q on backend %q has a quota of %d bytes; it uses %d and has provisioned %d, "+
			"leaving %d; the request for %d bytes does not fit%s",
		id.Namespace, id.Backend, ns.QuotaBytes, ns.UsedBytes, ns.ProvisionedBytes,
		room, size, deferred)
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
			// A namespace's parent dataset is driver-owned but holds no data of
			// its own. Listing it would hand the CO a handle for something that
			// is not a volume, and DeleteVolume on that handle would then try
			// to destroy a dataset full of other people's volumes.
			if volume.IsNamespaceDataset(d.LocalProperty(volume.NamespaceProperty)) {
				continue
			}
			// Delete protection's two shapes, skipped for the same reason and
			// by their LOCAL markers rather than by the configured graveyard
			// name, so that leftovers keep being skipped after an operator
			// turns delete protection off or renames the graveyard:
			//
			//   - the graveyard itself, a driver-owned container with no
			//     PersistentVolume, which is not a volume;
			//   - a retired volume, whose handle the CO has already been told
			//     is gone. Listing it would resurrect a handle DeleteVolume
			//     reported as deleted, and its dataset sits one level deeper
			//     than this driver provisions, so the handle would name the
			//     wrong thing anyway.
			if volume.IsGraveyard(d.LocalProperty(volume.GraveyardProperty)) ||
				volume.IsRetired(d.LocalProperty(volume.DeletedAtProperty)) {
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
			vid, leafErr := volume.IDFromLeaf(name, proto, b.Pool, b.ParentDataset, leaf)
			if leafErr != nil {
				// A dataset at a depth this driver never creates: report
				// nothing rather than a handle that names the wrong thing.
				obs.Logger(ctx).Warn("skipping a driver-owned dataset at an unexpected depth",
					"dataset", d.ID, "error", leafErr)
				continue
			}
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
		backendName, _, parseErr := backend.SnapshotSource(id)
		if parseErr != nil {
			return &csipb.ListSnapshotsResponse{}, nil // unknown id: empty, not an error
		}
		snap, qErr := c.reg.Snapshot(ctx, id)
		if qErr != nil || snap == nil {
			return &csipb.ListSnapshotsResponse{}, nil
		}
		return &csipb.ListSnapshotsResponse{Entries: []*csipb.ListSnapshotsResponse_Entry{
			{Snapshot: snapshotPB(id, snap.SourceVolumeID, backendName,
				snap.SizeBytes, snap.CreationTime)}}}, nil
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
			all = append(all, snapshotPB(s.ID, s.SourceVolumeID, name, s.SizeBytes, s.CreationTime))
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
func snapshotPB(id, sourceDataset, backendName string, sizeBytes int64, creation time.Time) *csipb.Snapshot {
	source := sourceDataset
	if b, err := csiRegistryBackend(backendName); err == nil {
		prefix := b.pool + "/" + b.parent + "/"
		if strings.HasPrefix(sourceDataset, prefix) {
			// A namespaced volume's dataset is one level deeper, so the leaf is
			// split rather than taken whole. A leaf that will not split leaves
			// the raw dataset path in place, which is what this did before any
			// mapping existed.
			if vid, leafErr := volume.IDFromLeaf(backendName, "nfs", b.pool, b.parent,
				strings.TrimPrefix(sourceDataset, prefix)); leafErr == nil {
				source = vid.String()
			}
		}
	}
	return &csipb.Snapshot{
		SnapshotId: id, SourceVolumeId: source, SizeBytes: sizeBytes,
		CreationTime: timestamppb.New(creation), ReadyToUse: true,
	}
}
