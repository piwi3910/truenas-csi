package sanity_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
