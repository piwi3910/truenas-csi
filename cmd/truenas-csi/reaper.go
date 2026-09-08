package main

import (
	"context"
	"log/slog"
	"sort"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/retention"
)

// startReaper starts the delete-protection reaper, if any backend asked for it.
//
// The reaper destroys graveyard datasets whose grace period has expired. It is
// started here and only here, in the controller, because it is the only place
// that holds appliance connections — and it is deliberately NOT folded into the
// orphan reconciler, which is report-only on purpose and must stay that way: an
// apparent orphan is more often a stale PersistentVolume listing than a leak,
// and a reconciler with a destroy path is one flag away from acting on that.
//
// The reaper acts on evidence of a different kind entirely. It touches only
// datasets this driver itself renamed into a dataset this driver created, and
// only after a delay this driver recorded on the object; retention.Reapable
// re-checks all four of those, from the appliance's answer, before every single
// destroy.
//
// It runs on EVERY controller replica rather than behind a lease. Two replicas
// destroying the same expired dataset is harmless — the middleware answers a
// missing dataset with null rather than an error, so the loser simply succeeds —
// and a lease would add a way for the reaper to silently never run, which for a
// feature whose whole job is bounded retention is the worse failure.
func startReaper(ctx context.Context, reg *backend.Registry, cfg *config.Config) {
	var on []string
	for name, b := range cfg.Backends {
		if b.DeleteProtection.Grace() > 0 {
			on = append(on, name+" "+b.DeleteProtection.String())
		}
	}
	if len(on) == 0 {
		return
	}
	sort.Strings(on)

	slog.Warn("delete protection is ENABLED: DeleteVolume will retire volumes into a graveyard "+
		"dataset instead of destroying them, and THEIR SPACE IS NOT RECLAIMED until the grace "+
		"period expires. The pool reserve and the capacity reported to the scheduler both keep "+
		"counting it as used",
		"backends", on)

	r := retention.NewReaper(reg.RetentionTargets)
	go r.Run(ctx)
	slog.Info("delete-protection reaper started; it destroys only datasets inside the graveyard "+
		"that are driver-owned, carry a deletion timestamp and are past their grace period",
		"backends", on)
}
