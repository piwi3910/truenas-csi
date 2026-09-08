package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// projectSecret writes dir the way the kubelet writes a mounted Secret or
// ConfigMap, and can be called again to rotate it.
//
// The layout matters, and it is the whole reason WatchFile watches a directory:
//
//	dir/config.yaml -> ..data/config.yaml   (symlink)
//	dir/..data      -> ..2026_09_08_03_00   (symlink, swapped atomically)
//	dir/..2026_09_08_03_00/config.yaml      (the real file)
//
// A rotation writes a NEW timestamped directory and renames a new "..data"
// symlink over the old one. The visible config.yaml never changes and the real
// file's inode is never written to, so a watch on either path sees nothing.
func projectSecret(t *testing.T, dir, name, content string, generation int) string {
	t.Helper()
	dataDir := filepath.Join(dir, fmt.Sprintf("..%d", generation))
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Atomic swap: create the new link under a temporary name, then rename it
	// over "..data". Exactly what the kubelet's atomic writer does.
	tmpLink := filepath.Join(dir, "..data_tmp")
	_ = os.Remove(tmpLink)
	if err := os.Symlink(dataDir, tmpLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	visible := filepath.Join(dir, name)
	if _, err := os.Lstat(visible); err != nil {
		if err := os.Symlink(filepath.Join("..data", name), visible); err != nil {
			t.Fatal(err)
		}
	}
	return visible
}

const validConfig = `
logLevel: info
backends:
  nas1:
    endpoint: wss://192.168.10.253/api/current
    username: truenas_admin
    apiKey: %s
    pool: Pool0
    parentDataset: k8s
`

// TestWatchFileSeesAKubernetesSecretRotation is the test that matters for this
// whole feature: a naive watch on the file path observes nothing when a Secret
// is rotated, so the driver would never learn about a new API key.
func TestWatchFileSeesAKubernetesSecretRotation(t *testing.T) {
	dir := t.TempDir()
	path := projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "8-first"), 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	changed := make(chan struct{}, 8)
	go func() {
		_ = WatchFile(ctx, path, func() { changed <- struct{}{} })
	}()
	// Give the watcher time to register before the swap; an event delivered
	// before Add() would make this test pass for the wrong reason.
	time.Sleep(200 * time.Millisecond)

	projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "9-rotated"), 2)

	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("a Kubernetes Secret rotation produced no change event: " +
			"the watch is on the file, not the directory")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "9-rotated") {
		t.Fatalf("the visible file still has the old content:\n%s", raw)
	}
}

// TestWatchFileStopsWithTheContext keeps the watcher from outliving the driver.
func TestWatchFileStopsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	path := projectSecret(t, dir, "config.yaml", fmt.Sprintf(validConfig, "8-first"), 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- WatchFile(ctx, path, func() {}) }()
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WatchFile did not return when its context was cancelled")
	}
}

func TestWatchFileRejectsAMissingDirectory(t *testing.T) {
	err := WatchFile(context.Background(),
		filepath.Join(t.TempDir(), "absent", "config.yaml"), func() {})
	if err == nil {
		t.Fatal("watching a directory that does not exist must fail loudly")
	}
}
