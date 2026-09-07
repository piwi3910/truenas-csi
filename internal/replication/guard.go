package replication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// ErrForbiddenCall is returned when the test-failover guard blocks a call.
//
// It exists so that "a test failover cannot promote the real target" is an
// enforced property of the code path rather than a promise about it: the guard
// sits between the test-failover logic and the middleware, and no future edit
// to that logic can reach production without also removing the guard.
var ErrForbiddenCall = errors.New("call is not permitted during a test failover")

// scratchGuard wraps a middleware client and permits only the calls a test
// failover legitimately needs, restricted to datasets under one scratch prefix.
type scratchGuard struct {
	inner  caller
	prefix string // e.g. "Pool0/k8s/csi-testfailover-group1"
}

func newScratchGuard(inner caller, prefix string) *scratchGuard {
	return &scratchGuard{inner: inner, prefix: prefix}
}

// CallJSON forwards a permitted call and refuses everything else.
func (g *scratchGuard) CallJSON(ctx context.Context, out any, method string, params ...any) error {
	if err := g.permit(method, params); err != nil {
		return err
	}
	return g.inner.CallJSON(ctx, out, method, params...)
}

func (g *scratchGuard) permit(method string, params []any) error {
	switch method {
	// Read-only calls are always safe.
	case mDatasetQuery, mSnapshotQuery, mReplicationQuery, mSnapshotTaskQuery:
		return nil

	// Cloning a replicated snapshot is the whole point of a test failover, but
	// the clone must land inside the scratch tree.
	case mSnapshotClone:
		p, _ := firstMap(params)
		dst, _ := p["dataset_dst"].(string)
		return g.requireScratch(method, dst)

	case mDatasetCreate:
		p, _ := firstMap(params)
		name, _ := p["name"].(string)
		return g.requireScratch(method, name)

	// Writing to a dataset is allowed only inside the scratch tree. This is what
	// stops a test failover clearing readonly on the production target.
	case mDatasetUpdate, mDatasetDelete:
		id, _ := firstString(params)
		return g.requireScratch(method, id)

	// Promotion, and every mutation of the production replication or snapshot
	// tasks, is categorically refused: a test failover that promoted the target
	// would turn a rehearsal into an unplanned, unrecorded failover.
	default:
		return fmt.Errorf("%w: %s", ErrForbiddenCall, method)
	}
}

func (g *scratchGuard) requireScratch(method, id string) error {
	if id == "" {
		return fmt.Errorf("%w: %s without a dataset", ErrForbiddenCall, method)
	}
	if id != g.prefix && !strings.HasPrefix(id, g.prefix+"/") {
		return fmt.Errorf("%w: %s targets %q, which is outside the scratch dataset %q",
			ErrForbiddenCall, method, id, g.prefix)
	}
	return nil
}

func firstMap(params []any) (map[string]any, bool) {
	if len(params) == 0 {
		return nil, false
	}
	m, ok := params[0].(map[string]any)
	return m, ok
}

func firstString(params []any) (string, bool) {
	if len(params) == 0 {
		return "", false
	}
	s, ok := params[0].(string)
	return s, ok
}

// asVolumeDataset converts a middleware dataset into the shape the ownership
// guard in internal/volume expects. The guard is deliberately not duplicated
// here: replication must refuse exactly what provisioning refuses.
func asVolumeDataset(ds *truenas.Dataset) *volume.Dataset {
	if ds == nil {
		return nil
	}
	out := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		out.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	return out
}

// verifyOwned fetches a dataset and refuses it unless this driver created it.
//
// Absence is an error here, unlike in DeleteVolume: replication decisions are
// made about datasets that must exist, and treating "gone" as "fine" would let
// a group silently target a path someone else is about to create.
func verifyOwned(ctx context.Context, c caller, id string) (*truenas.Dataset, error) {
	ds, err := queryDataset(ctx, c, id)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		return nil, fmt.Errorf("%w: %q does not exist", volume.ErrNotManaged, id)
	}
	if err := volume.VerifyOwned(asVolumeDataset(ds)); err != nil {
		return nil, err
	}
	return ds, nil
}

// deleteIfOwned destroys a dataset only after proving this driver created it.
// A dataset that is already gone is success; a foreign dataset is refused and
// nothing is deleted.
func deleteIfOwned(ctx context.Context, c caller, id string) error {
	ds, err := queryDataset(ctx, c, id)
	if err != nil {
		return err
	}
	if ds == nil {
		return nil
	}
	if err := volume.VerifyOwned(asVolumeDataset(ds)); err != nil {
		return err
	}
	return deleteDataset(ctx, c, id)
}
