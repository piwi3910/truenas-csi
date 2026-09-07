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
	err := run("controller", "unix:///tmp/never-created.sock", path, "worker-21", "/host", "")
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
	if err := run("controller", "unix:///tmp/x.sock",
		filepath.Join(t.TempDir(), "absent.yaml"), "n", "/host", ""); err == nil {
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
