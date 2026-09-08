// Command manager runs the TrueNAS CSI lifecycle operator.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	truenasv1alpha1 "github.com/piwi3910/truenas-csi/operator/api/v1alpha1"
	"github.com/piwi3910/truenas-csi/operator/internal/chartrender"
	"github.com/piwi3910/truenas-csi/operator/internal/controller"
	"github.com/piwi3910/truenas-csi/operator/internal/health"
)

var scheme = runtime.NewScheme()

func init() {
	utilRuntimeMust(clientgoscheme.AddToScheme(scheme))
	utilRuntimeMust(truenasv1alpha1.AddToScheme(scheme))
}

func utilRuntimeMust(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr   string
		probeAddr     string
		leaderElect   bool
		chartDir      string
		backendHealth bool
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metric endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probes bind to")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"run leader election so only one manager reconciles at a time; two operators applying the same release would fight over every object")
	// The chart is not embedded. It is copied into the operator image from
	// deploy/helm at build time, so there is exactly one copy of the manifests
	// in the repository and the operator reads the same files a Helm user does.
	flag.StringVar(&chartDir, "chart-dir", "/chart/truenas-csi", "directory holding the in-repo Helm chart")
	flag.BoolVar(&backendHealth, "backend-health", true,
		"scrape the driver's metrics endpoint to report per-backend reachability and orphan counts in status")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	ch, err := chartrender.LoadChart(chartDir)
	if err != nil {
		setupLog.Error(err, "load chart", "dir", chartDir)
		os.Exit(1)
	}
	setupLog.Info("loaded chart", "name", ch.Metadata.Name, "version", ch.Metadata.Version, "appVersion", ch.Metadata.AppVersion)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "truenas-csi-operator.truenas.watteel.com",
		// Give up leadership rather than keep reconciling after losing the
		// lease: two managers server-side-applying the same release would
		// overwrite each other's field ownership on every pass.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "start manager")
		os.Exit(1)
	}

	reconciler := &controller.TrueNASCSIDriverReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Chart:       ch,
		KubeVersion: discoverKubeVersion(mgr),
		// A refused upgrade has to be visible without reading the CR's status
		// by hand, so every refusal is also an event on the resource.
		Recorder: mgr.GetEventRecorder("truenas-csi-operator"),
	}
	if backendHealth {
		reconciler.Probe = health.NewScraper()
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "set up controller", "controller", "TrueNASCSIDriver")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}

// discoverKubeVersion asks the cluster what version it is, so the chart's
// `.Capabilities.KubeVersion` matches reality. A failure is not fatal: the
// documented minimum renders correctly on every supported cluster, and refusing
// to start because a discovery call failed would be a worse trade.
func discoverKubeVersion(mgr ctrl.Manager) chartrender.KubeVersion {
	dc, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return chartrender.DefaultKubeVersion
	}
	info, err := dc.ServerVersion()
	if err != nil || info == nil {
		return chartrender.DefaultKubeVersion
	}
	return chartrender.KubeVersion{
		Version: info.GitVersion,
		Major:   info.Major,
		Minor:   info.Minor,
	}
}
