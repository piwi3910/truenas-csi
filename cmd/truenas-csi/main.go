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
	"github.com/piwi3910/truenas-csi/internal/backend"
	_ "github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nfs"
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
	if err := run(*mode, *endpoint, *configPath, *nodeID, *hostRoot, *podmonAddr); err != nil {
		slog.Error("driver exited", "error", obs.Redact(err.Error()))
		os.Exit(1)
	}
}

// logLevel maps the configured level name onto slog.
// orphanInterval is how often the controller compares appliance state against
// the cluster's PersistentVolumes.
const orphanInterval = 30 * time.Minute

func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func run(mode, endpoint, configPath, nodeID, hostRoot, podmonAddr string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if nodeID != "" {
		cfg.NodeID = nodeID
	}
	// Register every credential for redaction before anything can log.
	for _, b := range cfg.Backends {
		obs.Register(b.APIKey)
	}
	obs.SetLogOutput(os.Stderr, logLevel(cfg.LogLevel))

	go serveHTTP(cfg)

	var (
		ctrl csipb.ControllerServer
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
		pm = podmon.New(cfg.NodeID, applianceName(cfg), applianceAddr(cfg))

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

	srv, err := server.New(endpoint, csi.NewIdentity(func() bool { return true }), ctrl, nd)
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
