package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/piwi3910/truenas-csi/internal/arraymetrics"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/prometheus/client_golang/prometheus"
)

// credentialReloader is the part of a client that can adopt a rotated
// credential in place. It is declared here, at the consumer, so that a client
// which cannot do it simply fails the type assertion and is reported — see
// applyRotatedCredentials.
type credentialReloader interface {
	ReloadCredentials(config.Backend) error
}

// startLogWatch applies the mounted logging ConfigMap, if there is one, and
// keeps watching it.
//
// A missing or invalid file is a warning, never fatal: the driver's job is to
// serve volumes, and it must not refuse to start because somebody mistyped a
// log level.
func startLogWatch(ctx context.Context, path, startupLevel string) {
	if path == "" {
		return
	}
	cur := config.Logging{Level: startupLevel}.Normalised()
	if l, err := config.LoadLogging(path); err != nil {
		slog.Warn("dynamic logging unavailable; keeping the startup log settings",
			"path", path, "error", obs.Redact(err.Error()))
	} else {
		cur = l
		applyLogging(l)
	}
	go func() {
		err := config.WatchLogging(ctx, path, cur, applyLogging, func(err error) {
			slog.Warn("rejected a logging config change; log settings are unchanged",
				"error", obs.Redact(err.Error()))
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("dynamic logging stopped", "error", obs.Redact(err.Error()))
		}
	}()
	slog.Info("dynamic logging enabled", "path", path,
		"level", cur.Level, "format", cur.Format)
}

// applyLogging puts new logging settings into force immediately.
func applyLogging(l config.Logging) {
	level, ok := obs.ParseLevel(l.Level)
	if !ok {
		// config.LoadLogging validated this already; belt and braces.
		slog.Warn("unknown log level ignored", "level", l.Level)
		return
	}
	obs.SetLogLevel(level)
	if err := obs.SetLogFormat(l.Format); err != nil {
		slog.Warn("unknown log format ignored", "format", l.Format)
	}
	// The default logger holds a handler captured when it was built, so a
	// format change has to be re-pointed at the new one. The level does not
	// need this: it lives in a shared slog.LevelVar.
	slog.SetDefault(obs.Logger(context.Background()))
	slog.Info("log settings reloaded", "level", l.Level, "format", l.Format)
}

// startCredentialReload watches the mounted configuration Secret and swaps
// rotated credentials into the live appliance connections.
//
// Nothing here can take the driver down. A file that fails to parse, fails
// validation — including the transport check that stops an API key being
// presented over plaintext — or changes a field that needs a restart is logged
// and discarded, and the driver keeps running on the configuration it has.
func startCredentialReload(ctx context.Context, reloader *config.Reloader, reg *backend.Registry) {
	go func() {
		err := reloader.Run(ctx,
			func(rotated map[string]config.Credential, cfg *config.Config) {
				applyRotatedCredentials(ctx, reg, cfg, rotated)
			},
			func(err error) {
				if errors.Is(err, config.ErrRestartRequired) {
					slog.Error("refusing a configuration change that needs a restart; "+
						"the driver is still running the previous configuration",
						"error", obs.Redact(err.Error()))
					return
				}
				slog.Error("rejected an invalid configuration change; "+
					"the driver is still running the previous configuration",
					"error", obs.Redact(err.Error()))
			})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("credential reload stopped; a rotated API key will now need a restart",
				"error", obs.Redact(err.Error()))
		}
	}()
	slog.Info("credential hot-reload enabled", "path", reloader.Path())
}

// applyRotatedCredentials records the new credentials and pushes them into the
// live connections.
//
// The two loops are ordered on purpose: every new credential is recorded before
// any connection is touched, so that a dial triggered by the second loop
// already presents the NEW key. Presenting a superseded key is not a harmless
// retry on TrueNAS — it is how a key gets revoked.
func applyRotatedCredentials(ctx context.Context, reg *backend.Registry, cfg *config.Config,
	rotated map[string]config.Credential) {
	for name, cred := range rotated {
		// Redact the new key before anything can log it, including the errors
		// produced by adopting it.
		obs.Register(cred.APIKey)
		config.SetCredential(name, cred)
	}
	if reg == nil {
		return
	}
	for name := range rotated {
		b, ok := cfg.Backends[name]
		if !ok {
			continue
		}
		cl, err := reg.Client(ctx, name)
		if err != nil {
			// The credential is recorded, so the next dial of this appliance
			// will use it whenever it comes back. Nothing more to do here.
			slog.Warn("credential recorded but not yet applied: the appliance is unreachable",
				"backend", name, "error", obs.Redact(err.Error()))
			continue
		}
		r, ok := cl.(credentialReloader)
		if !ok {
			slog.Warn("this backend's client cannot swap credentials in place; "+
				"restart the controller to pick up the new credential",
				"backend", name, "flavour", b.NormalisedFlavour())
			continue
		}
		if err := r.ReloadCredentials(b.Current()); err != nil {
			slog.Error("failed to apply a rotated credential; the connection keeps the old one",
				"backend", name, "error", obs.Redact(err.Error()))
			continue
		}
		slog.Info("credential reloaded without a restart", "backend", name)
	}
}

// startArrayMetrics starts appliance polling, optionally behind a leader
// election of its own.
//
// The election is separate from the CSI sidecars' — a replica may hold the
// provisioning lease and not this one — because the two answer different
// questions: the sidecars elect a writer, this elects the single replica
// allowed to spend the appliance's small concurrency budget on polling. With
// three replicas and no election, three pollers hit the same appliance, and the
// verified ceiling is 20 in-flight calls.
//
// The collector is registered with Prometheus ONLY while this replica leads,
// and unregistered when leadership is lost. A non-leader therefore exports no
// truenas_pool_*, truenas_dataset_* or truenas_iscsi_* series at all, which is
// the only honest answer: exporting zeros would read as a pool that suddenly
// emptied and page somebody, and exporting the last values it happened to
// collect would be a gauge that quietly stops tracking reality. An absent
// series is something PromQL already models correctly, and the collection
// counter resuming on a different replica is a counter reset that rate()
// handles — a stale copy of it on every replica is not.
func startArrayMetrics(ctx context.Context, arrayCollector *arraymetrics.Collector, o options) {
	poll := func(ctx context.Context) {
		// If nothing answers at startup the collector is skipped with a log
		// rather than failing the process: an appliance outage must not
		// crash-loop the controller, and a collector that can never produce a
		// value only adds a permanently empty scrape.
		if err := arrayCollector.Start(ctx); err != nil {
			slog.Warn("array metrics disabled: no reachable backend at startup",
				"error", obs.Redact(err.Error()))
			return
		}
		if err := prometheus.Register(arrayCollector); err != nil {
			slog.Warn("array metrics disabled: collector registration failed",
				"error", obs.Redact(err.Error()))
			return
		}
		defer prometheus.Unregister(arrayCollector)
		slog.Info("array metrics enabled", "interval", arrayCollector.Interval().String())
		arrayCollector.Run(ctx)
	}

	if o.metricsLease == "" {
		go poll(ctx)
		return
	}

	leader, err := obs.InClusterLeaderConfig(o.metricsLease)
	if err != nil {
		// No API access: one replica is the only sane assumption, and refusing
		// to collect anything at all would be worse than the load of polling.
		slog.Warn("array-metrics leader election unavailable; this replica will poll",
			"error", obs.Redact(err.Error()))
		go poll(ctx)
		return
	}
	go func() {
		slog.Info("array-metrics leader election enabled",
			"lease", o.metricsLease, "identity", leader.Identity)
		err := obs.RunLeader(ctx, leader, poll, func() {
			slog.Info("lost the array-metrics lease; this replica has stopped polling "+
				"and no longer exports array metrics", "lease", o.metricsLease)
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("array-metrics leader election stopped", "error", obs.Redact(err.Error()))
		}
	}()
}
