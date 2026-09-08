// Package reconcile detects TrueNAS objects this driver created that no longer
// have a corresponding PersistentVolume.
package reconcile

import (
	"context"
	"sort"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// PVLister supplies the volume handles Kubernetes currently knows about.
type PVLister interface {
	// VolumeHandles returns the set of CSI volume handles for this driver.
	// It must return an error rather than a partial set: a partial view read
	// as complete would report live volumes as orphans.
	VolumeHandles(ctx context.Context) (map[string]struct{}, error)
}

// OrphanReconciler reports driver-owned datasets with no PersistentVolume.
//
// It NEVER deletes. An orphan is more often a symptom of a stale or incomplete
// PV listing than of a genuinely leaked volume, and deleting on that evidence
// would destroy live data. Reporting is the whole job.
type OrphanReconciler struct {
	reg      *backend.Registry
	lister   PVLister
	interval time.Duration
}

// NewOrphanReconciler builds the reconciler. A zero interval means 30 minutes.
func NewOrphanReconciler(reg *backend.Registry, lister PVLister, interval time.Duration) *OrphanReconciler {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	return &OrphanReconciler{reg: reg, lister: lister, interval: interval}
}

// RunOnce compares appliance state against Kubernetes and reports the strays.
func (o *OrphanReconciler) RunOnce(ctx context.Context) ([]string, error) {
	handles, err := o.lister.VolumeHandles(ctx)
	if err != nil {
		// A failed listing is not evidence of orphans; report nothing.
		return nil, err
	}

	var orphans []string
	names := o.reg.Names()
	sort.Strings(names)

	for _, name := range names {
		b, cfgErr := o.reg.Backend(name)
		if cfgErr != nil {
			continue
		}
		c, clErr := o.reg.Client(ctx, name)
		if clErr != nil {
			obs.Logger(ctx).Warn("orphan scan skipping unreachable backend",
				"backend", name, "error", obs.Redact(clErr.Error()))
			continue
		}
		prefix := b.Pool + "/" + b.ParentDataset + "/"
		datasets, dsErr := c.DatasetList(ctx, prefix)
		if dsErr != nil {
			return nil, dsErr
		}
		count := 0
		for i := range datasets {
			d := &datasets[i]
			// Only ever consider datasets this driver created. An inherited
			// marker belongs to somebody else's data.
			if !d.Owned(volume.OwnerProperty, volume.OwnerValue) {
				continue
			}
			// A namespace's parent dataset is driver-owned and has no
			// PersistentVolume by design; reporting it as an orphan would be a
			// permanent false positive on every scan.
			if volume.IsNamespaceDataset(d.LocalProperty(volume.NamespaceProperty)) {
				continue
			}
			leaf := d.ID[len(prefix):]
			fallback := "nfs"
			if d.Type == "VOLUME" {
				fallback = "iscsi"
			}
			proto := volume.ProtocolOr(d.LocalProperty(volume.ProtocolProperty), fallback)
			id, leafErr := volume.IDFromLeaf(name, proto, b.Pool, b.ParentDataset, leaf)
			if leafErr != nil {
				// Reporting a handle for a dataset at a depth this driver never
				// creates would name the wrong thing; an operator investigating
				// gets the dataset path in the log instead.
				obs.Logger(ctx).Warn("orphan scan skipping a driver-owned dataset at an unexpected depth",
					"dataset", d.ID, "error", leafErr)
				continue
			}
			if _, live := handles[id.String()]; live {
				continue
			}
			orphans = append(orphans, id.String())
			count++
			obs.Logger(ctx).Warn("orphaned volume: present on the appliance with no PersistentVolume",
				"volume_id", id.String(), "dataset", d.ID,
				"action", "reported only, nothing deleted")
		}
		obs.SetOrphanCount(name, count)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// Run reports orphans on the configured interval until the context ends.
func (o *OrphanReconciler) Run(ctx context.Context) {
	t := time.NewTicker(o.interval)
	defer t.Stop()
	for {
		if _, err := o.RunOnce(ctx); err != nil {
			obs.Logger(ctx).Warn("orphan scan failed", "error", obs.Redact(err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
