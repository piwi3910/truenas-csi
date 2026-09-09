package truenas

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
)

// DatasetSpec describes a dataset or zvol to create.
type DatasetSpec struct {
	Name           string
	Type           string // FILESYSTEM or VOLUME
	VolSize        int64
	RefQuota       int64
	Sparse         bool
	VolBlockSize   string
	ShareType      string // "SMB" gives the dataset an NFSv4 ACL
	UserProperties map[string]string
}

func (s DatasetSpec) payload() map[string]any {
	p := map[string]any{"name": s.Name, "type": s.Type}
	switch s.Type {
	case "VOLUME":
		p["volsize"] = s.VolSize
		p["sparse"] = s.Sparse
		if s.VolBlockSize != "" {
			p["volblocksize"] = s.VolBlockSize
		}
	default:
		if s.RefQuota > 0 {
			p["refquota"] = s.RefQuota
		}
	}
	if s.ShareType != "" {
		p["share_type"] = s.ShareType
	}
	if len(s.UserProperties) > 0 {
		props := make([]map[string]string, 0, len(s.UserProperties))
		for k, v := range s.UserProperties {
			props = append(props, map[string]string{"key": k, "value": v})
		}
		p["user_properties"] = props
	}
	return p
}

// MinRefQuotaBytes is the smallest refquota TrueNAS will accept on a dataset.
//
// Below it, pool.dataset.create fails its whole schema union and reports
// "[EINVAL] data.PoolDatasetCreateFilesystem.refquota.constrained-int: Input
// should be greater than or equal to 1073741824" alongside three unrelated
// complaints about the VOLUME variant it also tried. Nothing in that names the
// caller's mistake. Verified on 25.10.6: 1073741823 is refused and 1073741824
// is accepted; zvols have no equivalent floor and were created at 64 MiB.
const MinRefQuotaBytes = 1 << 30

// DatasetCreate creates a dataset or zvol.
//
// A missing PARENT is translated here rather than forwarded. Nothing verifies
// that the configured parentDataset exists — the appliance is only asked at the
// first provision — so a typo in it, or a pool imported without that dataset,
// surfaces as a failure on every PersistentVolumeClaim long after the deploy
// that caused it. FailedPrecondition with the path to create says what to do;
// the raw middleware text ("pool_dataset_create.name: Parent dataset (...) does
// not exist") does not, and arrived as a bare Internal error.
func (c *Ops) DatasetCreate(ctx context.Context, spec DatasetSpec) (*Dataset, error) {
	var ds Dataset
	if err := c.CallJSON(ctx, &ds, "pool.dataset.create", spec.payload()); err != nil {
		if IsParentMissing(err) {
			parent := spec.Name
			if i := strings.LastIndex(parent, "/"); i > 0 {
				parent = parent[:i]
			}
			return nil, withStatus(err, codes.FailedPrecondition,
				"cannot create %s: its parent dataset %s does not exist on the appliance. "+
					"The driver never creates the configured parentDataset — create it "+
					"(or correct the backend's pool/parentDataset) and retry.",
				spec.Name, parent)
		}
		return nil, err
	}
	return &ds, nil
}

// DatasetQuery returns a dataset, or (nil, nil) when it does not exist.
//
// Absence is deliberately NOT an error: CSI requires create and delete to be
// idempotent, and the middleware reports a missing dataset with errname EINVAL
// whose reason begins "[ENOENT]". Callers must establish state by querying
// rather than by classifying errors.
func (c *Ops) DatasetQuery(ctx context.Context, id string) (*Dataset, error) {
	var out []Dataset
	err := c.CallJSON(ctx, &out, "pool.dataset.query", []any{[]any{"id", "=", id}}, map[string]any{})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// DatasetList returns every dataset whose id is under the given prefix.
func (c *Ops) DatasetList(ctx context.Context, prefix string) ([]Dataset, error) {
	var out []Dataset
	err := c.CallJSON(ctx, &out, "pool.dataset.query",
		[]any{[]any{"id", "^", prefix}}, map[string]any{})
	if err != nil && !IsNotFound(err) {
		return nil, err
	}
	return out, nil
}

// DatasetUpdate patches a dataset.
func (c *Ops) DatasetUpdate(ctx context.Context, id string, patch map[string]any) (*Dataset, error) {
	var ds Dataset
	if err := c.CallJSON(ctx, &ds, "pool.dataset.update", id, patch); err != nil {
		return nil, err
	}
	return &ds, nil
}

// SetUserProperty stamps a user property on an existing dataset.
//
// Needed for clones: a ZFS clone inherits NEITHER the ownership marker NOR the
// quota from its origin, so a restored volume that is not stamped here can
// never be deleted by the driver's own ownership guard.
func (c *Ops) SetUserProperty(ctx context.Context, id, key, value string) error {
	_, err := c.DatasetUpdate(ctx, id, map[string]any{
		"user_properties_update": []map[string]string{{"key": key, "value": value}},
	})
	return err
}

// DatasetRename moves a dataset to newName, which is a FULL dataset path
// ("Pool0/k8s/.trash/entry"), not a leaf.
//
// The middleware's own words: "No safety checks are performed when renaming ZFS
// resources. If the dataset is in use by services such as SMB, iSCSI, snapshot
// tasks, replication, or cloud sync, renaming may cause disruptions or service
// failures." Read as a conditional safety check that would pass on an idle
// dataset, that argues for never forcing. It is not one: 25.10 refuses EVERY
// rename without force, including a dataset with no share, no extent and
// nothing holding it (verified against the appliance). It is a mandatory
// acknowledgement, so a caller that wants a rename at all has to pass true and
// earn its safety from the ORDER it does things in — see
// internal/retention.renameWhenReleased, which renames only after the share and
// extent teardown it depends on has completed.
//
// recursive is not exposed: it renames CHILD DATASETS, which a volume dataset
// does not have. Snapshots always travel with their dataset regardless.
func (c *Ops) DatasetRename(ctx context.Context, id, newName string, force bool) error {
	return c.CallJSON(ctx, nil, "pool.dataset.rename", id,
		map[string]any{"new_name": newName, "force": force})
}

// DatasetDelete removes a dataset. A dataset that is already gone is success.
// A destroy blocked by a dependent clone is translated here rather than
// forwarded. It is the ordinary end of the most ordinary snapshot workflow --
// restore a snapshot, check the copy, delete the original -- and ZFS refuses,
// because the restored volume is a clone of the original's snapshot. Forwarded
// raw it arrives as codes.Internal, which the CO retries for ever: the claim
// sits in Terminating with no indication of what is holding it, while the
// message quotes ZFS suggesting `-R`, which would destroy the restored volume.
func (c *Ops) DatasetDelete(ctx context.Context, id string, recursive, force bool) error {
	err := c.CallJSON(ctx, nil, "pool.dataset.delete", id,
		map[string]any{"recursive": recursive, "force": force})
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return nil
	}
	if IsHasDependentClones(err) {
		return withStatus(err, codes.FailedPrecondition,
			"%s cannot be deleted while %s: delete those first. "+
				"They were created from a snapshot of this volume, so ZFS keeps this "+
				"one alive to serve them",
			id, describeClones(c.clonesOf(ctx, id)))
	}
	return err
}

// clonesOf finds the datasets whose origin is a snapshot of id.
//
// It answers on a best effort: this runs only on an error path, and a failure
// to enumerate must not replace a clear refusal with a listing error.
func (c *Ops) clonesOf(ctx context.Context, id string) []string {
	pool := id
	if i := strings.Index(pool, "/"); i > 0 {
		pool = pool[:i]
	}
	all, err := c.DatasetList(ctx, pool)
	if err != nil {
		return nil
	}
	var out []string
	for i := range all {
		origin := all[i].Origin.Value
		if origin == "" {
			continue
		}
		if ds, _, found := strings.Cut(origin, "@"); found && ds == id {
			out = append(out, all[i].ID)
		}
	}
	sort.Strings(out)
	return out
}

// describeClones renders the dependants for the refusal message, and says so
// honestly when it could not name them.
func describeClones(clones []string) string {
	if len(clones) == 0 {
		return "volumes cloned from its snapshots still exist"
	}
	if len(clones) == 1 {
		return "volume " + clones[0] + " still exists"
	}
	return "volumes " + strings.Join(clones, ", ") + " still exist"
}

// RecommendedZvolBlocksize asks the appliance rather than guessing.
func (c *Ops) RecommendedZvolBlocksize(ctx context.Context, pool string) (string, error) {
	var s string
	if err := c.CallJSON(ctx, &s, "pool.dataset.recommended_zvol_blocksize", pool); err != nil {
		return "", err
	}
	return s, nil
}

// PoolQuery returns a pool by name.
func (c *Ops) PoolQuery(ctx context.Context, name string) (*Pool, error) {
	var out []Pool
	if err := c.CallJSON(ctx, &out, "pool.query",
		[]any{[]any{"name", "=", name}}, map[string]any{}); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pool %q does not exist", name)
	}
	return &out[0], nil
}

// SnapshotCreate takes a snapshot of a dataset.
func (c *Ops) SnapshotCreate(ctx context.Context, dataset, name string) (*Snapshot, error) {
	var s Snapshot
	if err := c.CallJSON(ctx, &s, "pool.snapshot.create",
		map[string]any{"dataset": dataset, "name": name}); err != nil {
		return nil, err
	}
	return &s, nil
}

// SnapshotCreateRecursive snapshots a dataset and every dataset beneath it in
// one call.
//
// This is the only crash-consistent primitive ZFS offers across several
// datasets: the recursive snapshot is taken in a single transaction group, so
// every child is captured at the same instant. Looping over the children with
// SnapshotCreate would produce as many transaction groups as datasets and
// therefore no consistency guarantee at all.
func (c *Ops) SnapshotCreateRecursive(ctx context.Context, dataset, name string) (*Snapshot, error) {
	var s Snapshot
	if err := c.CallJSON(ctx, &s, "pool.snapshot.create",
		map[string]any{"dataset": dataset, "name": name, "recursive": true}); err != nil {
		return nil, err
	}
	return &s, nil
}

// SnapshotQuery returns a snapshot, or (nil, nil) when absent.
func (c *Ops) SnapshotQuery(ctx context.Context, id string) (*Snapshot, error) {
	var out []Snapshot
	err := c.CallJSON(ctx, &out, "pool.snapshot.query",
		[]any{[]any{"id", "=", id}}, map[string]any{})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return &out[0], nil
}

// SnapshotList returns snapshots of datasets under a prefix.
func (c *Ops) SnapshotList(ctx context.Context, datasetPrefix string) ([]Snapshot, error) {
	var out []Snapshot
	err := c.CallJSON(ctx, &out, "pool.snapshot.query",
		[]any{[]any{"dataset", "^", datasetPrefix}}, map[string]any{})
	if err != nil && !IsNotFound(err) {
		return nil, err
	}
	return out, nil
}

// SnapshotDelete removes a snapshot; already absent is success.
func (c *Ops) SnapshotDelete(ctx context.Context, id string) error {
	err := c.CallJSON(ctx, nil, "pool.snapshot.delete", id)
	if err != nil && IsNotFound(err) {
		return nil
	}
	return err
}

// SnapshotClone creates a dataset from a snapshot, applying props to the clone
// as it is made.
//
// The clone is deliberately NOT promoted. Promoting does not free the source —
// it INVERTS the dependency, leaving the ORIGINAL volume undeletable, which is
// strictly worse than a snapshot that cannot be deleted while clones exist.
//
// props are raw ZFS property names and values, which is what makes them worth
// having: a clone inherits nothing its origin holds LOCALLY, and setting a
// property afterwards cannot always recover it. refreservation is the case in
// point — "auto" is legal here and asks ZFS itself for volsize plus this pool's
// metadata overhead, a figure that depends on pool geometry and that no caller
// can compute. pool.dataset.update rejects "auto" outright, and recomputes the
// reservation only when volsize CHANGES, so a same-size clone could never be
// repaired after the fact. Verified against a real appliance.
func (c *Ops) SnapshotClone(ctx context.Context, snapshot, dst string, props map[string]any) error {
	payload := map[string]any{"snapshot": snapshot, "dataset_dst": dst}
	if len(props) > 0 {
		payload["dataset_properties"] = props
	}
	return c.CallJSON(ctx, nil, "pool.snapshot.clone", payload)
}

// ErrDatasetBusy indicates dependent clones or an active mount.
var ErrDatasetBusy = errors.New("dataset is busy")
