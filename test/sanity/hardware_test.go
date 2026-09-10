package sanity_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/server"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// TestCSISanityAgainstHardware runs the same conformance suite as TestCSISanity
// against a REAL appliance.
//
// It exists because the fake has been more permissive than the middleware in
// every direction that has been checked so far — dataset renames that silently
// need force, user properties that must be lowercase, inherited properties that
// report source LOCAL. A suite that only ever runs against the stand-in proves
// the driver conforms to the stand-in.
//
// The node half stays synthetic: staging and publishing on the machine running
// `go test` would need a real iSCSI initiator and root, and the node data paths
// have their own hardware coverage in test/integration. What is real here is
// every controller call — create, delete, expand, snapshot, list, modify —
// against the middleware.
func TestCSISanityAgainstHardware(t *testing.T) {
	endpoint := os.Getenv("TRUENAS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TRUENAS_ENDPOINT is not set: skipping conformance against real " +
			"hardware. Set TRUENAS_ENDPOINT (wss://host/api/current), " +
			"TRUENAS_API_KEY, TRUENAS_POOL and TRUENAS_PARENT to run it.")
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	// A parent of its own, so a failed run leaves its wreckage somewhere
	// obvious and never among volumes another suite is using.
	cfg := &config.Config{NodeID: "sanity-hw", Backends: map[string]config.Backend{
		"nas1": {
			Name: "nas1", Endpoint: endpoint,
			Username:           get("TRUENAS_USERNAME", "truenas_admin"),
			APIKey:             os.Getenv("TRUENAS_API_KEY"),
			Pool:               get("TRUENAS_POOL", "Pool0"),
			ParentDataset:      get("TRUENAS_SANITY_PARENT", "csi-sanity"),
			InsecureSkipVerify: os.Getenv("TRUENAS_INSECURE") == "true",
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid appliance configuration: %v", err)
	}
	reg, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	dir, err := os.MkdirTemp("/tmp", "tncsi-sanity-hw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "csi.sock")

	hostRoot := filepath.Join(dir, "host")
	exec := newMountingExec(hostRoot)
	pf, err := node.Detect(context.Background(), hostRoot, func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	pf.Found[node.CapNFS] = true
	pf.Found[node.CapExt4] = true
	delete(pf.Missing, node.CapNFS)
	delete(pf.Missing, node.CapExt4)

	srv, err := server.New("unix://"+sock, csi.NewIdentity(nil),
		csi.NewController(reg, cfg), csi.NewGroupController(reg, cfg),
		csi.NewNode(node.NewNode(cfg.NodeID, pf, exec)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(cancel)

	sanity.Test(t, sanity.TestConfig{
		Address:        "unix://" + sock,
		TargetPath:     filepath.Join(dir, "target"),
		StagingPath:    filepath.Join(dir, "staging"),
		TestVolumeSize: 1 << 30,
		TestVolumeParameters: map[string]string{
			"backend": "nas1", "protocol": "nfs",
			"server": get("TRUENAS_DATA_ADDR", "192.168.10.253"),
		},
		TestVolumeMutableParameters:         map[string]string{"sync": "standard"},
		IDGen:                               &sanity.DefaultIDGenerator{},
		TestInvalidListVolumesStartingToken: "invalid-token",
		CreateTargetDir: func(p string) (string, error) {
			return p, os.MkdirAll(p, 0o750)
		},
		CreateStagingDir: func(p string) (string, error) {
			return p, os.MkdirAll(p, 0o750)
		},
		RemoveTargetPath:  func(p string) error { return os.RemoveAll(p) },
		RemoveStagingPath: func(p string) error { return os.RemoveAll(p) },
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		ControllerDialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	})
}

// TestCrashMidCloneResumesAgainstHardware plants exactly the wreckage a
// controller crash leaves and checks the retry finishes it, against a REAL
// appliance.
//
// The premise is a ZFS fact the fake can only assert: a clone inherits neither
// the ownership marker nor refquota, so a controller that died between
// pool.snapshot.clone and the repairs that follow left a dataset that the retry
// then had to recognise as its own half-built work. Simulating it here is
// faithful because the wreckage is made the same way the driver would have made
// it -- a real clone of a real snapshot -- rather than described to a stand-in.
func TestCrashMidCloneResumesAgainstHardware(t *testing.T) {
	endpoint := os.Getenv("TRUENAS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TRUENAS_ENDPOINT is not set: skipping the crash-resume check " +
			"against real hardware")
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	pool := get("TRUENAS_POOL", "Pool0")
	parent := get("TRUENAS_SANITY_PARENT", "csi-sanity")
	cfg := &config.Config{NodeID: "resume-hw", Backends: map[string]config.Backend{
		"nas1": {
			Name: "nas1", Endpoint: endpoint,
			Username:           get("TRUENAS_USERNAME", "truenas_admin"),
			APIKey:             os.Getenv("TRUENAS_API_KEY"),
			Pool:               pool,
			ParentDataset:      parent,
			InsecureSkipVerify: os.Getenv("TRUENAS_INSECURE") == "true",
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid appliance configuration: %v", err)
	}
	ctx := context.Background()
	reg, err := backend.NewRegistry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ctrl := csi.NewController(reg, cfg)
	params := map[string]string{
		"backend": "nas1", "protocol": "nfs",
		"server": get("TRUENAS_DATA_ADDR", "192.168.10.253"),
	}
	const size = 1 << 30
	caps := []*csipb.VolumeCapability{{
		AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}}

	src, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: "resume-src", Parameters: params, VolumeCapabilities: caps,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: size},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	srcID := src.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: srcID})
	})

	snap, err := ctrl.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: srcID, Name: "resume-snap",
	})
	if err != nil {
		t.Fatalf("snapshot source: %v", err)
	}
	snapID := snap.GetSnapshot().GetSnapshotId()
	t.Cleanup(func() {
		_, _ = ctrl.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
	})

	// The crash: clone the snapshot by hand and stop there, which is precisely
	// the state a controller killed after pool.snapshot.clone leaves behind.
	api, err := reg.Client(ctx, "nas1")
	if err != nil {
		t.Fatalf("client for nas1: %v", err)
	}
	_, zfsSnap, err := backend.SnapshotSource(snapID)
	if err != nil {
		t.Fatalf("parse snapshot id %q: %v", snapID, err)
	}
	const cloneName = "resume-clone"
	clonePath := pool + "/" + parent + "/" + cloneName
	if err := api.SnapshotClone(ctx, zfsSnap, clonePath, nil); err != nil {
		t.Fatalf("planting the abandoned clone: %v", err)
	}
	t.Cleanup(func() { _ = api.DatasetDelete(context.Background(), clonePath, true, true) })

	planted, err := api.DatasetQuery(ctx, clonePath)
	if err != nil || planted == nil {
		t.Fatalf("query planted clone: %v", err)
	}
	if _, marked := planted.UserProperties[volume.OwnerProperty]; marked {
		t.Fatal("the planted clone carries an ownership marker — the premise of " +
			"this test is that a clone inherits none")
	}
	if planted.RefQuota.Parsed != 0 {
		t.Fatalf("the planted clone carries refquota %d — the premise of this "+
			"test is that a clone inherits none", planted.RefQuota.Parsed)
	}

	// The retry. Before the fix this answered AlreadyExists, forever.
	out, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: cloneName, Parameters: params, VolumeCapabilities: caps,
		CapacityRange: &csipb.CapacityRange{RequiredBytes: size},
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID},
			},
		},
	})
	if err != nil {
		t.Fatalf("retry after a crash mid-clone: %v", err)
	}
	t.Cleanup(func() {
		_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
			VolumeId: out.GetVolume().GetVolumeId()})
	})

	repaired, err := api.DatasetQuery(ctx, clonePath)
	if err != nil || repaired == nil {
		t.Fatalf("query resumed clone: %v", err)
	}
	if got := repaired.UserProperties[volume.OwnerProperty].Value; got != volume.OwnerValue {
		t.Errorf("resumed clone marker = %q, want %q — the delete guard would "+
			"refuse to remove this volume forever", got, volume.OwnerValue)
	}
	if got := repaired.UserProperties[volume.ProtocolProperty].Value; got != "nfs" {
		t.Errorf("resumed clone protocol = %q, want nfs", got)
	}
	if repaired.RefQuota.Parsed != size {
		t.Errorf("resumed clone refquota = %d, want %d", repaired.RefQuota.Parsed, size)
	}
	// And it must now be deletable, which is the whole point of the marker.
	if _, err := ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: out.GetVolume().GetVolumeId()}); err != nil {
		t.Errorf("resumed clone could not be deleted: %v", err)
	}
}

// TestDependentCloneGuardAgainstHardware proves the delete guards actually fire
// against the appliance.
//
// Both were dead for as long as they read origin's display form, and both
// passed their unit tests the whole time because the fakes echoed the origin
// back verbatim. A guard that only ever runs against a stand-in proves nothing,
// so this one runs against real ZFS: a real clone of a real snapshot, and the
// refusal has to name it.
func TestDependentCloneGuardAgainstHardware(t *testing.T) {
	endpoint := os.Getenv("TRUENAS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TRUENAS_ENDPOINT is not set: skipping the dependent-clone guard " +
			"check against real hardware")
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	pool := get("TRUENAS_POOL", "Pool0")
	parent := get("TRUENAS_SANITY_PARENT", "csi-sanity")
	cfg := &config.Config{NodeID: "guard-hw", Backends: map[string]config.Backend{
		"nas1": {
			Name: "nas1", Endpoint: endpoint,
			Username:           get("TRUENAS_USERNAME", "truenas_admin"),
			APIKey:             os.Getenv("TRUENAS_API_KEY"),
			Pool:               pool,
			ParentDataset:      parent,
			InsecureSkipVerify: os.Getenv("TRUENAS_INSECURE") == "true",
		},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid appliance configuration: %v", err)
	}
	ctx := context.Background()
	reg, err := backend.NewRegistry(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ctrl := csi.NewController(reg, cfg)
	params := map[string]string{
		"backend": "nas1", "protocol": "nfs",
		"server": get("TRUENAS_DATA_ADDR", "192.168.10.253"),
	}
	caps := []*csipb.VolumeCapability{{
		AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}}
	create := func(name string, src *csipb.VolumeContentSource) string {
		t.Helper()
		out, err := ctrl.CreateVolume(ctx, &csipb.CreateVolumeRequest{
			Name: name, Parameters: params, VolumeCapabilities: caps,
			CapacityRange:       &csipb.CapacityRange{RequiredBytes: 1 << 30},
			VolumeContentSource: src,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		id := out.GetVolume().GetVolumeId()
		t.Cleanup(func() {
			_, _ = ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: id})
		})
		return id
	}

	srcID := create("guard-src", nil)
	snap, err := ctrl.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: srcID, Name: "guard-snap",
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	snapID := snap.GetSnapshot().GetSnapshotId()
	t.Cleanup(func() {
		_, _ = ctrl.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
	})

	cloneID := create("guard-clone", &csipb.VolumeContentSource{
		Type: &csipb.VolumeContentSource_Snapshot{
			Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID},
		},
	})

	// The snapshot now has a real dependent clone. The guard must refuse by
	// name, rather than letting the middleware refuse with a raw EINVAL.
	_, err = ctrl.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteSnapshot with a dependent clone: want FailedPrecondition, got %v", err)
	}
	if !strings.Contains(err.Error(), "guard-clone") {
		t.Errorf("the refusal must name the volume blocking the delete, got: %v", err)
	}

	// And once the clone is gone the snapshot deletes cleanly, which is what
	// proves the guard was reading a real dependency rather than refusing
	// everything.
	if _, err := ctrl.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: cloneID}); err != nil {
		t.Fatalf("delete the clone: %v", err)
	}
	if _, err := ctrl.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("delete the snapshot once its clone is gone: %v", err)
	}
}
