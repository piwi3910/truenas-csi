package replication

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// nas is a stateful stand-in for one TrueNAS appliance: enough dataset,
// snapshot, snapshot-task and replication-task bookkeeping to exercise the
// safety properties, and enough recording to prove which calls did NOT happen.
type nas struct {
	*fake.Server

	name   string
	pool   string
	parent string

	mu        sync.Mutex
	datasets  map[string]*fakeDataset
	snapshots map[string][]string // dataset -> snapshot names, oldest first

	replTasks map[int]*fakeReplTask
	snapTasks map[int]*fakeSnapTask
	nextID    int

	datasetCreates []map[string]any
	datasetUpdates []datasetUpdate
	datasetDeletes []string
	promotions     []string
	replCreates    []map[string]any
	replUpdates    []replUpdate
	replRuns       []int
	snapCreates    []map[string]any
}

type datasetUpdate struct {
	ID    string
	Patch map[string]any
}

type replUpdate struct {
	ID    int
	Patch map[string]any
}

type fakeDataset struct {
	typ      string
	marker   string
	source   string
	origin   string
	readonly string
	volsize  int64
	refquota int64
}

type fakeReplTask struct {
	id      int
	payload map[string]any
	enabled bool
	state   string
}

type fakeSnapTask struct {
	id      int
	dataset string
	schema  string
	enabled bool
}

func (d *fakeDataset) json(id string) map[string]any {
	props := map[string]any{}
	if d.marker != "" {
		props[volume.OwnerProperty] = map[string]any{"value": d.marker, "source": d.source}
	}
	typ := d.typ
	if typ == "" {
		typ = "FILESYSTEM"
	}
	return map[string]any{
		"id":              id,
		"type":            typ,
		"mountpoint":      "/mnt/" + id,
		"origin":          map[string]any{"value": d.origin, "source": "LOCAL"},
		"volsize":         map[string]any{"parsed": d.volsize},
		"refquota":        map[string]any{"parsed": d.refquota},
		"user_properties": props,
	}
}

func newNAS(t *testing.T, name, pool, parent string) *nas {
	t.Helper()
	n := &nas{
		Server:    fake.Start(t, fake.Options{}),
		name:      name,
		pool:      pool,
		parent:    parent,
		datasets:  map[string]*fakeDataset{},
		snapshots: map[string][]string{},
		replTasks: map[int]*fakeReplTask{},
		snapTasks: map[int]*fakeSnapTask{},
	}

	n.Handle(mDatasetQuery, func(p []json.RawMessage) (any, error) {
		id := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return []any{}, nil
		}
		return []any{ds.json(id)}, nil
	})

	n.Handle(mDatasetCreate, func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		id, _ := payload["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.datasetCreates = append(n.datasetCreates, payload)
		ds := &fakeDataset{}
		if typ, ok := payload["type"].(string); ok {
			ds.typ = typ
		}
		if v, ok := payload["volsize"].(float64); ok {
			ds.volsize = int64(v)
		}
		if v, ok := payload["refquota"].(float64); ok {
			ds.refquota = int64(v)
		}
		applyMarker(payload["user_properties"], ds)
		n.datasets[id] = ds
		return ds.json(id), nil
	})

	n.Handle(mDatasetUpdate, func(p []json.RawMessage) (any, error) {
		var id string
		var patch map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] " + id}
		}
		n.datasetUpdates = append(n.datasetUpdates, datasetUpdate{ID: id, Patch: patch})
		if ro, ok := patch["readonly"].(string); ok {
			ds.readonly = ro
		}
		applyMarker(patch["user_properties_update"], ds)
		return ds.json(id), nil
	})

	n.Handle(mDatasetDelete, func(p []json.RawMessage) (any, error) {
		var id string
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.datasetDeletes = append(n.datasetDeletes, id)
		delete(n.datasets, id)
		return true, nil
	})

	n.Handle(mDatasetPromote, func(p []json.RawMessage) (any, error) {
		var id string
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.promotions = append(n.promotions, id)
		return nil, nil
	})

	n.Handle(mSnapshotQuery, func(p []json.RawMessage) (any, error) {
		dataset := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for i, name := range n.snapshots[dataset] {
			out = append(out, map[string]any{
				"id":        dataset + "@" + name,
				"name":      name,
				"dataset":   dataset,
				"createtxg": fmt.Sprint(i + 1),
			})
		}
		return out, nil
	})

	n.Handle(mSnapshotCreate, func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		ds, _ := payload["dataset"].(string)
		name, _ := payload["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.snapCreates = append(n.snapCreates, payload)
		n.snapshots[ds] = append(n.snapshots[ds], name)
		return map[string]any{"id": ds + "@" + name}, nil
	})

	n.Handle(mSnapshotClone, func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		snap, _ := payload["snapshot"].(string)
		dst, _ := payload["dataset_dst"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		// A real ZFS clone inherits NEITHER the ownership marker NOR the quota.
		n.datasets[dst] = &fakeDataset{origin: snap}
		return true, nil
	})

	n.Handle(mSnapshotTaskQuery, func(p []json.RawMessage) (any, error) {
		dataset := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for _, st := range n.snapTasks {
			if st.dataset == dataset {
				out = append(out, map[string]any{
					"id": st.id, "dataset": st.dataset,
					"naming_schema": st.schema, "enabled": st.enabled,
				})
			}
		}
		return out, nil
	})

	n.Handle(mSnapshotTaskCreate, func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.nextID++
		st := &fakeSnapTask{id: n.nextID, enabled: true}
		st.dataset, _ = payload["dataset"].(string)
		st.schema, _ = payload["naming_schema"].(string)
		n.snapTasks[st.id] = st
		return map[string]any{
			"id": st.id, "dataset": st.dataset, "naming_schema": st.schema, "enabled": st.enabled,
		}, nil
	})

	n.Handle(mSnapshotTaskUpdate, func(p []json.RawMessage) (any, error) {
		var id int
		var patch map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		st, ok := n.snapTasks[id]
		if !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] snapshot task"}
		}
		if e, ok := patch["enabled"].(bool); ok {
			st.enabled = e
		}
		return map[string]any{"id": st.id}, nil
	})

	n.Handle(mSnapshotTaskDelete, func(p []json.RawMessage) (any, error) {
		var id int
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.snapTasks, id)
		return true, nil
	})

	n.Handle(mReplicationQuery, func(p []json.RawMessage) (any, error) {
		var filters [][]any
		mustJSON(t, p[0], &filters)
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for _, task := range n.replTasks {
			if !replMatches(task, filters) {
				continue
			}
			out = append(out, task.json())
		}
		return out, nil
	})

	n.Handle(mReplicationCreate, func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.replCreates = append(n.replCreates, payload)
		n.nextID++
		task := &fakeReplTask{id: n.nextID, payload: payload, enabled: true, state: "PENDING"}
		if e, ok := payload["enabled"].(bool); ok {
			task.enabled = e
		}
		n.replTasks[task.id] = task
		return task.json(), nil
	})

	n.Handle(mReplicationUpdate, func(p []json.RawMessage) (any, error) {
		var id int
		var patch map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		task, ok := n.replTasks[id]
		if !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] replication task"}
		}
		n.replUpdates = append(n.replUpdates, replUpdate{ID: id, Patch: patch})
		if e, ok := patch["enabled"].(bool); ok {
			task.enabled = e
		}
		return task.json(), nil
	})

	n.Handle(mReplicationDelete, func(p []json.RawMessage) (any, error) {
		var id int
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.replTasks, id)
		return true, nil
	})

	n.Handle(mReplicationRun, func(p []json.RawMessage) (any, error) {
		var id int
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.replRuns = append(n.replRuns, id)
		if task, ok := n.replTasks[id]; ok {
			task.state = "SUCCESS"
		}
		return nil, nil
	})

	return n
}

func (task *fakeReplTask) json() map[string]any {
	name, _ := task.payload["name"].(string)
	direction, _ := task.payload["direction"].(string)
	target, _ := task.payload["target_dataset"].(string)
	sources, _ := task.payload["source_datasets"].([]any)
	return map[string]any{
		"id": task.id, "name": name, "direction": direction,
		"transport": "SSH", "source_datasets": sources, "target_dataset": target,
		"enabled": task.enabled, "auto": true,
		"state": map[string]any{"state": task.state},
	}
}

func replMatches(task *fakeReplTask, filters [][]any) bool {
	for _, f := range filters {
		if len(f) < 3 {
			continue
		}
		field, _ := f[0].(string)
		switch field {
		case "name":
			want, _ := f[2].(string)
			if name, _ := task.payload["name"].(string); name != want {
				return false
			}
		case "id":
			want, ok := f[2].(float64)
			if !ok || int(want) != task.id {
				return false
			}
		}
	}
	return true
}

func applyMarker(raw any, ds *fakeDataset) {
	props, ok := raw.([]any)
	if !ok {
		return
	}
	for _, p := range props {
		m, _ := p.(map[string]any)
		if m["key"] == volume.OwnerProperty {
			ds.marker, _ = m["value"].(string)
			ds.source = "LOCAL"
		}
	}
}

// put installs a dataset that this driver created.
func (n *nas) putOwned(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.datasets[id] = &fakeDataset{marker: volume.OwnerValue, source: "LOCAL", readonly: "ON"}
}

// putForeign installs a dataset the driver did not create — someone's real data.
func (n *nas) putForeign(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.datasets[id] = &fakeDataset{}
}

// putInherited installs a dataset carrying an INHERITED marker, which is not
// proof of ownership: a marker set by hand on the parent would otherwise clear
// every pre-existing dataset beneath it for replication.
func (n *nas) putInherited(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.datasets[id] = &fakeDataset{marker: volume.OwnerValue, source: "INHERITED"}
}

func (n *nas) addSnapshot(dataset, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.snapshots[dataset] = append(n.snapshots[dataset], name)
}

func (n *nas) dataset(id string) *fakeDataset {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.datasets[id]
}

func (n *nas) replTaskByName(name string) *fakeReplTask {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, t := range n.replTasks {
		if got, _ := t.payload["name"].(string); got == name {
			return t
		}
	}
	return nil
}

// readonlyWrites counts pool.dataset.update calls that cleared readonly on id.
// Promotion of a replication target IS that write, so counting it is how the
// tests tell one promotion from two.
func (n *nas) readonlyWrites(id string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, u := range n.datasetUpdates {
		if u.ID != id {
			continue
		}
		if ro, ok := u.Patch["readonly"].(string); ok && strings.EqualFold(ro, "OFF") {
			count++
		}
	}
	return count
}

func (n *nas) counts() (deletes, promotions, replCreates, replUpdates, replRuns int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.datasetDeletes), len(n.promotions), len(n.replCreates), len(n.replUpdates), len(n.replRuns)
}

func filterValue(t *testing.T, p []json.RawMessage) string {
	t.Helper()
	var filters [][]any
	mustJSON(t, p[0], &filters)
	if len(filters) == 0 || len(filters[0]) < 3 {
		t.Fatalf("unexpected query filters %s", string(p[0]))
	}
	s, _ := filters[0][2].(string)
	return s
}

func mustJSON(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", string(raw), err)
	}
}

// appliances wires the fake appliances into the Appliances interface.
type appliances struct {
	clients  map[string]*truenas.Client
	backends map[string]config.Backend
}

func (a *appliances) Client(_ context.Context, name string) (*truenas.Client, error) {
	c, ok := a.clients[name]
	if !ok {
		return nil, fmt.Errorf("no such backend %q", name)
	}
	return c, nil
}

func (a *appliances) Backend(name string) (config.Backend, error) {
	b, ok := a.backends[name]
	if !ok {
		return config.Backend{}, fmt.Errorf("no such backend %q", name)
	}
	return b, nil
}

// pair builds a source and a target appliance and the Manager over them.
func pair(t *testing.T) (src, dst *nas, m *Manager) {
	t.Helper()
	src = newNAS(t, "nas1", "Pool0", "k8s")
	dst = newNAS(t, "nas2", "Pool1", "k8s")
	a := &appliances{clients: map[string]*truenas.Client{}, backends: map[string]config.Backend{}}
	for _, n := range []*nas{src, dst} {
		c, err := truenas.Dial(context.Background(), config.Backend{
			Name: n.name, Endpoint: n.URL(), Username: "truenas_admin", APIKey: "1-secret",
			Pool: n.pool, ParentDataset: n.parent, InsecureSkipVerify: true,
		})
		if err != nil {
			t.Fatalf("Dial %s: %v", n.name, err)
		}
		t.Cleanup(func() { _ = c.Close() })
		a.clients[n.name] = c
		a.backends[n.name] = config.Backend{
			Name: n.name, Endpoint: n.URL(), Pool: n.pool, ParentDataset: n.parent,
		}
	}
	return src, dst, NewManager(a, NewMemoryStore())
}

func testVolume(name string) volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s", Name: name}
}

func testGroup(vols ...string) Group {
	g := Group{
		Name:                   "group1",
		SourceBackend:          "nas1",
		TargetBackend:          "nas2",
		SSHCredentialID:        7,
		SSHCredentialIDReverse: 8,
		Schedule:               Cron{Minute: "*/15"},
	}
	for _, v := range vols {
		g.Volumes = append(g.Volumes, testVolume(v))
	}
	return g
}
