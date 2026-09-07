package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testRelease = "6.12.58-test"

// fakeRoot builds a temporary host filesystem root containing the named binaries
// (paths relative to the root, e.g. "sbin/mkfs.ext4"), a proc/modules listing the
// named loaded modules, and an osrelease file pinning the kernel release.
func fakeRoot(t *testing.T, bins []string, loaded []string, kos []string) string {
	t.Helper()
	root := t.TempDir()

	for _, b := range bins {
		p := filepath.Join(root, b)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	var b strings.Builder
	for _, m := range loaded {
		b.WriteString(m + " 16384 0 - Live 0x0000000000000000\n")
	}
	if err := os.MkdirAll(filepath.Join(root, "proc", "sys", "kernel"), 0o755); err != nil {
		t.Fatalf("mkdir proc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "modules"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write proc/modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "sys", "kernel", "osrelease"), []byte(testRelease+"\n"), 0o644); err != nil {
		t.Fatalf("write osrelease: %v", err)
	}

	for _, k := range kos {
		p := filepath.Join(root, "lib", "modules", testRelease, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte("ELF"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

// recorder records every module name modprobe was asked to load.
type recorder struct {
	calls []string
}

func (r *recorder) modprobe(_ context.Context, mod string) error {
	r.calls = append(r.calls, mod)
	return nil
}

// TestPreflightMissingTool catches a Detect that reports a capability as present
// when the binary providing it is absent, or that fails to name the package an
// operator must install.
func TestPreflightMissingTool(t *testing.T) {
	root := fakeRoot(t, []string{"sbin/mkfs.ext4", "sbin/resize2fs"}, nil, nil)

	var rec recorder
	p, err := Detect(context.Background(), root, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if p.Found[CapXFS] {
		t.Fatalf("CapXFS reported available on a root without mkfs.xfs")
	}
	pkgs := p.Missing[CapXFS]
	if !containsSubstr(pkgs, "xfsprogs") {
		t.Fatalf("Missing[CapXFS] = %v, want it to name xfsprogs", pkgs)
	}
	if !p.Found[CapExt4] {
		t.Fatalf("CapExt4 reported unavailable although mkfs.ext4 and resize2fs are present")
	}

	err = p.Require(CapXFS)
	if err == nil {
		t.Fatalf("Require(CapXFS) returned nil on a node without xfsprogs")
	}
	if !strings.Contains(err.Error(), "xfsprogs") {
		t.Fatalf("Require(CapXFS) error = %q, want it to name xfsprogs", err)
	}
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("Require(CapXFS) error does not wrap ErrCapabilityUnavailable: %v", err)
	}
	if err := p.Require(CapExt4); err != nil {
		t.Fatalf("Require(CapExt4) = %v, want nil", err)
	}
}

// TestPreflightLoadsModules catches a Detect that treats a module which is on disk
// but not currently loaded as unavailable, instead of loading it with modprobe.
func TestPreflightLoadsModules(t *testing.T) {
	root := fakeRoot(t,
		[]string{"sbin/iscsiadm", "sbin/iscsid"},
		[]string{"ext4", "nfs"},
		[]string{"kernel/drivers/scsi/iscsi_tcp.ko"},
	)

	var rec recorder
	p, err := Detect(context.Background(), root, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if !p.Found[CapISCSI] {
		t.Fatalf("CapISCSI unavailable although iscsi_tcp.ko is on disk: missing=%v", p.Missing[CapISCSI])
	}
	if len(p.Missing[CapISCSI]) != 0 {
		t.Fatalf("Missing[CapISCSI] = %v, want empty", p.Missing[CapISCSI])
	}

	var loads int
	for _, c := range rec.calls {
		if c == "iscsi_tcp" {
			loads++
		}
	}
	if loads != 1 {
		t.Fatalf("modprobe called %d times for iscsi_tcp (all calls: %v), want exactly 1", loads, rec.calls)
	}
}

// TestPreflightModuleAbsentEntirely catches a Detect that pretends a module which
// is neither loaded nor on disk can be loaded anyway.
func TestPreflightModuleAbsentEntirely(t *testing.T) {
	root := fakeRoot(t, []string{"sbin/iscsiadm", "sbin/iscsid"}, nil, nil)

	var rec recorder
	p, err := Detect(context.Background(), root, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	if p.Found[CapISCSI] {
		t.Fatalf("CapISCSI reported available although iscsi_tcp is neither loaded nor on disk")
	}
	if !containsSubstr(p.Missing[CapISCSI], "iscsi_tcp") {
		t.Fatalf("Missing[CapISCSI] = %v, want it to name the iscsi_tcp module", p.Missing[CapISCSI])
	}
	if len(rec.calls) != 0 {
		t.Fatalf("modprobe called %v for a module that is not on disk", rec.calls)
	}
}

// TestPreflightAlreadyLoadedModuleIsNotReloaded catches a Detect that runs modprobe
// for modules /proc/modules already lists.
func TestPreflightAlreadyLoadedModuleIsNotReloaded(t *testing.T) {
	root := fakeRoot(t,
		[]string{"sbin/iscsiadm", "sbin/iscsid"},
		[]string{"iscsi_tcp", "libiscsi"},
		[]string{"kernel/drivers/scsi/iscsi_tcp.ko"},
	)

	var rec recorder
	p, err := Detect(context.Background(), root, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p.Found[CapISCSI] {
		t.Fatalf("CapISCSI unavailable although iscsi_tcp is loaded: missing=%v", p.Missing[CapISCSI])
	}
	if len(rec.calls) != 0 {
		t.Fatalf("modprobe called %v for an already loaded module", rec.calls)
	}
}

// TestTopologyLabelsReflectCapabilities catches labels that are hard-coded rather
// than derived from what was actually detected.
func TestTopologyLabelsReflectCapabilities(t *testing.T) {
	without := fakeRoot(t, []string{"sbin/mkfs.ext4", "sbin/resize2fs"}, nil, nil)
	p, err := Detect(context.Background(), without, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	labels := p.TopologyLabels()
	if got := labels["csi.truenas.watteel.com/xfs"]; got != "false" {
		t.Fatalf("xfs label = %q on a node without mkfs.xfs, want \"false\"", got)
	}
	if got := labels["csi.truenas.watteel.com/ext4"]; got != "true" {
		t.Fatalf("ext4 label = %q on a node with e2fsprogs, want \"true\"", got)
	}

	with := fakeRoot(t, []string{"sbin/mkfs.xfs", "sbin/xfs_growfs"}, nil, nil)
	p, err = Detect(context.Background(), with, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	labels = p.TopologyLabels()
	if got := labels["csi.truenas.watteel.com/xfs"]; got != "true" {
		t.Fatalf("xfs label = %q on a node with xfsprogs, want \"true\"", got)
	}
	if got := labels["csi.truenas.watteel.com/ext4"]; got != "false" {
		t.Fatalf("ext4 label = %q on a node without e2fsprogs, want \"false\"", got)
	}
}

// TestDetectFindsBinariesInEverySearchDir catches a Detect that only looks in sbin.
func TestDetectFindsBinariesInEverySearchDir(t *testing.T) {
	for _, dir := range []string{"sbin", "usr/sbin", "bin", "usr/bin"} {
		root := fakeRoot(t, []string{dir + "/mkfs.xfs", dir + "/xfs_growfs"}, nil, nil)
		p, err := Detect(context.Background(), root, nil)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if !p.Found[CapXFS] {
			t.Fatalf("xfsprogs in %s/ not found: missing=%v", dir, p.Missing[CapXFS])
		}
	}
}

func containsSubstr(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}
