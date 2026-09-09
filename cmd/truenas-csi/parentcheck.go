package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
)

// parentCheckTimeout bounds the whole check. It runs on the startup path, and a
// slow appliance must delay the controller by a bounded amount or not at all.
const parentCheckTimeout = 20 * time.Second

// verifyParentDatasets logs, once at startup, whether each backend's configured
// pool and parentDataset actually exist on its appliance.
//
// Nothing else ever asks. The appliance is first consulted at the first
// CreateVolume, so a mis-typed parentDataset — or a pool imported without that
// dataset — produces a driver that starts cleanly, reports healthy, brings
// every sidecar up green and validates its StorageClasses, and then fails every
// single PersistentVolumeClaim. The operator's mistake and its symptom are a
// deploy apart.
//
// It only WARNS. Failing startup would turn an appliance that happens to be
// unreachable at boot into a crash loop, and a driver with three healthy
// backends and one typo should still serve the three. The one thing it must not
// do is stay quiet.
func verifyParentDatasets(ctx context.Context, reg *backend.Registry) {
	ctx, cancel := context.WithTimeout(ctx, parentCheckTimeout)
	defer cancel()

	for _, name := range reg.Names() {
		b, err := reg.Backend(name)
		if err != nil {
			continue
		}
		c, err := reg.Client(ctx, name)
		if err != nil {
			slog.Warn("could not verify the backend's parent dataset: the appliance "+
				"is not reachable right now. Provisioning will report the real error",
				"backend", name, "error", obs.Redact(err.Error()))
			continue
		}
		parent := b.Pool + "/" + b.ParentDataset
		ds, err := c.DatasetQuery(ctx, parent)
		if err != nil {
			slog.Warn("could not verify the backend's parent dataset",
				"backend", name, "dataset", parent, "error", obs.Redact(err.Error()))
			continue
		}
		if ds == nil {
			slog.Error("the backend's parent dataset DOES NOT EXIST: every volume "+
				"on this backend will fail to provision until it is created. The "+
				"driver never creates it — create the dataset, or correct the "+
				"backend's pool and parentDataset",
				"backend", name, "dataset", parent,
				"pool", b.Pool, "parent_dataset", b.ParentDataset)
			continue
		}
		slog.Info("backend parent dataset verified",
			"backend", name, "dataset", parent)
	}
}
