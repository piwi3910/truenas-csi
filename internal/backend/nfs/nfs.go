// Package nfs provisions NFS volumes: one ZFS dataset per volume, quota'd with
// refquota, stamped as driver-owned, permissioned for the workload, and exported
// as an NFS share.
//
// Every rule enforced here was verified against a live TrueNAS 25.10.6:
//   - without refquota a pod's df reports the WHOLE POOL (31T for a 10Gi PVC),
//     so NodeGetVolumeStats is meaningless and one PVC can consume the pool;
//   - a fresh dataset is root:root 0755 and a non-root pod cannot write to it —
//     fsGroup does NOT fix this, kubelet skips fsGroup for NFS, so permissions
//     must be set here at provisioning time;
//   - the middleware SILENTLY PERMITS a refquota shrink (unlike a zvol volsize
//     shrink, which it refuses), so the shrink guard cannot be delegated;
//   - a ZFS clone inherits NEITHER the ownership marker NOR refquota, so a
//     restored volume must be stamped and quota'd explicitly or it leaks forever.
package nfs

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Protocol is the StorageClass "protocol" value this backend serves.
const Protocol = "nfs"

// StorageClass parameter names.
const (
	ParamServer     = "server"
	ParamNFSVersion = "nfsVersion"
	ParamNetworks   = "networks"
	ParamMaproot    = "maproot"
	ParamMode       = "mode"
	ParamUID        = "uid"
	ParamGID        = "gid"
)

// Defaults for the StorageClass parameters.
const (
	// defaultNFSVersion is 4 deliberately: mounting with vers=3 pulls rpc-statd
	// onto the node (systemd enabled it during validation) because v3 needs the
	// separate lock manager; v4 mounts with no such dependency.
	defaultNFSVersion = "4"
	// defaultMode is permissive because the alternative — a dataset no pod can
	// write to — is the single most common NFS-CSI failure mode. Operators who
	// want tighter permissions set mode/uid/gid on the StorageClass.
	defaultMode    = "0777"
	defaultMaproot = "root"
)

func init() { backend.Register(Protocol, New) }

// Backend provisions NFS volumes on one appliance.
type Backend struct {
	c      truenas.API
	pool   string
	parent string
	// retire is the delete-protection policy. Its zero value is "off", which is
	// the default and takes exactly the destroy path this driver always took.
	retire retention.Policy

	// mu guards the caches below. They are conveniences, never a source of
	// truth: everything they hold can be recomputed from the appliance or from
	// the PersistentVolume's own volume context, so a controller restart loses
	// nothing that matters.
	mu       sync.Mutex
	server   string
	versions map[string]string
}

// New builds an NFS backend bound to one appliance, pool and parent dataset.
func New(c truenas.API, opts backend.Options) backend.Backend {
	return &Backend{c: c, pool: opts.Pool, parent: opts.Parent, retire: opts.Retention,
		versions: map[string]string{}}
}

// Protocol implements backend.Backend.
func (b *Backend) Protocol() string { return Protocol }

// params is the resolved StorageClass configuration for one request.
type params struct {
	server     string
	nfsVersion string
	networks   []string
	maproot    string
	mode       string
	uid        int
	gid        int
}

func parseParams(p map[string]string) (params, error) {
	out := params{nfsVersion: defaultNFSVersion, mode: defaultMode, maproot: defaultMaproot}
	get := func(k string) string { return strings.TrimSpace(p[k]) }

	if v := get(ParamServer); v != "" {
		out.server = v
	}
	if v := get(ParamNFSVersion); v != "" {
		if v != "3" && v != "4" {
			return params{}, status.Errorf(codes.InvalidArgument,
				"%s=%q: only 3 and 4 are supported", ParamNFSVersion, v)
		}
		out.nfsVersion = v
	}
	if v := get(ParamNetworks); v != "" {
		for _, n := range strings.Split(v, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out.networks = append(out.networks, n)
			}
		}
	}
	if v := get(ParamMaproot); v != "" {
		out.maproot = v
	}
	if v := get(ParamMode); v != "" {
		if _, err := strconv.ParseUint(v, 8, 32); err != nil {
			return params{}, status.Errorf(codes.InvalidArgument, "%s=%q is not an octal mode", ParamMode, v)
		}
		out.mode = v
	}
	for _, f := range []struct {
		key string
		dst *int
	}{{ParamUID, &out.uid}, {ParamGID, &out.gid}} {
		v := get(f.key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return params{}, status.Errorf(codes.InvalidArgument, "%s=%q is not a valid id", f.key, v)
		}
		*f.dst = n
	}
	return out, nil
}

// mountpointOf prefers what the appliance reports and falls back to the
// conventional /mnt/<dataset> layout, which is what the live box uses.
func mountpointOf(ds *truenas.Dataset, dsPath string) string {
	if ds != nil && ds.Mountpoint != "" {
		return ds.Mountpoint
	}
	return "/mnt/" + dsPath
}

// Create provisions a volume, or returns the existing one unchanged.
//
// When r.SourceSnapshot is set the volume is restored by cloning that snapshot;
// the clone is then explicitly stamped, quota'd and permissioned, because it
// inherits none of those from its origin.
func (b *Backend) Create(ctx context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	if err := volume.Confine(r.ID, b.pool, b.parent); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if r.CapacityBytes <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "capacity must be positive, got %d", r.CapacityBytes)
	}
	p, err := parseParams(r.Params)
	if err != nil {
		return nil, err
	}
	b.remember(r.ID, p)

	dsPath := r.ID.DatasetPath()

	existing, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if existing != nil {
		// CSI requires an identical repeat to succeed and a conflicting one to
		// be reported, not silently reconciled.
		if existing.RefQuota.Parsed != r.CapacityBytes {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists with refquota %d bytes, requested %d",
				r.ID, existing.RefQuota.Parsed, r.CapacityBytes)
		}
		if err := b.ensureShare(ctx, mountpointOf(existing, dsPath), p); err != nil {
			return nil, err
		}
		if err := b.recordNetworkPolicy(ctx, dsPath, p); err != nil {
			return nil, err
		}
		return b.volumeFor(r.ID, r.CapacityBytes, mountpointOf(existing, dsPath), p), nil
	}

	ds, err := b.provision(ctx, dsPath, r)
	if err != nil {
		return nil, err
	}
	mountpoint := mountpointOf(ds, dsPath)

	// Everything past this point is rolled back on failure: a dataset without a
	// share, or one still root:root, is an orphan no retry can reconcile.
	rollback := func(cause error) error {
		if delErr := b.c.DatasetDelete(ctx, dsPath, true, true); delErr != nil {
			return status.Errorf(codes.Internal,
				"%v; rolling back dataset %s also failed: %v — it must be removed by hand",
				cause, dsPath, delErr)
		}
		return cause
	}

	// setperm BEFORE the export: a share published while the dataset is still
	// root:root 0755 is briefly mountable and unwritable by non-root pods.
	if err := b.c.SetPerm(ctx, mountpoint, p.mode, p.uid, p.gid); err != nil {
		return nil, rollback(status.Errorf(codes.Internal, "set permissions on %s: %v", mountpoint, err))
	}
	if err := b.ensureShare(ctx, mountpoint, p); err != nil {
		return nil, rollback(err)
	}
	if err := b.recordNetworkPolicy(ctx, dsPath, p); err != nil {
		return nil, rollback(err)
	}
	return b.volumeFor(r.ID, r.CapacityBytes, mountpoint, p), nil
}

// provision creates the dataset itself, either empty or as a clone of a
// snapshot, and returns it in a state that already carries the marker and quota.
func (b *Backend) provision(ctx context.Context, dsPath string, r backend.CreateRequest) (*truenas.Dataset, error) {
	ds, err := b.create(ctx, dsPath, r)
	if err != nil {
		return nil, err
	}
	// Recorded once, at birth, on both paths: a clone gets the identity of the
	// request that created it, never the one its origin carried.
	backend.RecordIdentity(ctx, b.c, dsPath, volume.IdentityFrom(r.Params))
	return ds, nil
}

// create makes the dataset, empty or cloned, already carrying the marker and
// the quota.
func (b *Backend) create(ctx context.Context, dsPath string, r backend.CreateRequest) (*truenas.Dataset, error) {
	if r.SourceSnapshot == "" {
		ds, err := b.c.DatasetCreate(ctx, truenas.DatasetSpec{
			Name:     dsPath,
			Type:     "FILESYSTEM",
			RefQuota: r.CapacityBytes,
			UserProperties: map[string]string{
				volume.OwnerProperty:    volume.OwnerValue,
				volume.OwnerIDProperty:  dsPath,
				volume.ProtocolProperty: "nfs",
			},
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create dataset %s: %v", dsPath, err)
		}
		return ds, nil
	}
	return b.restore(ctx, r.SourceSnapshot, dsPath, r.CapacityBytes)
}

// restore clones a snapshot and repairs everything the clone did not inherit.
//
// A ZFS clone takes its properties from its position in the hierarchy, not from
// its origin: without the explicit stamp the delete guard would refuse to remove
// the volume forever, and without refquota the pod would see the whole pool.
func (b *Backend) restore(ctx context.Context, snapshot, dsPath string, bytes int64) (*truenas.Dataset, error) {
	if err := b.c.SnapshotClone(ctx, snapshot, dsPath); err != nil {
		return nil, status.Errorf(codes.Internal, "clone snapshot %s into %s: %v", snapshot, dsPath, err)
	}
	fail := func(format string, args ...any) (*truenas.Dataset, error) {
		cause := status.Errorf(codes.Internal, format, args...)
		if delErr := b.c.DatasetDelete(ctx, dsPath, true, true); delErr != nil {
			return nil, status.Errorf(codes.Internal,
				"%v; removing the unmarked clone %s also failed: %v — it must be removed by hand",
				cause, dsPath, delErr)
		}
		return nil, cause
	}
	if err := b.c.SetUserProperty(ctx, dsPath, volume.OwnerIDProperty, dsPath); err != nil {
		return fail("stamp the owner id on clone %s: %v", dsPath, err)
	}
	if err := b.c.SetUserProperty(ctx, dsPath, volume.OwnerProperty, volume.OwnerValue); err != nil {
		return fail("stamp ownership on clone %s: %v", dsPath, err)
	}
	if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"refquota": bytes}); err != nil {
		return fail("set refquota on clone %s: %v", dsPath, err)
	}
	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return fail("query clone %s: %v", dsPath, err)
	}
	return ds, nil
}

// ensureShare exports the mountpoint, tolerating an export that already exists.
//
// The export is created FENCED: its host list holds nothing but DenyHost, so a
// volume nobody has published yet is reachable by nobody. Access arrives with
// ControllerPublishVolume and leaves with ControllerUnpublishVolume.
func (b *Backend) ensureShare(ctx context.Context, mountpoint string, p params) error {
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return status.Errorf(codes.Internal, "query NFS share for %s: %v", mountpoint, err)
	}
	if share != nil {
		return nil
	}
	if _, err := b.createShare(ctx, mountpoint, nil, p); err != nil {
		return status.Errorf(codes.Internal, "create NFS share for %s: %v", mountpoint, err)
	}
	return nil
}

// recordNetworkPolicy stores the operator's `networks` value on the dataset.
//
// It has to outlive this process: ControllerPublishVolume receives no
// StorageClass parameters, so a controller that restarted has no other way to
// learn which networks the operator was willing to export to.
func (b *Backend) recordNetworkPolicy(ctx context.Context, dsPath string, p params) error {
	if len(p.networks) == 0 {
		return nil
	}
	if err := b.c.SetUserProperty(ctx, dsPath, volume.NetworksProperty,
		strings.Join(p.networks, ",")); err != nil {
		return status.Errorf(codes.Internal, "recording the network policy on %s: %v", dsPath, err)
	}
	return nil
}

// Delete removes a volume, refusing anything this driver did not create.
func (b *Backend) Delete(ctx context.Context, id volume.ID) error {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil // already gone: CSI delete is idempotent
	}
	if err := verifyOwned(ds); err != nil {
		return err
	}

	mountpoint := mountpointOf(ds, dsPath)
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return status.Errorf(codes.Internal, "query NFS share for %s: %v", mountpoint, err)
	}
	if share != nil {
		if err := b.c.NFSShareDelete(ctx, share.ID); err != nil {
			return status.Errorf(codes.Internal, "delete NFS share %d: %v", share.ID, err)
		}
	}
	// The share is gone; only the dataset is left. Dispose destroys it, exactly
	// as this line always did, unless delete protection is on — in which case it
	// is renamed into the graveyard instead. The rename can only happen HERE,
	// after the export is removed: pool.dataset.rename performs no safety checks
	// and would happily leave a live export pointing at a path that no longer
	// exists.
	if err := retention.Dispose(ctx, b.c, b.retire, id, func(ctx context.Context) error {
		return b.c.DatasetDelete(ctx, dsPath, true, false)
	}); err != nil {
		return status.Errorf(codes.Internal, "delete dataset %s: %v", dsPath, err)
	}
	b.forget(id)
	return nil
}

// verifyOwned runs the ownership guard against a middleware dataset.
//
// The check is delegated to internal/volume rather than to Dataset.Owned so
// there is exactly one implementation of the rule, and so the refusal carries
// the reason — missing, wrong value, or merely INHERITED from the parent.
func verifyOwned(ds *truenas.Dataset) error {
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	return volume.VerifyOwned(view)
}

// Expand grows the volume's refquota.
//
// The shrink guard lives here because it cannot live anywhere else: the
// middleware silently accepts a refquota below current usage.
func (b *Backend) Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error) {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return 0, status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return 0, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	if err := verifyOwned(ds); err != nil {
		return 0, err
	}

	current := ds.RefQuota.Parsed
	if bytes < current {
		return 0, status.Errorf(codes.InvalidArgument,
			"cannot shrink volume %s from %d to %d bytes: TrueNAS would accept the smaller "+
				"refquota even below current usage, so the driver refuses it", id, current, bytes)
	}
	if bytes == current {
		return current, nil
	}
	if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"refquota": bytes}); err != nil {
		return 0, status.Errorf(codes.Internal, "set refquota on %s: %v", dsPath, err)
	}
	return bytes, nil
}

// PublishContext is what the node plugin needs to mount the export.
func (b *Backend) PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}

	b.mu.Lock()
	server := b.server
	version := b.versions[id.String()]
	b.mu.Unlock()
	if version == "" {
		version = defaultNFSVersion
	}
	if server == "" {
		// The address the node mounts from is a deployment fact the middleware
		// cannot answer for us, so it comes from the StorageClass. When this
		// process never saw the class — a restarted controller — the node falls
		// back to the value already recorded in the PV's volume context.
		// Fall back to the appliance's own address. Relying on a cache that a
		// controller restart empties would strand every existing volume.
		server = b.c.Host()
	}
	if server == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"no NFS server address is known for %s: set the %q StorageClass parameter", id, ParamServer)
	}
	return map[string]string{
		"server":     server,
		"share":      mountpointOf(ds, dsPath),
		"nfsVersion": version,
	}, nil
}

func (b *Backend) volumeFor(id volume.ID, bytes int64, mountpoint string, p params) *backend.Volume {
	ctxMap := map[string]string{
		"share":      mountpoint,
		"nfsVersion": p.nfsVersion,
	}
	if p.server != "" {
		ctxMap["server"] = p.server
	}
	return &backend.Volume{ID: id, CapacityBytes: bytes, Context: ctxMap}
}

func (b *Backend) remember(id volume.ID, p params) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.server != "" {
		b.server = p.server
	}
	b.versions[id.String()] = p.nfsVersion
}

func (b *Backend) forget(id volume.ID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.versions, id.String())
}

// ensure the interfaces stay satisfied even if either grows a method.
var (
	_ backend.Backend   = (*Backend)(nil)
	_ backend.Publisher = (*Backend)(nil)
)
