// Package sanity_test runs the CSI specification conformance suite against the
// driver, backed by an in-process fake TrueNAS appliance.
package sanity_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kubernetes-csi/csi-test/v5/pkg/sanity"
	"github.com/piwi3910/truenas-csi/internal/backend"
	_ "github.com/piwi3910/truenas-csi/internal/backend/iscsi"
	_ "github.com/piwi3910/truenas-csi/internal/backend/nfs"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/server"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// nas is a stateful fake appliance: enough of pool.dataset and pool.snapshot for
// the conformance suite to create, list, snapshot and delete volumes.
type nas struct {
	mu       sync.Mutex
	datasets map[string]map[string]any
	snaps    map[string]map[string]any
}

func newNAS() *nas {
	return &nas{datasets: map[string]map[string]any{}, snaps: map[string]map[string]any{}}
}

func arg(params []json.RawMessage, i int, out any) error {
	if i >= len(params) {
		return fmt.Errorf("missing argument %d", i)
	}
	return json.Unmarshal(params[i], out)
}

func (n *nas) install(s *fake.Server) {
	s.HandleValue("system.info", map[string]any{"version": "25.10.6"})
	s.HandleValue("pool.query", []any{map[string]any{
		"name": "Pool0", "status": "ONLINE", "healthy": true,
		"free": map[string]any{"parsed": int64(44861949222912)},
		"size": map[string]any{"parsed": int64(72000831750144)},
	}})
	s.HandleValue("pool.dataset.recommended_zvol_blocksize", "128K")
	s.HandleValue("filesystem.setperm", 1)
	s.HandleValue("core.get_jobs", []any{map[string]any{"id": 1, "state": "SUCCESS"}})
	s.HandleValue("sharing.nfs.query", []any{})
	s.HandleValue("sharing.nfs.create", map[string]any{"id": 1, "path": "/mnt/Pool0/k8s/x"})
	s.HandleValue("sharing.nfs.delete", nil)

	s.Handle("pool.dataset.create", func(p []json.RawMessage) (any, error) {
		var spec map[string]any
		if err := arg(p, 0, &spec); err != nil {
			return nil, err
		}
		name, _ := spec["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, exists := n.datasets[name]; exists {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "[EEXIST] Path /mnt/" + name + " already exists"}
		}
		ds := map[string]any{
			"id": name, "type": spec["type"], "mountpoint": "/mnt/" + name,
			"user_properties": map[string]any{},
		}
		if v, ok := spec["volsize"]; ok {
			ds["volsize"] = map[string]any{"parsed": v}
		}
		if v, ok := spec["refquota"]; ok {
			ds["refquota"] = map[string]any{"parsed": v}
		}
		if props, ok := spec["user_properties"].([]any); ok {
			up := map[string]any{}
			for _, raw := range props {
				if m, ok := raw.(map[string]any); ok {
					up[fmt.Sprint(m["key"])] = map[string]any{
						"value": m["value"], "source": "LOCAL"}
				}
			}
			ds["user_properties"] = up
		}
		n.datasets[name] = ds
		return ds, nil
	})

	s.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		var filters [][]any
		_ = arg(p, 0, &filters)
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for id, ds := range n.datasets {
			if len(filters) == 0 {
				out = append(out, ds)
				continue
			}
			f := filters[0]
			if len(f) < 3 {
				continue
			}
			op, want := fmt.Sprint(f[1]), fmt.Sprint(f[2])
			if (op == "=" && id == want) || (op == "^" && strings.HasPrefix(id, want)) {
				out = append(out, ds)
			}
		}
		return out, nil
	})

	s.Handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		var id string
		var patch map[string]any
		_ = arg(p, 0, &id)
		_ = arg(p, 1, &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "[ENOENT] None: PoolDataset " + id + " does not exist"}
		}
		for _, k := range []string{"volsize", "refquota"} {
			if v, ok := patch[k]; ok {
				ds[k] = map[string]any{"parsed": v}
			}
		}
		if props, ok := patch["user_properties_update"].([]any); ok {
			up, _ := ds["user_properties"].(map[string]any)
			if up == nil {
				up = map[string]any{}
			}
			for _, raw := range props {
				if m, ok := raw.(map[string]any); ok {
					up[fmt.Sprint(m["key"])] = map[string]any{
						"value": m["value"], "source": "LOCAL"}
				}
			}
			ds["user_properties"] = up
		}
		return ds, nil
	})

	s.Handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		var id string
		_ = arg(p, 0, &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := n.datasets[id]; !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "[ENOENT] None: PoolDataset " + id + " does not exist"}
		}
		delete(n.datasets, id)
		return nil, nil
	})

	s.Handle("pool.snapshot.create", func(p []json.RawMessage) (any, error) {
		var spec map[string]any
		_ = arg(p, 0, &spec)
		id := fmt.Sprintf("%v@%v", spec["dataset"], spec["name"])
		n.mu.Lock()
		defer n.mu.Unlock()
		snap := map[string]any{"id": id, "name": id, "dataset": spec["dataset"]}
		n.snaps[id] = snap
		return snap, nil
	})

	s.Handle("pool.snapshot.query", func(p []json.RawMessage) (any, error) {
		var filters [][]any
		_ = arg(p, 0, &filters)
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for id, sn := range n.snaps {
			if len(filters) == 0 {
				out = append(out, sn)
				continue
			}
			f := filters[0]
			if len(f) < 3 {
				continue
			}
			field, op, want := fmt.Sprint(f[0]), fmt.Sprint(f[1]), fmt.Sprint(f[2])
			switch {
			case field == "id" && op == "=" && id == want:
				out = append(out, sn)
			case field == "dataset" && op == "^" && strings.HasPrefix(fmt.Sprint(sn["dataset"]), want):
				out = append(out, sn)
			}
		}
		return out, nil
	})

	s.Handle("pool.snapshot.delete", func(p []json.RawMessage) (any, error) {
		var id string
		_ = arg(p, 0, &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.snaps, id)
		return nil, nil
	})

	s.Handle("pool.snapshot.clone", func(p []json.RawMessage) (any, error) {
		var spec map[string]any
		_ = arg(p, 0, &spec)
		dst := fmt.Sprint(spec["dataset_dst"])
		n.mu.Lock()
		defer n.mu.Unlock()
		// A clone inherits neither the ownership marker nor the quota; the
		// driver must stamp both itself, and the suite exercises that path.
		n.datasets[dst] = map[string]any{
			"id": dst, "type": "FILESYSTEM", "mountpoint": "/mnt/" + dst,
			"user_properties": map[string]any{},
			"origin": map[string]any{
				"value": spec["snapshot"], "parsed": spec["snapshot"], "source": "NONE"},
		}
		return nil, nil
	})
}

// TestCSISanity runs the CSI conformance suite against Identity and Controller.
//
// The Node service is exercised by internal/node's own tests: its operations
// mount real filesystems through host binaries, which a unit-test host neither
// has nor should be asked to provide.
func TestCSISanity(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	newNAS().install(s)

	cfg := &config.Config{NodeID: "worker-21", Backends: map[string]config.Backend{
		"nas1": {Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-x",
			Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true},
	}}
	reg, err := backend.NewRegistry(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	dir, err := os.MkdirTemp("/tmp", "tncsi-sanity")
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
	// The conformance suite exercises NFS volumes, so declare the tooling that
	// path needs as present on this synthetic host.
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
			"backend": "nas1", "protocol": "nfs", "server": "192.168.10.253",
		},
		// The suite exercises MODIFY_VOLUME only when the driver advertises it,
		// and only with the parameters given here. sync=standard is chosen
		// because it is the default the appliance already applies, so the
		// suite's volumes are provisioned with exactly the durability they
		// would have had — while still driving the whole allowlist,
		// normalisation and pool.dataset.update path.
		TestVolumeMutableParameters: map[string]string{"sync": "standard"},
		IDGen:                       &sanity.DefaultIDGenerator{},
		// Without this the suite sends an EMPTY starting token, which is not
		// invalid at all — the driver correctly treats it as "no token" and the
		// spec then fails for the wrong reason.
		TestInvalidListVolumesStartingToken: "invalid-token",
		// The fake mount creates directories as a real mount would need them to
		// exist, so the suite's own directory setup must tolerate that.
		CreateTargetDir: func(p string) (string, error) {
			return p, os.MkdirAll(p, 0o750)
		},
		CreateStagingDir: func(p string) (string, error) {
			return p, os.MkdirAll(p, 0o750)
		},
		RemoveTargetPath:  func(p string) error { return os.RemoveAll(p) },
		RemoveStagingPath: func(p string) error { return os.RemoveAll(p) },
		// csi-sanity does not set transport credentials itself, and a UNIX
		// socket has none to negotiate.
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
		ControllerDialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	})
}

// TestSanityFakeIsRealistic guards the fake itself: a stand-in that never says
// no would let the conformance suite pass against behaviour the appliance does
// not have.
func TestSanityFakeIsRealistic(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	newNAS().install(s)

	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: s.URL(), Username: "u", APIKey: "8-x",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	// A missing dataset must come back as the appliance really reports it:
	// errname EINVAL with the true errno only in the reason text.
	ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/absent")
	if err != nil || ds != nil {
		t.Fatalf("absent dataset should be (nil, nil), got (%v, %v)", ds, err)
	}
	if _, err := c.DatasetCreate(context.Background(), truenas.DatasetSpec{
		Name: "Pool0/k8s/dup", Type: "FILESYSTEM"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DatasetCreate(context.Background(), truenas.DatasetSpec{
		Name: "Pool0/k8s/dup", Type: "FILESYSTEM"}); err == nil {
		t.Fatal("creating the same dataset twice must fail, as it does on the appliance")
	}
	// And a clone must arrive WITHOUT the ownership marker.
	if err := c.SnapshotClone(context.Background(), "Pool0/k8s/dup@s", "Pool0/k8s/clone", nil); err != nil {
		t.Fatal(err)
	}
	cl, err := c.DatasetQuery(context.Background(), "Pool0/k8s/clone")
	if err != nil || cl == nil {
		t.Fatalf("clone query: %v %v", cl, err)
	}
	if cl.Owned(volume.OwnerProperty, volume.OwnerValue) {
		t.Fatal("the fake must reproduce the real behaviour: a clone inherits no marker")
	}
}
