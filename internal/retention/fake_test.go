package retention

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// nas is a stateful stand-in for the appliance's dataset surface, layered on the
// transport-level fake so the tests exercise the real client, the real JSON
// shapes and the real error classification rather than a hand-written stub of
// truenas.API.
//
// It reproduces the three behaviours delete protection depends on and would be
// worthless without:
//
//   - pool.dataset.rename moves the dataset AND its snapshots, and leaves its
//     user properties alone. That is what makes the ownership marker survive a
//     retire, and it is why Retire stamps only the two new properties.
//   - user properties are INHERITED: a child reports its parent's property with
//     source INHERITED. Every LOCAL-source rule in the guards is meaningless
//     against a fake that does not model this.
//   - a rename or delete of a dataset with a dependent clone fails EBUSY.
type nas struct {
	*fake.Server

	mu       sync.Mutex
	datasets map[string]*fakeDataset
	// clonedFrom maps a clone's dataset id to the dataset it depends on, which
	// is what makes that dataset refuse to be destroyed.
	clonedFrom map[string]string
	// renameFail, when set, makes the next rename fail with this error.
	renameFail *fake.RPCError
	// forces records the force flag of every rename that reached the box,
	// including the ones that failed.
	forces []bool
}

type fakeDataset struct {
	// props are the LOCAL user properties. Inherited ones are computed.
	props map[string]string
}

func newNAS(t *testing.T) *nas {
	t.Helper()
	n := &nas{
		Server:     fake.Start(t, fake.Options{}),
		datasets:   map[string]*fakeDataset{},
		clonedFrom: map[string]string{},
	}

	n.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for id := range n.datasets {
			if n.matches(id, p) {
				out = append(out, n.jsonOf(id))
			}
		}
		return out, nil
	})

	n.Handle("pool.dataset.create", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		id, _ := payload["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds := &fakeDataset{props: map[string]string{}}
		if raw, ok := payload["user_properties"].([]any); ok {
			for _, e := range raw {
				m, _ := e.(map[string]any)
				k, _ := m["key"].(string)
				v, _ := m["value"].(string)
				ds.props[k] = v
			}
		}
		n.datasets[id] = ds
		return n.jsonOf(id), nil
	})

	n.Handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		var id string
		var patch map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return nil, notFound(id)
		}
		if raw, ok := patch["user_properties_update"].([]any); ok {
			for _, e := range raw {
				m, _ := e.(map[string]any)
				k, _ := m["key"].(string)
				v, _ := m["value"].(string)
				ds.props[k] = v
			}
		}
		return n.jsonOf(id), nil
	})

	n.Handle("pool.dataset.rename", func(p []json.RawMessage) (any, error) {
		var id string
		var opts map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &opts)
		dst, _ := opts["new_name"].(string)
		force, _ := opts["force"].(bool)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.forces = append(n.forces, force)
		if n.renameFail != nil {
			e := n.renameFail
			n.renameFail = nil
			return nil, e
		}
		ds, ok := n.datasets[id]
		if !ok {
			return nil, notFound(id)
		}
		if _, taken := n.datasets[dst]; taken {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[EEXIST] " + dst}
		}
		delete(n.datasets, id)
		n.datasets[dst] = ds
		// Dependencies follow the dataset: renaming is a namespace operation
		// and does not touch the clone relationship.
		for clone, origin := range n.clonedFrom {
			if origin == id {
				n.clonedFrom[clone] = dst
			}
		}
		return nil, nil
	})

	n.Handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		var id string
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := n.datasets[id]; !ok {
			return nil, notFound(id)
		}
		for _, origin := range n.clonedFrom {
			if origin == id {
				return nil, &fake.RPCError{Code: -32001, ErrName: "EBUSY",
					Reason: "[EBUSY] dataset is busy: snapshot has dependent clones"}
			}
		}
		delete(n.datasets, id)
		return true, nil
	})

	return n
}

// jsonOf renders a dataset the way pool.dataset.query does, computing the
// source of every user property from the hierarchy.
func (n *nas) jsonOf(id string) map[string]any {
	props := map[string]any{}
	// Ancestors first, so a LOCAL value overwrites an inherited one.
	for other, ds := range n.datasets {
		if other == id || !strings.HasPrefix(id, other+"/") {
			continue
		}
		for k, v := range ds.props {
			props[k] = map[string]any{"value": v, "source": "INHERITED"}
		}
	}
	for k, v := range n.datasets[id].props {
		props[k] = map[string]any{"value": v, "source": "LOCAL"}
	}
	return map[string]any{
		"id": id, "type": "FILESYSTEM", "mountpoint": "/mnt/" + id,
		"user_properties": props,
	}
}

func (n *nas) matches(id string, p []json.RawMessage) bool {
	var filters [][]any
	if len(p) == 0 || json.Unmarshal(p[0], &filters) != nil {
		return true
	}
	for _, f := range filters {
		if len(f) != 3 {
			continue
		}
		field, _ := f[0].(string)
		want, _ := f[2].(string)
		if field != "id" {
			continue
		}
		switch f[1] {
		case "^":
			if !strings.HasPrefix(id, want) {
				return false
			}
		default:
			if id != want {
				return false
			}
		}
	}
	return true
}

func notFound(id string) *fake.RPCError {
	return &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] None: " + id + " does not exist"}
}

func mustJSON(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", string(raw), err)
	}
}

func (n *nas) client(t *testing.T) truenas.API {
	t.Helper()
	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: n.URL(), Username: "truenas_admin", APIKey: "1-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// put installs a dataset with the given LOCAL user properties.
func (n *nas) put(id string, props map[string]string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if props == nil {
		props = map[string]string{}
	}
	n.datasets[id] = &fakeDataset{props: props}
}

// putRetired installs a dataset shaped exactly as Retire leaves one.
func (n *nas) putRetired(id, from, deletedAt string) {
	n.put(id, map[string]string{
		volume.OwnerProperty:       volume.OwnerValue,
		volume.RetiredFromProperty: from,
		volume.DeletedAtProperty:   deletedAt,
	})
}

// listEverything makes pool.dataset.query ignore its filters and answer with
// every dataset on the box.
//
// It models the appliance answering more broadly than it was asked — a filter
// the middleware ignored, a future schema change, a bug — which is precisely
// the condition under which a reaper that trusted its query would destroy live
// data.
func (n *nas) listEverything() {
	n.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		out := []any{}
		for id := range n.datasets {
			out = append(out, n.jsonOf(id))
		}
		return out, nil
	})
}

// renameForces returns the force flag of every rename the box received.
func (n *nas) renameForces() []bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]bool(nil), n.forces...)
}

func (n *nas) has(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.datasets[id]
	return ok
}

func (n *nas) ids() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.datasets))
	for id := range n.datasets {
		out = append(out, id)
	}
	return out
}

// clone records that clone depends on origin, so origin refuses to be destroyed.
func (n *nas) clone(cloneID, origin string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.clonedFrom[cloneID] = origin
}

func (n *nas) uncloneAll() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.clonedFrom = map[string]string{}
}

// testPolicy is the policy every test in this package works against.
func testPolicy(grace string) Policy {
	return PolicyFor(config.Backend{
		Pool: "Pool0", ParentDataset: "k8s",
		DeleteProtection: config.DeleteProtection{Enabled: true, GracePeriod: grace},
	})
}
