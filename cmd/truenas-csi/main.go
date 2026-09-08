// Command truenas-csi runs the TrueNAS CSI driver as either the controller or
// the node plugin, selected by -mode.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/arraymetrics"
	"github.com/piwi3910/truenas-csi/internal/backend"
	_ "github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nfs"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nvme"
	_ "github.com/piwi3910/truenas-csi/internal/backend/smb"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/podmon"
	"github.com/piwi3910/truenas-csi/internal/reconcile"
	"github.com/piwi3910/truenas-csi/internal/server"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	var (
		mode       = flag.String("mode", "", "which plugin to run: controller or node")
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI socket endpoint")
		configPath = flag.String("config", "/etc/truenas-csi/config.yaml", "path to the driver configuration")
		nodeID     = flag.String("node-id", "", "node name (overrides the configuration)")
		hostRoot   = flag.String("host-root", "/host", "path where the host filesystem is mounted")
		podmonAddr = flag.String("podmon-addr", "", "address for the ValidateVolumeHostConnectivity extension "+
			"(a driver extension, not CSI); empty disables it. Either a TCP address or unix:///path/to.sock")
		logConfig = flag.String("log-config", "", "path to a mounted logging ConfigMap holding logLevel and "+
			"logFormat, re-read live; empty disables dynamic logging")
		metricsLease = flag.String("metrics-lease", "",
			"name of the Lease electing the single controller replica that polls the appliance for "+
				"array metrics; empty means every replica polls, multiplying load against the "+
				"appliance's 20-call concurrency ceiling. Deliberately distinct from the CSI "+
				"sidecars' own election, which elects a writer rather than a poller")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", driver.DriverName, driver.Version)
		return
	}
	if *mode != "controller" && *mode != "node" {
		fmt.Fprintf(os.Stderr, "-mode must be %q or %q, got %q\n", "controller", "node", *mode)
		os.Exit(1)
	}
	if err := run(options{
		mode:         *mode,
		endpoint:     *endpoint,
		configPath:   *configPath,
		nodeID:       *nodeID,
		hostRoot:     *hostRoot,
		podmonAddr:   *podmonAddr,
		logConfig:    *logConfig,
		metricsLease: *metricsLease,
	}); err != nil {
		slog.Error("driver exited", "error", obs.Redact(err.Error()))
		os.Exit(1)
	}
}

// options is everything the command line decides. It is a struct rather than a
// parameter list because run already took six positional strings and the next
// reader of a seventh would have had no chance.
type options struct {
	mode       string
	endpoint   string
	configPath string
	nodeID     string
	hostRoot   string
	podmonAddr string

	logConfig string
	// metricsLease names the Lease electing the array-metrics poller. Empty
	// means no election: this replica polls.
	metricsLease string
}

// orphanInterval is how often the controller compares appliance state against
// the cluster's PersistentVolumes.
const orphanInterval = 30 * time.Minute

// logLevel maps the configured level name onto slog, falling back to info for
// anything it does not recognise. The startup path tolerates a bad name; the
// watched ConfigMap deliberately does not, because there somebody is waiting
// for the debug records they just asked for.
func logLevel(name string) slog.Level {
	l, _ := obs.ParseLevel(name)
	return l
}

func run(o options) error {
	mode, endpoint, hostRoot, podmonAddr := o.mode, o.endpoint, o.hostRoot, o.podmonAddr

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}
	if o.nodeID != "" {
		cfg.NodeID = o.nodeID
	}
	// Register every credential for redaction before anything can log.
	for _, b := range cfg.Backends {
		obs.Register(b.APIKey)
	}
	obs.SetLogOutput(os.Stderr, logLevel(cfg.LogLevel))
	// Route the package-level logger through obs too. Without this the driver's
	// own slog.Info calls bypass the redacting handler entirely — and would also
	// ignore a log level raised through the watched ConfigMap.
	slog.SetDefault(obs.Logger(context.Background()))

	// Dynamic log level and format, from a ConfigMap that is deliberately NOT
	// the credential Secret: raising verbosity during an incident should not
	// require access to an API key.
	startLogWatch(ctx, o.logConfig, cfg.LogLevel)

	go serveHTTP(cfg)

	var (
		ctrl csipb.ControllerServer
		gc   csipb.GroupControllerServer
		nd   csipb.NodeServer
		reg  *backend.Registry
		pm   *podmon.Service
	)

	switch mode {
	case "controller":
		reg, err = backend.NewRegistry(ctx, cfg)
		if err != nil {
			return err
		}
		defer reg.Close()
		ctrl = csi.NewController(reg, cfg)
		// Group snapshots are a controller-side capability: the node plugin has
		// no part in them.
		gc = csi.NewGroupController(reg, cfg)

		// The orphan reconciler reports appliance objects with no
		// PersistentVolume. It never deletes; an apparent orphan is more often
		// a stale PV listing than a leak. Without a usable API-server client it
		// is skipped rather than silently reporting everything as orphaned.
		if lister, lErr := reconcile.NewKubePVLister(driver.DriverName); lErr != nil {
			slog.Warn("orphan reporting disabled: no in-cluster API access",
				"error", obs.Redact(lErr.Error()))
		} else {
			go reconcile.NewOrphanReconciler(reg, lister, orphanInterval).Run(ctx)
			slog.Info("orphan reporting enabled", "interval", orphanInterval.String())
		}

		// Credentials arrive as a mounted Secret and can be rotated under a
		// running driver. Only the controller holds appliance connections, so
		// only the controller has anything to swap.
		startCredentialReload(ctx, config.NewReloader(o.configPath, cfg), reg)

		// Array-level metrics run on the controller only: the node plugin has
		// no appliance client.
		startArrayMetrics(ctx, arraymetrics.New(reg, cfg.MetricsPollInterval()), o)
		obs.MarkReady()

	case "node":
		if err := cfg.ValidateNode(); err != nil {
			return err
		}
		pf, err := node.Detect(ctx, hostRoot, node.HostModprobe(hostRoot))
		if err != nil {
			return fmt.Errorf("node capability preflight: %w", err)
		}
		for capName, missing := range pf.Missing {
			slog.Warn("capability unavailable on this node",
				"capability", capName, "install", missing)
		}
		nn := node.NewNode(cfg.NodeID, pf, node.HostExec(hostRoot))
		nn.Root = hostRoot
		// The connectivity monitor polls the data path of every volume this
		// node has staged, so a NAS the node can no longer reach shows up as an
		// abnormal volume condition and a metric instead of as pods hanging on
		// I/O that never completes.
		go nn.Health().Run(ctx)
		slog.Info("volume connectivity monitoring enabled",
			"interval", node.DefaultHealthInterval.String(),
			"timeout", node.DefaultHealthTimeout.String())
		nd = csi.NewNode(nn)

		// The connectivity extension answers from the node plugin's own state
		// and its own bounded probes. It is given a lookup function, not the
		// driver's client or its socket, so that it keeps answering when the
		// driver it lives beside has stalled -- which is the only reason to run
		// a second health checker at all.
		pm = podmon.New(cfg.NodeID, applianceName(cfg), applianceAddr(cfg))
		pm.Volumes = func(id string) (podmon.VolumeRef, bool) {
			t, ok := nn.Health().Target(id)
			if !ok {
				return podmon.VolumeRef{}, false
			}
			return podmon.VolumeRef{VolumeID: t.VolumeID, Protocol: t.Protocol, Path: t.Path}, true
		}
		obs.MarkReady()
	}

	if podmonAddr != "" && pm != nil {
		go func() {
			slog.Info("serving the ValidateVolumeHostConnectivity extension "+
				"(a driver extension, not CSI)", "address", podmonAddr, "path", podmon.ValidatePath)
			if err := podmon.Serve(ctx, podmonAddr, pm); err != nil {
				slog.Warn("podmon extension listener stopped", "error", obs.Redact(err.Error()))
			}
		}()
	}

	srv, err := server.New(endpoint, csi.NewIdentity(func() bool { return true }), ctrl, gc, nd)
	if err != nil {
		return err
	}
	slog.Info("serving", "driver", driver.DriverName, "version", driver.Version,
		"mode", mode, "endpoint", srv.Addr())
	return srv.Serve(ctx)
}

// serveHTTP exposes metrics and health. It never carries volume data, so a
// failure here is logged and tolerated rather than fatal.
func serveHTTP(cfg *config.Config) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.Handle("/healthz", obs.HealthHandler())
	mux.Handle("/readyz", obs.ReadyHandler())
	s := &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Warn("metrics/health listener stopped", "error", err.Error())
	}
}

// applianceName and applianceAddr pick the appliance the podmon extension
// probes. A multi-backend driver has no single data path, so the first backend
// by name is used: the extension reports node-level reachability, and every
// volume it is asked about is checked individually anyway.
func applianceName(cfg *config.Config) string {
	names := make([]string, 0, len(cfg.Backends))
	for name := range cfg.Backends {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func applianceAddr(cfg *config.Config) string {
	name := applianceName(cfg)
	if name == "" {
		return ""
	}
	u, err := url.Parse(cfg.Backends[name].Endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() != "" {
		return u.Host
	}
	return net.JoinHostPort(u.Hostname(), "443")
}
