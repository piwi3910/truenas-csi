package retention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// ErrNotReapable means a dataset failed at least one of the four preconditions
// and must not be destroyed.
var ErrNotReapable = errors.New("dataset is not reapable")

// Reapable answers the only question that may ever precede a destroy, and it
// answers it from the dataset the appliance just returned rather than from
// anything the driver remembers.
//
// FOUR preconditions, checked every time, all of them:
//
//  1. INSIDE THE GRAVEYARD — a direct child of <pool>/<parent>/<graveyard>, and
//     not the graveyard itself. Prefix and depth both, because the destroy is
//     recursive and a match one level too deep would take siblings with it.
//  2. DRIVER-OWNED — io.truenas.csi:managed == truenas-csi with source LOCAL.
//     The source rule is not decoration: ZFS user properties are inherited, so
//     the graveyard's own marker reaches everything beneath it, and a
//     presence-only check would clear a dataset a human dropped in there for
//     destruction.
//  3. CARRIES A DELETION TIMESTAMP — a LOCAL, parsable io.truenas.csi:deleted-at.
//     No timestamp means the driver has no idea when the grace period started,
//     and "no idea" is never "expired".
//  4. PAST ITS GRACE PERIOD — now - deletedAt >= grace, with grace > 0.
//
// It returns an error naming the precondition that failed, so a refusal is
// something an operator can read rather than a silent skip.
func Reapable(ds *truenas.Dataset, p Policy, now time.Time) error {
	if !p.On() {
		return fmt.Errorf("%w: delete protection is off", ErrNotReapable)
	}
	if ds == nil {
		return fmt.Errorf("%w: no dataset", ErrNotReapable)
	}

	// 1. Inside the graveyard.
	if err := p.ConfineToGraveyard(ds.ID); err != nil {
		return fmt.Errorf("%w: %w", ErrNotReapable, err)
	}

	// 2. Driver-owned, LOCALly.
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	if err := volume.VerifyOwned(view); err != nil {
		return fmt.Errorf("%w: %w", ErrNotReapable, err)
	}
	// The graveyard root must never be reaped — destroying it recursively would
	// take every retired volume with it — and ConfineToGraveyard already refuses
	// it by PATH, exactly and by construction.
	//
	// It is deliberately NOT also refused by the graveyard marker, which looks
	// like free defence in depth and is in fact a bug. Verified against 25.10:
	// ZFS inherits user properties to children, and TrueNAS reports an INHERITED
	// user property with source "LOCAL", indistinguishable from one set on the
	// dataset itself. So every dataset renamed into the graveyard inherits the
	// root's marker and reads as carrying it locally — and a marker check here
	// refused every retired volume for ever, which is a feature whose whole
	// purpose is bounded retention silently becoming unbounded.
	//
	// The same fact limits what the ownership check above can promise; see
	// .procoder/notes/truenas-api-findings.md.

	// 3. Carries a deletion timestamp.
	deletedAt, err := volume.ParseDeletedAt(ds.LocalProperty(volume.DeletedAtProperty))
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrNotReapable, ds.ID, err)
	}

	// 4. Past its grace period. A timestamp in the future — a clock that went
	// backwards, an operator editing the property — reads as "not yet", which
	// is the direction that keeps the data.
	if age := now.UTC().Sub(deletedAt); age < p.Grace {
		return fmt.Errorf("%w: %q was retired %s ago and the grace period is %s: %s left",
			ErrNotReapable, ds.ID, age.Round(time.Second), p.Grace, (p.Grace - age).Round(time.Second))
	}
	return nil
}

// Reaper destroys graveyard datasets whose grace period has expired.
//
// It is deliberately a thing of its own rather than a mode of
// internal/reconcile. That reconciler is report-only, and correctly so — an
// apparent orphan is more often a stale PersistentVolume listing than a leak —
// and giving it the power to destroy would put that judgement one flag away
// from being wrong. The reaper destroys only what the driver ITSELF renamed
// into a dataset it created, after a delay it recorded on the object.
type Reaper struct {
	// Backends supplies one appliance client and its policy per sweep. It is a
	// function rather than a registry so this package does not depend on
	// internal/backend, which depends on it.
	Backends func(ctx context.Context) []Target

	// Now is the clock. Tests set it; zero means time.Now.
	Now Clock
}

// Target is one appliance the reaper sweeps.
type Target struct {
	// Name is the backend name, for logs.
	Name string
	// Client is the appliance connection.
	Client truenas.API
	// Policy is that appliance's resolved retention policy.
	Policy Policy
}

// NewReaper builds a reaper over the given targets.
func NewReaper(targets func(ctx context.Context) []Target) *Reaper {
	return &Reaper{Backends: targets, Now: time.Now}
}

// RunOnce sweeps every target once and returns the datasets it destroyed.
//
// It never returns an error for a single failed destroy: one busy dataset, or
// one appliance that is down, must not stop the others from being reclaimed,
// and every failure is retried on the next sweep anyway.
func (r *Reaper) RunOnce(ctx context.Context) []string {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	var destroyed []string
	for _, t := range r.Backends(ctx) {
		destroyed = append(destroyed, r.sweep(ctx, t, now())...)
	}
	sort.Strings(destroyed)
	return destroyed
}

func (r *Reaper) sweep(ctx context.Context, t Target, now time.Time) []string {
	if !t.Policy.On() {
		return nil
	}
	log := obs.Logger(ctx)
	datasets, err := t.Client.DatasetList(ctx, t.Policy.prefix())
	if err != nil {
		log.Warn("reaper could not list the graveyard; nothing was destroyed",
			"backend", t.Name, "graveyard", t.Policy.Root(), "error", obs.Redact(err.Error()))
		return nil
	}

	var destroyed []string
	for i := range datasets {
		ds := &datasets[i]
		if err := Reapable(ds, t.Policy, now); err != nil {
			// Not an error condition: most sweeps see mostly unexpired
			// datasets. Debug keeps the common case quiet while still letting
			// an operator ask why something was not destroyed.
			log.Debug("reaper kept a dataset", "backend", t.Name, "dataset", ds.ID, "reason", err.Error())
			continue
		}
		// Not forced, and not recursive beyond the dataset's own children: a
		// retired volume that is the ORIGIN of a live clone cannot be destroyed.
		// The appliance says so with EFAULT and the words "dependent clones" --
		// NOT with EBUSY, which is what this used to test for, so the quiet path
		// below never ran and every sweep warned about a dataset that was
		// behaving exactly as designed. Promoting the clone to break
		// the dependency is exactly what this codebase already refuses to do —
		// promote INVERTS the dependency and would make the live volume depend
		// on a dataset that is queued for destruction. So the reaper waits: the
		// clone is somebody's running volume, and when it goes away this
		// dataset becomes destroyable on the next sweep with no further help.
		if err := t.Client.DatasetDelete(ctx, ds.ID, true, false); err != nil {
			if truenas.IsHasDependentClones(err) || truenas.IsBusy(err) {
				log.Info("reaper kept an expired dataset: it is still the origin of a clone. "+
					"It will be destroyed once the volume cloned from it is deleted; nothing "+
					"is promoted, because promoting would make the LIVE volume depend on this one",
					"backend", t.Name, "dataset", ds.ID)
				continue
			}
			log.Warn("reaper could not destroy an expired dataset; it will be retried",
				"backend", t.Name, "dataset", ds.ID, "error", obs.Redact(err.Error()))
			continue
		}
		log.Info("reaper destroyed a retired volume: its grace period expired",
			"backend", t.Name, "dataset", ds.ID,
			"volume_id", ds.LocalProperty(volume.RetiredFromProperty),
			"retired_at", ds.LocalProperty(volume.DeletedAtProperty),
			"grace_period", t.Policy.Grace.String())
		destroyed = append(destroyed, ds.ID)
	}
	return destroyed
}

// Run sweeps on each target's interval until the context ends.
//
// One ticker at the shortest configured interval drives every target; a target
// is swept on every tick regardless, because Reapable, not the schedule, is
// what decides whether anything is destroyed.
func (r *Reaper) Run(ctx context.Context) {
	interval := r.interval(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Reaper) interval(ctx context.Context) time.Duration {
	interval := time.Duration(0)
	for _, t := range r.Backends(ctx) {
		if !t.Policy.On() {
			continue
		}
		if interval == 0 || t.Policy.ReapInterval < interval {
			interval = t.Policy.ReapInterval
		}
	}
	if interval <= 0 {
		return time.Hour
	}
	return interval
}
