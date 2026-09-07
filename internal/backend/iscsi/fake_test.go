package iscsi

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
)

// nas is a stateful stand-in for the appliance's iSCSI and dataset surface,
// layered on the transport-level fake. Tests assert against the objects it
// holds rather than against a script of expected calls, because the driver's
// contract is "the appliance ends up in this state", not "these calls happen".
type nas struct {
	t *testing.T
	s *fake.Server

	mu       sync.Mutex
	seq      int
	datasets map[string]map[string]any
	portals  []map[string]any
	targets  []map[string]any
	extents  []map[string]any
	texts    []map[string]any
	auths    []map[string]any
	inits    []map[string]any

	// fail makes one method return an error, so rollback paths can be driven.
	fail map[string]*fake.RPCError
}

func newNAS(t *testing.T) *nas {
	t.Helper()
	resetState()
	n := &nas{
		t:        t,
		s:        fake.Start(t, fake.Options{}),
		datasets: map[string]map[string]any{},
		fail:     map[string]*fake.RPCError{},
	}
	n.install()
	return n
}

func (n *nas) client() *truenas.Client {
	n.t.Helper()
	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: n.s.URL(), Username: "u", APIKey: "1-secretkey",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	})
	if err != nil {
		n.t.Fatalf("dial fake: %v", err)
	}
	n.t.Cleanup(func() { _ = c.Close() })
	return c
}

func (n *nas) backend() *iscsiBackend {
	return New(n.client(), "Pool0", "k8s").(*iscsiBackend)
}

func (n *nas) failOn(method string, e *fake.RPCError) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fail[method] = e
}

func (n *nas) clearFail(method string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.fail, method)
}

func (n *nas) nextID() int {
	n.seq++
	return n.seq
}

// --- helpers -----------------------------------------------------------------

func arg[T any](t *testing.T, raw []json.RawMessage, i int) T {
	t.Helper()
	var v T
	if i >= len(raw) {
		return v
	}
	if err := json.Unmarshal(raw[i], &v); err != nil {
		t.Fatalf("decode param %d: %v", i, err)
	}
	return v
}

func norm(v any) string {
	switch t := v.(type) {
	case float64:
		return fmt.Sprintf("%g", t)
	case int:
		return fmt.Sprintf("%g", float64(t))
	case int64:
		return fmt.Sprintf("%g", float64(t))
	default:
		return fmt.Sprintf("%v", v)
	}
}

// matches applies the middleware's query-filter shape, which is all the driver
// ever sends: [[field, op, value], ...] with op "=" or "^".
func matches(item map[string]any, raw []json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var filters [][]any
	if err := json.Unmarshal(raw[0], &filters); err != nil {
		return true
	}
	for _, f := range filters {
		if len(f) != 3 {
			continue
		}
		field, _ := f[0].(string)
		got := norm(item[field])
		want := norm(f[2])
		switch f[1] {
		case "^":
			if !strings.HasPrefix(got, want) {
				return false
			}
		default:
			if got != want {
				return false
			}
		}
	}
	return true
}

func filterItems(items []map[string]any, raw []json.RawMessage) []map[string]any {
	out := []map[string]any{}
	for _, it := range items {
		if matches(it, raw) {
			out = append(out, it)
		}
	}
	return out
}

func notFound(what string) *fake.RPCError {
	return &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] None: " + what + " does not exist"}
}

func alreadyExists(what string) *fake.RPCError {
	return &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[EEXIST] " + what + " already exists"}
}

// --- handlers ----------------------------------------------------------------

func (n *nas) handle(method string, h func(p []json.RawMessage) (any, error)) {
	n.s.Handle(method, func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		e, failing := n.fail[method]
		n.mu.Unlock()
		if failing {
			return nil, e
		}
		return h(p)
	})
}

func (n *nas) install() {
	t := n.t

	n.handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		all := make([]map[string]any, 0, len(n.datasets))
		for _, d := range n.datasets {
			all = append(all, d)
		}
		return filterItems(all, p), nil
	})

	n.handle("pool.dataset.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		id, _ := spec["name"].(string)
		if _, ok := n.datasets[id]; ok {
			return nil, alreadyExists(id)
		}
		props := map[string]any{}
		if raw, ok := spec["user_properties"].([]any); ok {
			for _, e := range raw {
				m, _ := e.(map[string]any)
				k, _ := m["key"].(string)
				props[k] = map[string]any{"value": m["value"], "source": "LOCAL"}
			}
		}
		ds := map[string]any{
			"id":              id,
			"type":            spec["type"],
			"volsize":         map[string]any{"parsed": spec["volsize"]},
			"volblocksize":    map[string]any{"value": spec["volblocksize"], "source": "LOCAL"},
			"sparse":          spec["sparse"],
			"user_properties": props,
		}
		n.datasets[id] = ds
		return ds, nil
	})

	n.handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		id := arg[string](t, p, 0)
		patch := arg[map[string]any](t, p, 1)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return nil, notFound(id)
		}
		if v, ok := patch["volsize"]; ok {
			ds["volsize"] = map[string]any{"parsed": v}
		}
		if raw, ok := patch["user_properties_update"].([]any); ok {
			props, _ := ds["user_properties"].(map[string]any)
			if props == nil {
				props = map[string]any{}
				ds["user_properties"] = props
			}
			for _, e := range raw {
				m, _ := e.(map[string]any)
				k, _ := m["key"].(string)
				props[k] = map[string]any{"value": m["value"], "source": "LOCAL"}
			}
		}
		return ds, nil
	})

	n.handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		id := arg[string](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		if _, ok := n.datasets[id]; !ok {
			return nil, notFound(id)
		}
		delete(n.datasets, id)
		return true, nil
	})

	n.handle("pool.dataset.recommended_zvol_blocksize", func([]json.RawMessage) (any, error) {
		return "128K", nil
	})

	n.handle("pool.snapshot.clone", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		snap, _ := spec["snapshot"].(string)
		dst, _ := spec["dataset_dst"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		origin := snap
		if i := strings.Index(snap, "@"); i >= 0 {
			origin = snap[:i]
		}
		src, ok := n.datasets[origin]
		if !ok {
			return nil, notFound(snap)
		}
		// A clone inherits NEITHER the ownership marker NOR an explicit size
		// stamp — reproducing that here is the whole point of this handler.
		n.datasets[dst] = map[string]any{
			"id": dst, "type": src["type"],
			"volsize":         src["volsize"],
			"origin":          map[string]any{"value": snap, "source": "LOCAL"},
			"user_properties": map[string]any{},
		}
		return true, nil
	})

	n.handle("iscsi.global.config", func([]json.RawMessage) (any, error) {
		return map[string]any{"basename": "iqn.2005-10.org.freenas.ctl", "listen_port": 3260}, nil
	})

	n.handle("iscsi.portal.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.portals, p), nil
	})

	n.handle("iscsi.portal.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		po := map[string]any{"id": n.nextID(), "comment": spec["comment"], "listen": spec["listen"]}
		n.portals = append(n.portals, po)
		return po, nil
	})

	n.handle("iscsi.target.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.targets, p), nil
	})

	n.handle("iscsi.target.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		name, _ := spec["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, tg := range n.targets {
			if tg["name"] == name {
				return nil, alreadyExists("target " + name)
			}
		}
		tg := map[string]any{"id": n.nextID(), "name": name, "groups": spec["groups"]}
		n.targets = append(n.targets, tg)
		return tg, nil
	})

	n.handle("iscsi.target.update", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		patch := arg[map[string]any](t, p, 1)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, tg := range n.targets {
			if norm(tg["id"]) == norm(id) {
				for k, v := range patch {
					tg[k] = v
				}
				return tg, nil
			}
		}
		return nil, notFound("target")
	})

	n.handle("iscsi.target.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, tg := range n.targets {
			if norm(tg["id"]) == norm(id) {
				n.targets = append(n.targets[:i], n.targets[i+1:]...)
				return true, nil
			}
		}
		return nil, notFound("target")
	})

	n.handle("iscsi.extent.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.extents, p), nil
	})

	n.handle("iscsi.extent.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		name, _ := spec["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		// The appliance enforces a 64-character ceiling on extent names.
		if len(name) > 64 {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL",
				Reason: "iscsi_extent_create.name: Value greater than 64 not allowed"}
		}
		for _, e := range n.extents {
			if e["name"] == name {
				return nil, alreadyExists("extent " + name)
			}
		}
		id := n.nextID()
		e := map[string]any{
			"id": id, "name": name, "type": spec["type"], "disk": spec["disk"],
			"naa": fmt.Sprintf("0x6589cfc0000000000000000000000%03d", id),
		}
		n.extents = append(n.extents, e)
		return e, nil
	})

	n.handle("iscsi.extent.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, e := range n.extents {
			if norm(e["id"]) == norm(id) {
				n.extents = append(n.extents[:i], n.extents[i+1:]...)
				return true, nil
			}
		}
		return nil, notFound("extent")
	})

	n.handle("iscsi.targetextent.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.texts, p), nil
	})

	n.handle("iscsi.targetextent.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, te := range n.texts {
			if norm(te["target"]) == norm(spec["target"]) && norm(te["lunid"]) == norm(spec["lunid"]) {
				return nil, alreadyExists(fmt.Sprintf("lun %v on target %v", spec["lunid"], spec["target"]))
			}
		}
		te := map[string]any{"id": n.nextID(), "target": spec["target"], "extent": spec["extent"], "lunid": spec["lunid"]}
		n.texts = append(n.texts, te)
		return te, nil
	})

	n.handle("iscsi.targetextent.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, te := range n.texts {
			if norm(te["id"]) == norm(id) {
				n.texts = append(n.texts[:i], n.texts[i+1:]...)
				return true, nil
			}
		}
		return nil, notFound("targetextent")
	})

	n.handle("iscsi.auth.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.auths, p), nil
	})

	n.handle("iscsi.auth.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		a := map[string]any{"id": n.nextID(), "tag": spec["tag"], "user": spec["user"], "secret": spec["secret"]}
		n.auths = append(n.auths, a)
		return a, nil
	})

	n.handle("iscsi.auth.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, a := range n.auths {
			if norm(a["id"]) == norm(id) {
				n.auths = append(n.auths[:i], n.auths[i+1:]...)
				return true, nil
			}
		}
		return nil, notFound("auth")
	})

	n.handle("iscsi.initiator.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.inits, p), nil
	})

	n.handle("iscsi.initiator.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		ig := map[string]any{"id": n.nextID(), "comment": spec["comment"], "initiators": spec["initiators"]}
		n.inits = append(n.inits, ig)
		return ig, nil
	})

	n.handle("iscsi.initiator.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, ig := range n.inits {
			if norm(ig["id"]) == norm(id) {
				n.inits = append(n.inits[:i], n.inits[i+1:]...)
				return true, nil
			}
		}
		return nil, notFound("initiator")
	})
}

// --- state accessors ---------------------------------------------------------

func (n *nas) hasDataset(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.datasets[id]
	return ok
}

func (n *nas) dataset(id string) map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.datasets[id]
}

func (n *nas) putDataset(ds map[string]any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id, _ := ds["id"].(string)
	n.datasets[id] = ds
}

func (n *nas) counts() (datasets, extents, targets, texts int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.datasets), len(n.extents), len(n.targets), len(n.texts)
}

func (n *nas) initiators() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []string{}
	for _, ig := range n.inits {
		raw, _ := ig["initiators"].([]any)
		for _, v := range raw {
			s, _ := v.(string)
			out = append(out, s)
		}
	}
	return out
}

func (n *nas) authSecrets() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := []string{}
	for _, a := range n.auths {
		s, _ := a["secret"].(string)
		out = append(out, s)
	}
	return out
}

func (n *nas) initiatorGroups() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.inits)
}

// zvol builds a dataset as the appliance would report it, so tests can seed
// pre-existing state — including datasets this driver must refuse to touch.
func zvol(id string, size int64, marker, source string) map[string]any {
	props := map[string]any{}
	if marker != "" {
		props["io.truenas.csi:managed"] = map[string]any{"value": marker, "source": source}
	}
	return map[string]any{
		"id": id, "type": "VOLUME",
		"volsize":         map[string]any{"parsed": float64(size)},
		"user_properties": props,
	}
}
