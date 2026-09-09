package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// ErrNamespaceDatasetConflict means the dataset a namespace would use already
// exists and is not one this driver created for that purpose.
//
// It is a refusal, not a recovery: adopting an operator's existing dataset
// would put driver-owned volumes inside it and put it on the reclamation path,
// where a later empty-namespace sweep would try to delete it.
var ErrNamespaceDatasetConflict = errors.New("namespace dataset exists but is not driver-owned")

// NamespaceDataset is the state of one namespace's parent dataset after
// EnsureNamespace has reconciled it.
type NamespaceDataset struct {
	// Path is the ZFS path, <pool>/<parent>/<namespace>.
	Path string

	// UsedBytes is ZFS `used` for the dataset: every volume beneath it, every
	// snapshot of those volumes, and every byte written into them. This is the
	// figure the quota is measured against, and it is the appliance's own,
	// which is what makes it stronger than a ledger the driver maintains.
	UsedBytes int64

	// ProvisionedBytes is the sum of what the namespace's volumes were
	// PROMISED — refquota for a filesystem, volsize for a zvol — regardless of
	// how much of it has been written.
	//
	// It exists because UsedBytes cannot bound thin volumes, which is the thing
	// this quota is for. A ZFS quota charges nothing for a 1 TiB refquota until
	// a byte is written into it, so measuring only usage lets a namespace with
	// a 2 GiB quota bind a hundred 1 GiB claims; every one of them succeeds,
	// and the tenant discovers the limit as write failures spread across
	// workloads that were already running. Verified on hardware: three 1 GiB
	// claims all bound under a 2 GiB quota before this was counted.
	ProvisionedBytes int64

	// QuotaBytes is the configured quota, 0 for unlimited.
	QuotaBytes int64

	// QuotaDeferred reports that QuotaBytes is below UsedBytes and was
	// therefore NOT applied to the dataset. See EnsureNamespace.
	QuotaDeferred bool
}

// Room returns how many more bytes may be provisioned into the namespace, and
// whether a limit applies at all.
//
// It measures against whichever of usage and provisioned size is LARGER, so
// both failure modes are covered by one number: a namespace whose volumes are
// full is bounded by UsedBytes, and a namespace that has promised more than it
// has written is bounded by ProvisionedBytes. Taking usage alone would let thin
// volumes past the ceiling; taking provisioned alone would miss bytes written
// by anything that did not provision through this driver — a restored
// replication stream, a snapshot growing, an operator copying data in over SSH.
func (n *NamespaceDataset) Room() (bytes int64, limited bool) {
	if n == nil || n.QuotaBytes <= 0 {
		return 0, false
	}
	charged := n.UsedBytes
	if n.ProvisionedBytes > charged {
		charged = n.ProvisionedBytes
	}
	if room := n.QuotaBytes - charged; room > 0 {
		return room, true
	}
	return 0, true
}

// Charged is the figure Room measured against, for error messages that have to
// explain which of the two limits was hit.
func (n *NamespaceDataset) Charged() int64 {
	if n == nil {
		return 0
	}
	if n.ProvisionedBytes > n.UsedBytes {
		return n.ProvisionedBytes
	}
	return n.UsedBytes
}

// provisionedUnder sums what the namespace's volumes were promised.
//
// Only the namespace's DIRECT children count, and only those this driver owns:
// a snapshot is not a volume, a nested dataset an operator made by hand is not
// this driver's to charge for, and the namespace dataset itself carries the
// quota rather than consuming it. A listing failure returns 0 rather than an
// error — losing the thin ceiling is better than refusing to provision at all
// when the appliance is merely slow to answer a secondary question.
func provisionedUnder(ctx context.Context, c truenas.API, path string) int64 {
	children, err := c.DatasetList(ctx, path+"/")
	if err != nil {
		obs.Logger(ctx).Warn("could not total the namespace's provisioned bytes; "+
			"the quota is measured against usage alone for this request",
			"dataset", path, "error", obs.Redact(err.Error()))
		return 0
	}
	var total int64
	for i := range children {
		d := &children[i]
		if strings.Contains(strings.TrimPrefix(d.ID, path+"/"), "/") {
			continue // deeper than a direct child
		}
		view := &volume.Dataset{ID: d.ID, UserProperties: map[string]volume.Property{}}
		for k, v := range d.UserProperties {
			view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
		}
		if volume.VerifyOwned(view) != nil {
			continue
		}
		switch {
		case d.VolSize.Parsed > 0:
			total += d.VolSize.Parsed
		case d.RefQuota.Parsed > 0:
			total += d.RefQuota.Parsed
		}
	}
	return total
}

// EnsureNamespace creates the namespace's parent dataset if it is missing and
// brings its ZFS quota into line with the configured one.
//
// Quota lowering is the interesting case. ZFS accepts a quota below current
// usage: existing data survives, but every further write into the dataset fails
// with EDQUOT — including writes by pods that were running happily a moment
// earlier, which turns an operator's accounting change into an outage in a
// namespace that was not over its OLD limit. So the driver does not apply it.
// It leaves the existing ZFS quota alone and reports QuotaDeferred, and because
// CreateVolume measures against the CONFIGURED quota rather than the applied
// one, no new volume is provisioned into the namespace until usage drops back
// under the new figure. The ceiling therefore binds immediately for new
// allocations, which is what the operator asked for, without failing writes for
// workloads that are already there. Raising a quota is applied immediately;
// there is nothing to protect against.
func EnsureNamespace(ctx context.Context, c truenas.API, pool, parent, namespace string, quota int64) (*NamespaceDataset, error) {
	if err := volume.ValidateNamespace(namespace); err != nil {
		return nil, err
	}
	path := pool + "/" + parent + "/" + namespace

	ds, err := c.DatasetQuery(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("querying namespace dataset %s: %w", path, err)
	}
	if ds == nil {
		ds, err = c.DatasetCreate(ctx, truenas.DatasetSpec{
			Name: path,
			Type: "FILESYSTEM",
			// No refquota: refquota bounds this dataset's own data and would
			// not constrain the volumes beneath it, which is the entire point.
			// The ceiling is the `quota` property, set below — the middleware's
			// create payload here has no field for it.
			UserProperties: volume.NamespaceProperties(namespace),
		})
		if err != nil {
			return nil, fmt.Errorf("creating namespace dataset %s: %w", path, err)
		}
		obs.Logger(ctx).Info("created namespace dataset", "dataset", path, "namespace", namespace)
	} else if err := verifyNamespaceDataset(ds, namespace); err != nil {
		return nil, err
	}

	out := &NamespaceDataset{Path: path, UsedBytes: ds.Used.Parsed, QuotaBytes: quota}
	if quota > 0 {
		// Only worth the listing when there is a ceiling to hit.
		out.ProvisionedBytes = provisionedUnder(ctx, c, path)
	}

	if quota > 0 && ds.Used.Parsed > quota {
		out.QuotaDeferred = true
		obs.Logger(ctx).Warn("namespace quota is below current usage and was not applied to ZFS; "+
			"existing data and running workloads are untouched, but no new volume will be provisioned here "+
			"until usage falls below the quota",
			"namespace", namespace, "dataset", path, "used", ds.Used.Parsed, "quota", quota)
		return out, nil
	}

	// The recorded value is what lets the common path — quota unchanged since
	// the last CreateVolume — cost a query and no write at all.
	applied, recorded := volume.AppliedQuota(ds.LocalProperty(volume.NamespaceQuotaProperty))
	if recorded && applied == quota {
		return out, nil
	}
	if err := applyNamespaceQuota(ctx, c, path, quota); err != nil {
		return nil, err
	}
	obs.Logger(ctx).Info("applied namespace quota", "namespace", namespace, "dataset", path, "quota", quota)
	return out, nil
}

// applyNamespaceQuota sets the ZFS quota property and records what was set.
//
// UNVERIFIED: `pool.dataset.update` accepting a `quota` of 0 to clear the
// property is taken from the middleware's documented dataset schema at
// https://192.168.10.253/api/docs/current/ and has not been exercised against
// hardware. The set path (a positive byte count) matches how refquota is
// already set by the volume backends.
func applyNamespaceQuota(ctx context.Context, c truenas.API, path string, quota int64) error {
	if _, err := c.DatasetUpdate(ctx, path, map[string]any{"quota": quota}); err != nil {
		return fmt.Errorf("setting quota %d on %s: %w", quota, path, err)
	}
	if err := c.SetUserProperty(ctx, path, volume.NamespaceQuotaProperty, volume.FormatQuota(quota)); err != nil {
		// The quota is in force; only the bookkeeping failed. Refusing the
		// volume over that would be worse than re-applying an identical quota
		// on the next CreateVolume, which is all this costs.
		obs.Logger(ctx).Warn("namespace quota applied but not recorded; it will be re-applied on the next create",
			"dataset", path, "error", obs.Redact(err.Error()))
	}
	return nil
}

// ReclaimNamespace deletes a namespace's parent dataset once the last volume in
// it has gone, reporting whether it deleted anything.
//
// Every refusal here is deliberate and none of them is an error the caller
// should act on:
//
//   - the dataset is gone already — nothing to do;
//   - it is not driver-owned, or carries no namespace marker — it is somebody
//     else's dataset that happens to sit at this path, and the ownership
//     discipline in internal/volume says we do not touch it;
//   - it still has children — a volume, or a dataset a human put there.
//
// The delete is non-recursive and non-forced for the same reason: if ZFS says
// the dataset is not empty, that is a fact the driver must respect rather than
// override, and a `force` delete is how a driver destroys data it never
// created.
func ReclaimNamespace(ctx context.Context, c truenas.API, pool, parent, namespace string) (bool, error) {
	if err := volume.ValidateNamespace(namespace); err != nil {
		return false, err
	}
	path := pool + "/" + parent + "/" + namespace

	ds, err := c.DatasetQuery(ctx, path)
	if err != nil {
		return false, fmt.Errorf("querying namespace dataset %s: %w", path, err)
	}
	if ds == nil {
		return false, nil
	}
	if err := verifyNamespaceDataset(ds, namespace); err != nil {
		return false, err
	}

	children, err := c.DatasetList(ctx, path+"/")
	if err != nil {
		return false, fmt.Errorf("listing %s: %w", path, err)
	}
	if len(children) > 0 {
		return false, nil
	}
	if err := c.DatasetDelete(ctx, path, false, false); err != nil {
		return false, fmt.Errorf("deleting empty namespace dataset %s: %w", path, err)
	}
	obs.Logger(ctx).Info("reclaimed empty namespace dataset", "dataset", path, "namespace", namespace)
	return true, nil
}

// verifyNamespaceDataset checks that an existing dataset is one this driver
// created for exactly this namespace.
//
// Both halves matter. VerifyOwned (with its LOCAL-source rule) is what stops
// the driver adopting, filling and later deleting an operator's dataset that
// merely inherited the marker. The namespace marker on top of it is what stops
// a VOLUME dataset — also driver-owned — from being mistaken for a container
// and having volumes created inside it.
func verifyNamespaceDataset(ds *truenas.Dataset, namespace string) error {
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	if err := volume.VerifyOwned(view); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrNamespaceDatasetConflict, ds.ID, err)
	}
	got := ds.LocalProperty(volume.NamespaceProperty)
	if got == "" {
		return fmt.Errorf("%w: %s has no %s property — it is a volume or a hand-made dataset, not a namespace container",
			ErrNamespaceDatasetConflict, ds.ID, volume.NamespaceProperty)
	}
	if got != namespace {
		return fmt.Errorf("%w: %s accounts for namespace %q, not %q",
			ErrNamespaceDatasetConflict, ds.ID, got, namespace)
	}
	return nil
}
