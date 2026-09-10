package csi

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ControllerModifyVolume changes ZFS properties on a live volume.
//
// This is the CSI MODIFY_VOLUME capability, driven by a VolumeAttributesClass
// on a PersistentVolumeClaim. It exists because ZFS genuinely has properties
// worth changing on a volume that is mounted and in use: the driver's own
// benchmark measures 337 4 KiB write IOPS at 94.6 ms over NFS against 45,155 at
// 0.71 ms over iSCSI, and almost all of that gap is a RAIDZ2 pool with no SLOG
// honouring synchronous writes. `sync` is a per-dataset property, it can be
// changed while the volume is mounted, and it takes effect on the next write.
// So an operator can trade durability for two orders of magnitude of write
// throughput on ONE claim, and change their mind afterwards.
//
// What this call must not become is a general "set any ZFS property" endpoint.
// A VolumeAttributesClass is a cluster-scoped object whose parameters are
// chosen by whoever may create one — a different, usually wider, set of people
// than those who may edit a StorageClass, and a different set again from those
// who own the pool. Handing that map to pool.dataset.update unfiltered would
// let a VolumeAttributesClass set `quota` and resize a volume behind the
// resizer's back, set `readonly=on` and break every pod using it, or set
// `mountpoint` and detach the data from its share. The allowlist below is
// therefore a closed set, and everything outside it is refused by name.
func (c *controller) ControllerModifyVolume(ctx context.Context, req *csipb.ControllerModifyVolumeRequest) (resp *csipb.ControllerModifyVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerModifyVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	// Parsed before anything is looked up, so a request naming a property this
	// driver refuses never reaches the appliance at all.
	mod, err := parseModification(req.GetMutableParameters())
	if err != nil {
		return nil, err
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	// Looked up even when there is nothing to apply: the spec requires NotFound
	// for a volume that is gone, and only a query can establish that.
	ds, err := c.volumeDataset(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := c.applyModification(ctx, id, ds, mod); err != nil {
		return nil, err
	}
	return &csipb.ControllerModifyVolumeResponse{}, nil
}

// mutableProperty is one ZFS property a VolumeAttributesClass may set, together
// with everything the driver needs in order to say no.
type mutableProperty struct {
	// name is the ZFS property name, which is also the middleware field name on
	// pool.dataset.update and the key a VolumeAttributesClass writes.
	name string
	// filesystemOnly marks a property that does not exist on a zvol. Refusing
	// these here rather than letting the appliance refuse them is the
	// difference between InvalidArgument ("fix your VolumeAttributesClass") and
	// Internal ("retry"), and only the first is true.
	filesystemOnly bool
	// values is the closed set of values the middleware accepts, uppercase as
	// pool.dataset.update declares them. Verified against the appliance's own
	// schema at https://192.168.10.253/api/docs/current/ (pool.dataset.update).
	values map[string]bool
	// why is the one-line rationale, rendered into the refusal for a bad value
	// so an operator sees the trade rather than a bare enum dump.
	why string
}

// enum builds a value set from the middleware's uppercase spellings.
func enum(vs ...string) map[string]bool {
	out := make(map[string]bool, len(vs))
	for _, v := range vs {
		out[v] = true
	}
	return out
}

// inheritValue resets a property to whatever the parent dataset says. It is
// accepted for every allowlisted property because it is the only way to undo a
// modification without guessing what the site's default was.
const inheritValue = "INHERIT"

// compressionValues is the compression enum pool.dataset.update declares, in
// the appliance's own spelling. It is stated in full rather than pattern-matched
// because a pattern would accept spellings the middleware rejects, turning an
// operator's typo into an Internal error instead of an InvalidArgument.
var compressionValues = func() map[string]bool {
	vs := []string{"ON", "OFF", "LZ4", "GZIP", "GZIP-1", "GZIP-9", "ZSTD", "ZSTD-FAST",
		"ZLE", "LZJB", inheritValue}
	for i := 1; i <= 19; i++ {
		vs = append(vs, fmt.Sprintf("ZSTD-%d", i))
	}
	for _, n := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 20, 30, 40, 50, 60, 70, 80, 90,
		100, 500, 1000} {
		vs = append(vs, fmt.Sprintf("ZSTD-FAST-%d", n))
	}
	return enum(vs...)
}()

// mutableProperties is the whole allowlist: the ZFS properties a
// VolumeAttributesClass may change on a volume this driver owns.
//
// Every entry meets all four of these, and an addition must too:
//
//  1. It can be changed while the volume is mounted and in use.
//  2. It affects FUTURE writes only, so applying it cannot corrupt or lose data
//     that is already on the volume.
//  3. It says nothing about capacity. Capacity belongs to
//     ControllerExpandVolume alone — two code paths writing a volume's size
//     will eventually disagree, and the one that loses silently resizes a
//     volume the CO believes is a different size. That is why `quota`,
//     `refquota`, `reservation`, `refreservation` and `volsize` are absent.
//  4. The appliance actually accepts it on pool.dataset.update. `primarycache`
//     and `logbias` are obvious candidates on paper and are NOT here for
//     exactly this reason: TrueNAS 25.10's pool.dataset.update schema declares
//     "no additional properties" and lists neither, so allowlisting them would
//     only produce guaranteed appliance rejections.
//
// Deliberately excluded beyond those: `volblocksize` (immutable after a zvol is
// created — ZFS will not change it and pool.dataset.update does not offer it),
// `readonly` (breaks every pod writing to the volume, with no way for the pod
// to find out except an I/O error), `mountpoint` (detaches the data from the
// share serving it), and `deduplication` (a pool-wide memory commitment that
// one claim must not be able to make on the operator's behalf).
var mutableProperties = []mutableProperty{{
	name:   "sync",
	values: enum("STANDARD", "ALWAYS", "DISABLED", inheritValue),
	why: "synchronous write behaviour; DISABLED trades durability across a power " +
		"failure for write throughput",
}, {
	name:   "compression",
	values: compressionValues,
	why:    "compression algorithm applied to blocks written from now on",
}, {
	name:           "atime",
	filesystemOnly: true,
	values:         enum("ON", "OFF", inheritValue),
	why: "whether reading a file writes back its access time; a zvol has no " +
		"filenames and therefore no atime",
}, {
	name:           "recordsize",
	filesystemOnly: true,
	// Every power of two from 512 to 16M. recordsize is the one modifiable
	// property pool.dataset.update declares no enum for — its schema is a bare
	// string — so this set is stated here, and it stopped at 1M while the
	// appliance went to 16M. Measured on 25.10.6: 2M, 4M, 8M and 16M are all
	// accepted and reported back verbatim, 256 and 32M are refused as "an
	// invalid recordsize". An operator asking for a large recordsize, the usual
	// choice for big sequential files, was refused by the DRIVER rather than by
	// ZFS.
	values: enum("512", "1K", "2K", "4K", "8K", "16K", "32K", "64K", "128K", "256K",
		"512K", "1M", "2M", "4M", "8M", "16M", inheritValue),
	why: "the block size ZFS uses for files written from now on; the zvol " +
		"equivalent is volblocksize, which cannot be changed after creation",
}}

// syncDisabled is the one value in the allowlist that can lose acknowledged
// writes. It is permitted — an informed operator is entitled to make that trade
// — but never silently; see warn below.
const syncDisabled = "DISABLED"

// modification is a validated, normalised set of property changes: every key is
// on the allowlist and every value is one the middleware accepts.
type modification struct {
	// props maps a mutableProperty to the requested value, in the middleware's
	// uppercase spelling.
	props map[string]string
}

// empty reports whether there is nothing to apply.
func (m modification) empty() bool { return len(m.props) == 0 }

// parseModification validates a VolumeAttributesClass's mutable parameters
// against the allowlist and returns them normalised.
//
// It is the security boundary of this feature, so it fails closed twice over:
// an unknown KEY is refused by name, and a known key with an unknown VALUE is
// refused too rather than forwarded for the appliance to judge. Both are
// InvalidArgument, because both are a fault in the VolumeAttributesClass that
// no amount of retrying will fix.
//
// An empty map is a successful no-op. A VolumeAttributesClass with no
// parameters is a legal object, and failing it would leave the CO retrying
// something that cannot be repaired by retrying.
func parseModification(mutable map[string]string) (modification, error) {
	mod := modification{}
	if len(mutable) == 0 {
		return mod, nil
	}
	mod.props = make(map[string]string, len(mutable))
	// Sorted so that a request carrying several bad keys always names the same
	// one; map order would otherwise make the error message flap between
	// retries and look like a different fault each time.
	for _, key := range sortedKeys(mutable) {
		p, ok := lookupMutableProperty(key)
		if !ok {
			return modification{}, status.Errorf(codes.InvalidArgument,
				"volume attribute %q is not one this driver will change; supported: %s",
				key, strings.Join(mutablePropertyNames(), ", "))
		}
		// ZFS spells its values lowercase and the middleware demands uppercase,
		// so a VolumeAttributesClass written the natural way (sync: disabled)
		// has to be accepted and normalised rather than rejected as a typo.
		want := strings.ToUpper(strings.TrimSpace(mutable[key]))
		if !p.values[want] {
			return modification{}, status.Errorf(codes.InvalidArgument,
				"volume attribute %q does not accept the value %q (%s); accepted: %s",
				key, mutable[key], p.why, strings.Join(sortedSet(p.values), ", "))
		}
		mod.props[key] = want
	}
	return mod, nil
}

// applyModification writes a validated modification onto a dataset.
//
// The dataset is passed in rather than queried here so the caller keeps the
// NotFound decision, and so the current values are read once: idempotency is
// decided by comparing against them, not by writing and hoping.
func (c *controller) applyModification(ctx context.Context, id volume.ID,
	ds *truenas.Dataset, mod modification) error {
	if mod.empty() {
		return nil
	}
	// Never modify a dataset that is not ours. Every mutating path in this
	// driver checks the io.truenas.csi:managed marker with source LOCAL first,
	// and a volume id is attacker-supplied text: without this, a
	// VolumeAttributesClass plus a hand-written PersistentVolume would rewrite
	// properties on the operator's own datasets.
	if err := requireOwned(ds); err != nil {
		return err
	}

	// Warned on the REQUEST, not on the change, so an operator re-applying the
	// same class still sees it. Losing acknowledged writes on a power failure
	// is a legitimate choice and this call does not refuse it — but it must
	// never be a quiet one.
	if mod.props["sync"] == syncDisabled {
		obs.Logger(ctx).Warn("sync=disabled requested for volume: writes will be acknowledged "+
			"before they reach stable storage, so a power failure or appliance crash can lose "+
			"data this volume has already told the application was written",
			"dataset", id.DatasetPath())
	}

	patch, err := mod.patch(ds)
	if err != nil {
		return err
	}
	// CSI retries this call, so applying the same class twice must succeed and
	// change nothing. Everything already at the requested value drops out of
	// the patch above, and an empty patch means the appliance is not touched.
	if len(patch) == 0 {
		obs.Logger(ctx).Debug("volume attributes already match; nothing to change")
		return nil
	}
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return toStatus(err)
	}
	if _, err := cl.DatasetUpdate(ctx, id.DatasetPath(), patch); err != nil {
		// Internal, never InvalidArgument: the driver understood and accepted
		// every key and value, so a refusal here is a condition on the
		// appliance — and the code decides whether the CO retries. Calling this
		// InvalidArgument would make the CO give up on a change that would
		// succeed once the pool is importable again.
		return status.Errorf(codes.Internal, "setting %s on %s: %v",
			strings.Join(patchKeys(patch), ", "), id.DatasetPath(), obs.Redact(err.Error()))
	}
	obs.Logger(ctx).Info("volume attributes changed",
		"properties", strings.Join(patchKeys(patch), ","))
	return nil
}

// patch reduces a modification to the pool.dataset.update payload for one
// dataset: the properties that apply to this KIND of dataset, minus the ones
// already at the requested value.
func (m modification) patch(ds *truenas.Dataset) (map[string]any, error) {
	out := map[string]any{}
	for _, key := range sortedKeys(m.props) {
		p, _ := lookupMutableProperty(key)
		// A zvol is a block device with no filenames in it, so the properties
		// ZFS defines over a namespace do not exist there. The appliance would
		// reject these too, but with an Internal-shaped failure that tells the
		// CO to retry forever; refusing here says what is wrong instead.
		if p.filesystemOnly && ds.Type == "VOLUME" {
			return nil, status.Errorf(codes.InvalidArgument,
				"volume attribute %q cannot be set on %s: it is a zvol, and %s applies only "+
					"to filesystem datasets (%s)", key, ds.ID, key, p.why)
		}
		if propertyMatches(ds, key, m.props[key]) {
			continue
		}
		out[key] = m.props[key]
	}
	return out, nil
}

// propertyMatches reports whether a dataset is already at the requested value.
//
// INHERIT is the case a plain string comparison gets wrong: it does not name a
// value at all, it asks for the property to follow the parent. So it is
// satisfied by the SOURCE — anything but LOCAL — rather than by the value, and
// a dataset already inheriting is left alone.
func propertyMatches(ds *truenas.Dataset, key, want string) bool {
	cur, known := ds.ZFSProperty(key)
	if !known {
		return false
	}
	if want == inheritValue {
		return cur.Source != "" && cur.Source != "LOCAL"
	}
	// The middleware reports values in ZFS's lowercase spelling and accepts
	// them in uppercase, so the comparison has to ignore case or every change
	// would look necessary and no request would ever be a no-op.
	return strings.EqualFold(cur.Value, want)
}

// requireOwned refuses a dataset this driver did not create, with the reason.
//
// The check is delegated to internal/volume so there is exactly one
// implementation of the ownership rule in the driver, and so the refusal
// distinguishes a missing marker from one merely INHERITED from the parent
// dataset — which is the case that would otherwise clear the driver to rewrite
// properties on an operator's pre-existing data.
func requireOwned(ds *truenas.Dataset) error {
	if ds == nil {
		return status.Error(codes.NotFound, "volume does not exist")
	}
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	if err := volume.VerifyOwned(view); err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}
	return nil
}

// lookupMutableProperty finds an allowlisted property by name.
func lookupMutableProperty(name string) (mutableProperty, bool) {
	for _, p := range mutableProperties {
		if p.name == name {
			return p, true
		}
	}
	return mutableProperty{}, false
}

// mutablePropertyNames lists the allowlist for an error message.
func mutablePropertyNames() []string {
	out := make([]string, 0, len(mutableProperties))
	for _, p := range mutableProperties {
		out = append(out, p.name)
	}
	sort.Strings(out)
	return out
}

// patchKeys lists the properties a patch changes, in a stable order.
func patchKeys(patch map[string]any) []string {
	out := make([]string, 0, len(patch))
	for k := range patch {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns a map's keys in a stable order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedSet returns a set's members in a stable order.
func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
