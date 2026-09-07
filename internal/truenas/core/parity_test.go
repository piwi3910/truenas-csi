package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	corefake "github.com/piwi3910/truenas-csi/internal/truenas/core/fake"
	scalefake "github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// Compile-time assertion: both clients are the same thing to the rest of the
// driver. If either one grows or loses a method, this stops compiling.
var (
	_ truenas.API = (*Client)(nil)
	_ truenas.API = (*truenas.Client)(nil)
)

// op is one observable operation run against both implementations.
type op struct {
	name string
	run  func(ctx context.Context, api truenas.API) string
}

// str renders a result the way a caller would observe it: absence, identity and
// error-ness, not the wire shape.
func str(v any, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	switch t := v.(type) {
	case nil:
		return "nil"
	case *truenas.Dataset:
		if t == nil {
			return "nil"
		}
		return fmt.Sprintf("dataset %s type=%s mountpoint=%s", t.ID, t.Type, t.Mountpoint)
	case *truenas.Snapshot:
		if t == nil {
			return "nil"
		}
		return "snapshot " + t.ID
	case *truenas.NFSShare:
		if t == nil {
			return "nil"
		}
		return "share " + t.Path
	case *truenas.ISCSITarget:
		if t == nil {
			return "nil"
		}
		return "target " + t.Name
	case *truenas.Pool:
		if t == nil {
			return "nil"
		}
		return fmt.Sprintf("pool %s free=%d", t.Name, t.Free.Parsed)
	case string:
		return t
	}
	return fmt.Sprintf("%v", v)
}

var parityOps = []op{
	{"query an absent dataset", func(ctx context.Context, a truenas.API) string {
		return str(a.DatasetQuery(ctx, "Pool0/k8s/nope"))
	}},
	{"query an existing dataset", func(ctx context.Context, a truenas.API) string {
		return str(a.DatasetQuery(ctx, "Pool0/k8s/vol1"))
	}},
	{"create a dataset", func(ctx context.Context, a truenas.API) string {
		return str(a.DatasetCreate(ctx, truenas.DatasetSpec{
			Name: "Pool0/k8s/new1", Type: "FILESYSTEM", RefQuota: 1 << 30,
			UserProperties: map[string]string{"io.truenas.csi:managed": "yes"},
		}))
	}},
	{"a created dataset is owned", func(ctx context.Context, a truenas.API) string {
		ds, err := a.DatasetQuery(ctx, "Pool0/k8s/new1")
		if err != nil {
			return "error: " + err.Error()
		}
		// The ownership marker must be LOCAL, not inherited: this is the guard
		// that stops the driver deleting somebody's production dataset.
		return fmt.Sprintf("owned=%t", ds.Owned("io.truenas.csi:managed", "yes"))
	}},
	{"delete an absent dataset", func(ctx context.Context, a truenas.API) string {
		return str(nil, a.DatasetDelete(ctx, "Pool0/k8s/nope", true, true))
	}},
	{"query an absent snapshot", func(ctx context.Context, a truenas.API) string {
		return str(a.SnapshotQuery(ctx, "Pool0/k8s/vol1@nope"))
	}},
	{"query an existing snapshot", func(ctx context.Context, a truenas.API) string {
		return str(a.SnapshotQuery(ctx, "Pool0/k8s/vol1@snap1"))
	}},
	{"query an absent nfs share", func(ctx context.Context, a truenas.API) string {
		return str(a.NFSShareByPath(ctx, "/mnt/Pool0/k8s/nope"))
	}},
	{"query an existing nfs share", func(ctx context.Context, a truenas.API) string {
		return str(a.NFSShareByPath(ctx, "/mnt/Pool0/k8s/vol1"))
	}},
	{"query an absent iscsi target", func(ctx context.Context, a truenas.API) string {
		return str(a.TargetByName(ctx, "nope"))
	}},
	{"query an existing pool", func(ctx context.Context, a truenas.API) string {
		return str(a.PoolQuery(ctx, "Pool0"))
	}},
	{"query an absent pool", func(ctx context.Context, a truenas.API) string {
		p, err := a.PoolQuery(ctx, "Nope")
		if err != nil {
			return "error: pool does not exist"
		}
		return str(p, nil)
	}},
	{"recommended zvol blocksize", func(ctx context.Context, a truenas.API) string {
		return str(a.RecommendedZvolBlocksize(ctx, "Pool0"))
	}},
	{"set a user property", func(ctx context.Context, a truenas.API) string {
		return str(nil, a.SetUserProperty(ctx, "Pool0/k8s/vol1", "io.truenas.csi:managed", "yes"))
	}},
	{"set permissions (job)", func(ctx context.Context, a truenas.API) string {
		return str(nil, a.SetPerm(ctx, "/mnt/Pool0/k8s/vol1", "0770", 1000, 1000))
	}},
	{"list datasets under a prefix", func(ctx context.Context, a truenas.API) string {
		out, err := a.DatasetList(ctx, "Pool0/k8s/")
		if err != nil {
			return "error: " + err.Error()
		}
		ids := make([]string, 0, len(out))
		for _, d := range out {
			ids = append(ids, d.ID)
		}
		sort.Strings(ids)
		return "datasets: " + strings.Join(ids, " ")
	}},
}

// TestCoreImplementsTheSameInterface is the anti-drift test. Both clients are
// driven through the SAME operations against their own fake, and the results
// must be indistinguishable. A CORE-only quirk that leaks into a caller-visible
// difference — an error where SCALE returns nil, a missing mountpoint — fails
// here rather than in a cluster.
func TestCoreImplementsTheSameInterface(t *testing.T) {
	scale := dialScaleFake(t)
	corec := dialFake(t, seededCoreFake(t))

	ctx := context.Background()
	for _, o := range parityOps {
		t.Run(o.name, func(t *testing.T) {
			want := o.run(ctx, scale)
			got := o.run(ctx, corec)
			if want != got {
				t.Fatalf("observable behaviour differs\n scale: %s\n  core: %s", want, got)
			}
		})
	}
}

func seededCoreFake(t *testing.T) *corefake.Server {
	t.Helper()
	s := corefake.Start(t, corefake.Options{})
	s.SeedParity()
	return s
}

// dialScaleFake stands up the SCALE fake seeded with the same objects the CORE
// fake carries, so the two are compared on equal state.
func dialScaleFake(t *testing.T) *truenas.Client {
	t.Helper()
	s := scalefake.Start(t, scalefake.Options{})

	dataset := func(id string) map[string]any {
		return map[string]any{
			"id": id, "name": id, "type": "FILESYSTEM",
			"mountpoint": "/mnt/" + id,
		}
	}
	datasets := map[string]map[string]any{"Pool0/k8s/vol1": dataset("Pool0/k8s/vol1")}

	s.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		return filterByID(p, datasets), nil
	})
	s.Handle("pool.dataset.create", func(p []json.RawMessage) (any, error) {
		var spec struct {
			Name           string              `json:"name"`
			Type           string              `json:"type"`
			UserProperties []map[string]string `json:"user_properties"`
		}
		if len(p) > 0 {
			_ = json.Unmarshal(p[0], &spec)
		}
		d := dataset(spec.Name)
		d["type"] = spec.Type
		// The appliance takes user properties as a list and reports them back as
		// a map with a source; the ownership guard reads the map form.
		props := map[string]any{}
		for _, kv := range spec.UserProperties {
			props[kv["key"]] = map[string]any{"value": kv["value"], "source": "LOCAL"}
		}
		d["user_properties"] = props
		datasets[spec.Name] = d
		return d, nil
	})
	s.Handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		var id string
		if len(p) > 0 {
			_ = json.Unmarshal(p[0], &id)
		}
		d, ok := datasets[id]
		if !ok {
			return nil, &scalefake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "[ENOENT] None: PoolDataset " + id + " does not exist"}
		}
		return d, nil
	})
	s.Handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		var id string
		if len(p) > 0 {
			_ = json.Unmarshal(p[0], &id)
		}
		if _, ok := datasets[id]; !ok {
			return nil, &scalefake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "[ENOENT] None: PoolDataset " + id + " does not exist"}
		}
		delete(datasets, id)
		return true, nil
	})

	snapshots := map[string]map[string]any{
		"Pool0/k8s/vol1@snap1": {"id": "Pool0/k8s/vol1@snap1", "name": "snap1", "dataset": "Pool0/k8s/vol1"},
	}
	s.Handle("pool.snapshot.query", func(p []json.RawMessage) (any, error) {
		return filterByID(p, snapshots), nil
	})

	shares := map[string]map[string]any{
		"/mnt/Pool0/k8s/vol1": {"id": 1, "path": "/mnt/Pool0/k8s/vol1", "enabled": true},
	}
	s.Handle("sharing.nfs.query", func(p []json.RawMessage) (any, error) {
		return filterByField(p, shares, "path"), nil
	})
	s.HandleValue("iscsi.target.query", []any{})
	pools := map[string]map[string]any{
		"Pool0": {"name": "Pool0", "status": "ONLINE", "healthy": true,
			"size": 72000000000000, "free": 44900000000000},
	}
	s.Handle("pool.query", func(p []json.RawMessage) (any, error) {
		return filterByField(p, pools, "name"), nil
	})
	s.HandleValue("pool.dataset.recommended_zvol_blocksize", "128K")
	s.HandleValue("filesystem.setperm", 9615)
	s.HandleValue("core.get_jobs", []any{map[string]any{"id": 9615, "state": "SUCCESS"}})

	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: s.URL(), Username: "truenas_admin", APIKey: "8-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial scale fake: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// filterByID applies the [["id","=",x]] / [["id","^",prefix]] filter the driver
// sends, so the SCALE fake answers like the appliance rather than returning
// everything.
func filterByID(p []json.RawMessage, store map[string]map[string]any) []map[string]any {
	return filterByField(p, store, "id")
}

func filterByField(p []json.RawMessage, store map[string]map[string]any, field string) []map[string]any {
	out := []map[string]any{}
	var filters [][]any
	if len(p) > 0 {
		_ = json.Unmarshal(p[0], &filters)
	}
	for _, item := range store {
		if scaleMatchesAll(item, field, filters) {
			out = append(out, item)
		}
	}
	return out
}

func scaleMatchesAll(item map[string]any, _ string, filters [][]any) bool {
	for _, f := range filters {
		if len(f) != 3 {
			continue
		}
		name := fmt.Sprint(f[0])
		want := fmt.Sprint(f[2])
		got := fmt.Sprint(item[name])
		switch fmt.Sprint(f[1]) {
		case "=":
			if got != want {
				return false
			}
		case "^":
			if len(got) < len(want) || got[:len(want)] != want {
				return false
			}
		}
	}
	return true
}
