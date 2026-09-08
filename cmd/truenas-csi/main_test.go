package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogLevelMapping(t *testing.T) {
	for name, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo,
		"warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo,
		"nonsense": slog.LevelInfo,
	} {
		if got := logLevel(name); got != want {
			t.Errorf("logLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestRunRejectsPlaintextConfig is the last line of defence for the rule that
// cost three API keys: a plaintext endpoint must stop the process before it can
// connect, whichever mode it was started in.
func TestRunRejectsPlaintextConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
nodeID: worker-21
backends:
  nas1:
    endpoint: http://192.168.10.253/api/current
    username: truenas_admin
    apiKey: 8-secret
    pool: Pool0
    parentDataset: k8s
`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(options{mode: "controller", endpoint: "unix:///tmp/never-created.sock",
		configPath: path, nodeID: "worker-21", hostRoot: "/host"})
	if err == nil {
		t.Fatal("a plaintext endpoint must stop the driver starting")
	}
	if !strings.Contains(err.Error(), "wss://") {
		t.Fatalf("error should explain the wss requirement, got %v", err)
	}
	if _, statErr := os.Stat("/tmp/never-created.sock"); statErr == nil {
		os.Remove("/tmp/never-created.sock")
		t.Fatal("the driver must fail before it opens a socket")
	}
}

func TestRunRejectsMissingConfig(t *testing.T) {
	if err := run(options{mode: "controller", endpoint: "unix:///tmp/x.sock",
		configPath: filepath.Join(t.TempDir(), "absent.yaml"), nodeID: "n", hostRoot: "/host"}); err == nil {
		t.Fatal("a missing config file must be fatal")
	}
}

// TestOrphanReconcilerIsWiredIntoTheController guards a gap that survived a
// full "everything is implemented" report: the reconciler had tests and worked,
// but nothing ever started it, so it did nothing in a running driver.
func TestOrphanReconcilerIsWiredIntoTheController(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reconcile.NewOrphanReconciler", "reconcile.NewKubePVLister"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go never calls %s: the reconciler would be dead code "+
				"in the shipped binary", want)
		}
	}
}

// TestMetricsAndHealthAreServed likewise checks the observability endpoints are
// actually started, not merely implemented.
func TestMetricsAndHealthAreServed(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/metrics", "/healthz", "/readyz", "go serveHTTP"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go does not serve %s", want)
		}
	}
}

// TestPodmonIsWiredIntoTheNode is the same guard as the reconciler's: the
// podmon extension is worthless if the shipped binary never starts a listener
// for it, and a package with green tests proves nothing about that.
func TestPodmonIsWiredIntoTheNode(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"podmon.New", "podmon.Serve", "podmon-addr"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go never references %s: the podmon extension would never "+
				"be reachable in a running node plugin", want)
		}
	}
}

// TestEveryBuiltProtocolIsLinkedIn guards a subtle way a backend becomes dead
// code: it registers itself from init(), so if nothing imports the package the
// protocol simply does not exist in the shipped binary and a StorageClass
// naming it fails with "protocol is not supported by this driver version".
func TestEveryBuiltProtocolIsLinkedIn(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir("../../internal/backend")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		want := "internal/backend/" + e.Name()
		if !strings.Contains(string(src), want) {
			t.Errorf("protocol package %s is built but never imported by main, so it is "+
				"not registered in the shipped binary", want)
		}
	}
}

// TestArrayCollectorIsWiredIntoTheController keeps the appliance metrics from
// becoming implemented-but-never-started.
func TestArrayCollectorIsWiredIntoTheController(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"arraymetrics.New", "startArrayMetrics"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go never calls %s: the collector would be dead code", want)
		}
	}
	// The poll loop itself moved into hardening.go, behind the leader election.
	// It is just as dead if nothing there ever runs it.
	hardening, err := os.ReadFile("hardening.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"arrayCollector.Run(ctx)", "obs.RunLeader"} {
		if !strings.Contains(string(hardening), want) {
			t.Errorf("hardening.go never calls %s: the collector would never poll", want)
		}
	}
}

// TestOperationalHardeningIsWiredIn is the same guard for the pieces that only
// exist if main actually starts them: credential hot-reload, the dynamic log
// level, and the array-metrics leader election. Each has tests of its own, and
// those prove nothing about the shipped binary.
func TestOperationalHardeningIsWiredIn(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"startCredentialReload", "config.NewReloader", "startLogWatch",
		"log-config", "metrics-lease",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go never references %s: the feature would be dead code "+
				"in the shipped binary", want)
		}
	}
}
