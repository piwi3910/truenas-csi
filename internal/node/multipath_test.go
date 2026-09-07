package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// captureLogs redirects the driver's log output for the duration of one test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	obs.SetLogOutput(buf, slog.LevelDebug)
	t.Cleanup(func() { obs.SetLogOutput(nil, slog.LevelInfo) })
	return buf
}

func TestMultipathDegrades(t *testing.T) {
	buf := captureLogs(t)
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	// No CapMultipath: worker-21's actual state — multipath-tools is not installed.
	n := newTestNode(t, root, e, CapISCSI, CapExt4)

	stage := func(staging string) {
		t.Helper()
		if err := n.Stage(context.Background(), StageRequest{
			VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(),
			VolumeCapability: VolumeCapability{FsType: "ext4"},
		}); err != nil {
			t.Fatalf("Stage: %v", err)
		}
	}
	stage(filepath.Join(t.TempDir(), "globalmount"))
	stage(filepath.Join(t.TempDir(), "globalmount2"))

	// The attach must succeed on the plain by-id device: a missing multipathd is
	// a degradation, not a failure.
	for _, c := range e.only("mount") {
		if !strings.Contains(c, testByID) {
			t.Fatalf("stage did not use the plain by-id device: %q", c)
		}
	}
	if got := e.only("multipath"); len(got) != 0 {
		t.Fatalf("node ran multipath on a host without it: %q", got)
	}

	// The warning names the package to install, and is emitted once per node
	// start rather than once per volume — two stages, one line.
	logs := buf.String()
	warned := 0
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if strings.Contains(line, "multipath-tools") {
			warned++
		}
	}
	if warned != 1 {
		t.Fatalf("multipath-tools warning was logged %d times, want exactly 1 per node start:\n%s", warned, logs)
	}
}

func TestMultipathUsesMapperDevice(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	const wwid = "36589cfc000000a960e31390c2657efa7"
	e := &fakeExec{handler: func(name string, args []string) ([]byte, error) {
		if name == "multipath" {
			if args[len(args)-1] != wwid {
				return nil, errors.New("multipath was not asked about our wwid")
			}
			return []byte(wwid + " dm-3 TrueNAS ,iSCSI Disk\n" +
				"size=1.0G features='0' hwhandler='1 alua' wp=rw\n"), nil
		}
		return h.run(name, args)
	}}
	n := newTestNode(t, root, e, CapISCSI, CapExt4, CapMultipath)
	staging := filepath.Join(t.TempDir(), "globalmount")

	if err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	mapper := "/dev/mapper/" + wwid
	mkfs := e.only("mkfs.ext4")
	if len(mkfs) != 1 || !strings.HasSuffix(mkfs[0], mapper) {
		t.Fatalf("mkfs used %q, want the mapper device %q", mkfs, mapper)
	}
	mounts := e.only("mount")
	if len(mounts) != 1 || !strings.Contains(mounts[0], mapper+" ") {
		t.Fatalf("mount used %q, want the mapper device %q", mounts, mapper)
	}
	for _, c := range mounts {
		if strings.Contains(c, "/dev/disk/by-id/") {
			t.Fatalf("mount used a single-path device on a multipathed volume: %q", c)
		}
	}

	// user_friendly_names on gives the map an mpathN alias instead of the wwid;
	// both forms must resolve.
	eFriendly := &fakeExec{handler: func(name string, args []string) ([]byte, error) {
		if name == "multipath" {
			return []byte("mpatha (" + wwid + ") dm-3 TrueNAS ,iSCSI Disk\n"), nil
		}
		return h.run(name, args)
	}}
	dev, ok, err := multipathDevice(context.Background(), eFriendly, testNAA)
	if err != nil || !ok || dev != "/dev/mapper/mpatha" {
		t.Fatalf("multipathDevice(friendly) = %q, %v, %v", dev, ok, err)
	}

	// A device that is not multipathed reports so rather than erroring: the
	// caller then uses the single path, which works.
	eNone := &fakeExec{handler: func(name string, args []string) ([]byte, error) {
		if name == "multipath" {
			return nil, nil
		}
		return h.run(name, args)
	}}
	if dev, ok, err := multipathDevice(context.Background(), eNone, testNAA); ok || err != nil || dev != "" {
		t.Fatalf("multipathDevice(no map) = %q, %v, %v", dev, ok, err)
	}
}
