package nvme

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// nas is a stateful stand-in for the appliance's nvmet and dataset surface,
// layered on the transport-level fake. Tests assert against the objects it
// holds rather than against a script of calls: the contract is "the appliance
// ends up in this state", not "these methods were invoked".
type nas struct {
	t *testing.T
	s *fake.Server

	mu         sync.Mutex
	seq        int
	rdma       bool
	datasets   map[string]map[string]any
	subsys     []map[string]any
	namespaces []map[string]any
	ports      []map[string]any
	portSubsys []map[string]any
	hosts      []map[string]any
	hostSubsys []map[string]any

	fail map[string]*fake.RPCError
}

func newNAS(t *testing.T) *nas {
	t.Helper()
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

func (n *nas) backend() *nvmeBackend {
	return New(n.client(), backend.Options{Pool: "Pool0", Parent: "k8s"}).(*nvmeBackend)
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

// matches applies the middleware's query-filter shape: [[field, op, value], ...].
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
		if norm(item[field]) != norm(f[2]) {
			return false
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

// listHandler returns a reader of the fake's own state for one query method,
// so a test can make the FIRST list miss what the appliance already holds --
// which is the shape of a lost check-then-act race, and the only way to reach
// the create that then fails.
func (n *nas) listHandler(method string) func(p []json.RawMessage) (any, error) {
	items := func() []map[string]any { return nil }
	switch method {
	case "nvmet.port_subsys.query":
		items = func() []map[string]any { return n.portSubsys }
	case "nvmet.host_subsys.query":
		items = func() []map[string]any { return n.hostSubsys }
	}
	return func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(items(), p), nil
	}
}

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

// deleteByID removes the item with the given id from a slice of objects.
func deleteByID(items []map[string]any, id float64) ([]map[string]any, bool) {
	for i, it := range items {
		if norm(it["id"]) == norm(id) {
			return append(items[:i:i], items[i+1:]...), true
		}
	}
	return items, false
}

// --- handlers ----------------------------------------------------------------

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
		if v, ok := patch["comments"]; ok {
			ds["comments"] = map[string]any{"value": v, "source": "LOCAL"}
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
		// A clone inherits NEITHER the ownership marker NOR any user property
		// of its origin — reproducing that here is the point of this handler.
		// It inherits no refreservation either, so a thick source yields a THIN
		// clone unless the caller asks for one at clone time. Verified on a
		// real appliance: a 1 GiB thick zvol reserved 1246007808 bytes and its
		// clone reserved nothing.
		clone := map[string]any{
			"id": dst, "type": src["type"],
			"volsize": src["volsize"],
			// The middleware UPPERCASES origin's display form and keeps the
			// true name only in rawvalue — verified on a real appliance.
			"origin": map[string]any{
				"value": strings.ToUpper(snap), "rawvalue": snap, "source": "LOCAL"},
			"user_properties": map[string]any{},
		}
		if props, ok := spec["dataset_properties"].(map[string]any); ok {
			if r, ok := props["refreservation"]; ok {
				clone["refreservation"] = map[string]any{"value": r, "source": "LOCAL"}
			}
		}
		n.datasets[dst] = clone
		return true, nil
	})

	n.handle("pool.dataset.recommended_zvol_blocksize", func([]json.RawMessage) (any, error) {
		return "128K", nil
	})

	n.handle("nvmet.global.config", func([]json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return map[string]any{
			"basenqn": "nqn.2011-06.com.truenas:uuid:6ab80cc6-0000-0000-0000-000000000000",
			"kernel":  true, "ana": false, "rdma": n.rdma,
		}, nil
	})

	n.handle("nvmet.subsys.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.subsys, p), nil
	})

	n.handle("nvmet.subsys.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		name, _ := spec["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, s := range n.subsys {
			if s["name"] == name {
				return nil, alreadyExists("subsystem " + name)
			}
		}
		id := n.nextID()
		s := map[string]any{
			"id": id, "name": name,
			"subnqn":         "nqn.2011-06.com.truenas:uuid:6ab80cc6-0000-0000-0000-000000000000:" + name,
			"serial":         fmt.Sprintf("%020x", id),
			"allow_any_host": spec["allow_any_host"],
		}
		n.subsys = append(n.subsys, s)
		return s, nil
	})

	n.handle("nvmet.subsys.update", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		patch := arg[map[string]any](t, p, 1)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, sub := range n.subsys {
			if norm(sub["id"]) != norm(id) {
				continue
			}
			for k, v := range patch {
				sub[k] = v
			}
			return sub, nil
		}
		return nil, notFound("subsystem")
	})

	n.handle("nvmet.subsys.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		out, ok := deleteByID(n.subsys, id)
		if !ok {
			return nil, notFound("subsystem")
		}
		n.subsys = out
		return true, nil
	})

	n.handle("nvmet.namespace.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.namespaces, p), nil
	})

	n.handle("nvmet.namespace.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		path, _ := spec["device_path"].(string)
		for _, ns := range n.namespaces {
			if ns["device_path"] == path {
				return nil, alreadyExists("namespace " + path)
			}
		}
		ns := map[string]any{
			"id": n.nextID(), "subsys_id": spec["subsys_id"],
			"device_type": spec["device_type"], "device_path": path,
		}
		n.namespaces = append(n.namespaces, ns)
		return ns, nil
	})

	n.handle("nvmet.namespace.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		out, ok := deleteByID(n.namespaces, id)
		if !ok {
			return nil, notFound("namespace")
		}
		n.namespaces = out
		return true, nil
	})

	n.handle("nvmet.port.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.ports, p), nil
	})

	n.handle("nvmet.port.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		po := map[string]any{
			"id": n.nextID(), "addr_trtype": spec["addr_trtype"],
			"addr_traddr": spec["addr_traddr"], "addr_trsvcid": spec["addr_trsvcid"],
		}
		n.ports = append(n.ports, po)
		return po, nil
	})

	n.handle("nvmet.port.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		out, ok := deleteByID(n.ports, id)
		if !ok {
			return nil, notFound("port")
		}
		n.ports = out
		return true, nil
	})

	n.handle("nvmet.port_subsys.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.portSubsys, p), nil
	})

	n.handle("nvmet.port_subsys.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		ps := map[string]any{"id": n.nextID(), "port_id": spec["port_id"], "subsys_id": spec["subsys_id"]}
		n.portSubsys = append(n.portSubsys, ps)
		return ps, nil
	})

	n.handle("nvmet.port_subsys.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		out, ok := deleteByID(n.portSubsys, id)
		if !ok {
			return nil, notFound("port_subsys")
		}
		n.portSubsys = out
		return true, nil
	})

	n.handle("nvmet.host.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.hosts, p), nil
	})

	n.handle("nvmet.host.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		h := map[string]any{"id": n.nextID(), "hostnqn": spec["hostnqn"]}
		n.hosts = append(n.hosts, h)
		return h, nil
	})

	n.handle("nvmet.host_subsys.query", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		return filterItems(n.hostSubsys, p), nil
	})

	n.handle("nvmet.host_subsys.create", func(p []json.RawMessage) (any, error) {
		spec := arg[map[string]any](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		hs := map[string]any{"id": n.nextID(), "host_id": spec["host_id"], "subsys_id": spec["subsys_id"]}
		n.hostSubsys = append(n.hostSubsys, hs)
		return hs, nil
	})

	n.handle("nvmet.host_subsys.delete", func(p []json.RawMessage) (any, error) {
		id := arg[float64](t, p, 0)
		n.mu.Lock()
		defer n.mu.Unlock()
		out, ok := deleteByID(n.hostSubsys, id)
		if !ok {
			return nil, notFound("host_subsys")
		}
		n.hostSubsys = out
		return true, nil
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

// counts reports the objects a volume is made of, in creation order.
func (n *nas) counts() (datasets, subsystems, namespaces, ports, portSubsys int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.datasets), len(n.subsys), len(n.namespaces), len(n.ports), len(n.portSubsys)
}

func (n *nas) aclCounts() (hosts, links int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.hosts), len(n.hostSubsys)
}

func (n *nas) firstSubsys() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.subsys) == 0 {
		return nil
	}
	return n.subsys[0]
}

// hostGrants counts the host-to-subsystem ACL entries on the appliance.
func (n *nas) hostGrants() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.hostSubsys)
}

func (n *nas) firstPort() map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.ports) == 0 {
		return nil
	}
	return n.ports[0]
}

func (n *nas) seedPort(trtype, addr string, svcid int) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.nextID()
	n.ports = append(n.ports, map[string]any{
		"id": id, "addr_trtype": trtype, "addr_traddr": addr, "addr_trsvcid": svcid,
	})
	return id
}

func (n *nas) enableRDMA() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.rdma = true
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

// --- tests -------------------------------------------------------------------

func volID(name string) volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "nvme", Pool: "Pool0", Parent: "k8s", Name: name}
}

func createReq(name string, size int64, params map[string]string) backend.CreateRequest {
	if params == nil {
		params = map[string]string{}
	}
	return backend.CreateRequest{ID: volID(name), CapacityBytes: size, Params: params}
}

// TestNVMeRegistersItself pins the wiring: the registry finds this backend by
// the StorageClass protocol value, not by an import of this package.
func TestNVMeRegistersItself(t *testing.T) {
	found := false
	for _, p := range backend.Protocols() {
		if p == Protocol {
			found = true
		}
	}
	if !found {
		t.Fatalf("nvme must register itself, registry has %v", backend.Protocols())
	}
}

// TestNVMeCreateStampsOwnership: the appliance holds ~20 TiB of live data, and
// the ONLY thing distinguishing a driver zvol from the operator's own is the
// LOCAL ownership marker. A zvol created without it can never be deleted.
func TestNVMeCreateStampsOwnership(t *testing.T) {
	n := newNAS(t)
	b := n.backend()

	vol, err := b.Create(context.Background(), createReq("pvc-own", 1<<30, nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-own")
	if ds == nil {
		t.Fatal("no zvol was created")
	}
	props, _ := ds["user_properties"].(map[string]any)
	marker, _ := props["io.truenas.csi:managed"].(map[string]any)
	if marker == nil || marker["value"] != volume.OwnerValue || marker["source"] != "LOCAL" {
		t.Fatalf("the zvol must carry the LOCAL ownership marker, got %v", props)
	}
	if ds["type"] != "VOLUME" {
		t.Fatalf("an NVMe namespace needs a zvol, got type %v", ds["type"])
	}
	if sparse, _ := ds["sparse"].(bool); !sparse {
		t.Fatal("the zvol must be sparse by default")
	}
	if bs, _ := ds["volblocksize"].(map[string]any); bs == nil || bs["value"] != "128K" {
		t.Fatalf("volblocksize must come from recommended_zvol_blocksize, got %v", ds["volblocksize"])
	}

	dsN, subs, nss, ports, links := n.counts()
	if dsN != 1 || subs != 1 || nss != 1 || ports != 1 || links != 0 {
		t.Fatalf("want one of each object and NO port binding before publish, got datasets=%d subsys=%d namespaces=%d ports=%d port_subsys=%d",
			dsN, subs, nss, ports, links)
	}

	for _, k := range []string{"protocol", "portal", "nqn", "serial", "transport"} {
		if vol.Context[k] == "" {
			t.Fatalf("publish context is missing %q: %v", k, vol.Context)
		}
	}
	if vol.Context["protocol"] != Protocol {
		t.Fatalf("protocol = %q, want %q", vol.Context["protocol"], Protocol)
	}
	if vol.Context["transport"] != "tcp" {
		t.Fatalf("transport = %q, want tcp", vol.Context["transport"])
	}
	if !strings.HasSuffix(vol.Context["nqn"], ":"+subsystemName(volID("pvc-own"))) {
		t.Fatalf("nqn must be <basenqn>:<subsystem>, got %q", vol.Context["nqn"])
	}
	if s := n.firstSubsys(); s == nil || vol.Context["serial"] != s["serial"] {
		t.Fatalf("serial must be the subsystem's own, got %q", vol.Context["serial"])
	}
}

// TestNVMeCreateIsIdempotent: CSI retries CreateVolume freely. A retry must
// converge on what is already there. A second VOLUME gets its own subsystem —
// one subsystem per volume is this backend's defining decision — but shares the
// one port, which is appliance-wide.
func TestNVMeCreateIsIdempotent(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	first, err := b.Create(ctx, createReq("pvc-1", 1<<30, nil))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := b.Create(ctx, createReq("pvc-1", 1<<30, nil))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}

	ds, subs, nss, ports, links := n.counts()
	if ds != 1 || subs != 1 || nss != 1 || ports != 1 || links != 0 {
		t.Fatalf("a retry must not stack objects, got datasets=%d subsys=%d namespaces=%d ports=%d port_subsys=%d",
			ds, subs, nss, ports, links)
	}
	if first.Context["nqn"] != second.Context["nqn"] || first.Context["serial"] != second.Context["serial"] {
		t.Fatalf("a retry must return the same identity: %v then %v", first.Context, second.Context)
	}

	other, err := b.Create(ctx, createReq("pvc-2", 1<<30, nil))
	if err != nil {
		t.Fatalf("Create pvc-2: %v", err)
	}
	ds, subs, nss, ports, links = n.counts()
	if ds != 2 || subs != 2 || nss != 2 || links != 0 {
		t.Fatalf("a second volume needs its own subsystem and namespace, got datasets=%d subsys=%d namespaces=%d port_subsys=%d",
			ds, subs, nss, links)
	}
	if ports != 1 {
		t.Fatalf("the port is shared across volumes, got %d ports", ports)
	}
	if other.Context["nqn"] == first.Context["nqn"] {
		t.Fatalf("two volumes share the subsystem NQN %q", other.Context["nqn"])
	}
}

// TestNVMeCreateRollsBackOnFailure: a half-built volume leaves a zvol nobody
// will ever reference and nobody will ever delete, because Kubernetes never
// learns its handle. Failure must leave the appliance as it was found — and
// must NOT remove the shared port, which this call did not create.
func TestNVMeCreateRollsBackOnFailure(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	preexisting := n.seedPort("TCP", "192.168.10.253", 4420)

	n.failOn("nvmet.namespace.create", &fake.RPCError{Code: -32001, ErrName: "EFAULT", Reason: "boom"})
	if _, err := b.Create(ctx, createReq("pvc-rb", 1<<30, nil)); err == nil {
		t.Fatal("Create must fail when the namespace cannot be made")
	}
	if n.hasDataset("Pool0/k8s/pvc-rb") {
		t.Fatal("the zvol must be rolled back")
	}
	_, subs, nss, ports, links := n.counts()
	if subs != 0 || nss != 0 || links != 0 {
		t.Fatalf("rollback left objects behind: subsys=%d namespaces=%d port_subsys=%d", subs, nss, links)
	}
	if ports != 1 || n.firstPort()["id"] == nil || norm(n.firstPort()["id"]) != norm(float64(preexisting)) {
		t.Fatalf("rollback destroyed a port it did not create: %v", n.firstPort())
	}

	n.clearFail("nvmet.namespace.create")
	if _, err := b.Create(ctx, createReq("pvc-rb", 1<<30, nil)); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	ds, subs, nss, ports, links := n.counts()
	if ds != 1 || subs != 1 || nss != 1 || ports != 1 || links != 0 {
		t.Fatalf("retry must build exactly one volume, got datasets=%d subsys=%d namespaces=%d ports=%d port_subsys=%d",
			ds, subs, nss, ports, links)
	}
}

// TestNVMeDeleteVerifiesOwnership is the pool's last line of defence. Only a
// marker set LOCALLY on the zvol itself is proof of ownership — a marker
// inherited from the parent dataset is not, because ZFS user properties are
// inherited by every child.
func TestNVMeDeleteVerifiesOwnership(t *testing.T) {
	cases := []struct {
		name    string
		seed    map[string]any
		wantErr bool
	}{
		{"unmarked", zvol("Pool0/k8s/pvc-x", 1<<30, "", ""), true},
		{"inherited marker", zvol("Pool0/k8s/pvc-x", 1<<30, "truenas-csi", "INHERITED"), true},
		{"foreign value", zvol("Pool0/k8s/pvc-x", 1<<30, "someone-else", "LOCAL"), true},
		{"owned", zvol("Pool0/k8s/pvc-x", 1<<30, "truenas-csi", "LOCAL"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNAS(t)
			b := n.backend()
			n.putDataset(tc.seed)

			err := b.Delete(context.Background(), volID("pvc-x"))
			if tc.wantErr {
				if !errors.Is(err, volume.ErrNotManaged) {
					t.Fatalf("want ErrNotManaged, got %v", err)
				}
				if !n.hasDataset("Pool0/k8s/pvc-x") {
					t.Fatal("a dataset this driver does not own was destroyed")
				}
				if got := n.s.CallsTo("pool.dataset.delete"); got != 0 {
					t.Fatalf("no destructive call may be issued at all, got %d", got)
				}
				if got := n.s.CallsTo("nvmet.subsys.delete"); got != 0 {
					t.Fatalf("ownership must be checked before anything is torn down, got %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if n.hasDataset("Pool0/k8s/pvc-x") {
				t.Fatal("an owned dataset must be deleted")
			}
		})
	}
}

// TestNVMeDeleteRemovesEveryObject: the subsystem, its namespace and its port
// binding must go too, or every deleted volume leaves an exported subsystem
// behind. Deleting twice must still succeed.
func TestNVMeDeleteRemovesEveryObject(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-d", 1<<30, map[string]string{
		ParamHostNQNs: "nqn.2014-08.org.nvmexpress:uuid:node-a",
	})); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(ctx, volID("pvc-d")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ds, subs, nss, ports, links := n.counts()
	if ds != 0 || subs != 0 || nss != 0 || links != 0 {
		t.Fatalf("want everything gone, got datasets=%d subsys=%d namespaces=%d port_subsys=%d", ds, subs, nss, links)
	}
	if ports != 1 {
		t.Fatalf("the shared port must survive a volume delete, got %d ports", ports)
	}
	if _, hostLinks := n.aclCounts(); hostLinks != 0 {
		t.Fatalf("the host ACL binding must go with the subsystem, got %d", hostLinks)
	}

	if err := b.Delete(ctx, volID("pvc-d")); err != nil {
		t.Fatalf("second Delete must be a no-op, got %v", err)
	}
}

// TestNVMeExpandRejectsShrink: the middleware refuses a zvol shrink, but the
// driver must refuse first and say so, rather than surfacing a middleware error
// whose text tells the operator nothing.
func TestNVMeExpandRejectsShrink(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-e", 2<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Counted from after Create, which writes the volume's port id as a user
	// property and so issues a dataset update of its own.
	before := n.s.CallsTo("pool.dataset.update")
	if _, err := b.Expand(ctx, volID("pvc-e"), 1<<30); !errors.Is(err, ErrShrinkNotAllowed) {
		t.Fatalf("want ErrShrinkNotAllowed, got %v", err)
	}
	if got := n.s.CallsTo("pool.dataset.update") - before; got != 0 {
		t.Fatalf("a shrink must never reach the middleware, got %d updates", got)
	}

	got, err := b.Expand(ctx, volID("pvc-e"), 4<<30)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != 4<<30 {
		t.Fatalf("want 4 GiB, got %d", got)
	}
	if got, err := b.Expand(ctx, volID("pvc-e"), 4<<30); err != nil || got != 4<<30 {
		t.Fatalf("idempotent expand: got %d, %v", got, err)
	}
}

// TestNVMePortalNeverWildcard: a port bound to 0.0.0.0 describes where the
// APPLIANCE listens, not an address a node can dial. Handing the wildcard to
// `nvme connect` produces a connection refused against 0.0.0.0, so the publish
// context must fall back to the appliance's own host.
func TestNVMePortalNeverWildcard(t *testing.T) {
	n := newNAS(t)
	c := n.client()
	b := New(c, backend.Options{Pool: "Pool0", Parent: "k8s"}).(*nvmeBackend)
	ctx := context.Background()

	n.seedPort("TCP", "0.0.0.0", 4420)

	vol, err := b.Create(ctx, createReq("pvc-w", 1<<30, nil))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, portal := range []string{vol.Context["portal"], ""} {
		if portal == "" {
			continue
		}
		if strings.HasPrefix(portal, "0.0.0.0") || strings.HasPrefix(portal, "::") || strings.HasPrefix(portal, "[::]") {
			t.Fatalf("portal must never be the wildcard, got %q", portal)
		}
	}
	if want := c.Host() + ":4420"; vol.Context["portal"] != want {
		t.Fatalf("portal = %q, want the appliance host %q", vol.Context["portal"], want)
	}

	// Publish reads live state and must apply the same rule; the node may
	// attach long after the controller that provisioned the volume died.
	pc, err := b.Publish(ctx, volID("pvc-w"),
		backend.NodeRef{ID: "worker-1", Addrs: []string{"10.0.0.1"}})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pc["portal"] != vol.Context["portal"] {
		t.Fatalf("PublishContext portal = %q, want %q", pc["portal"], vol.Context["portal"])
	}
}

// TestNVMeEmptyHostNQNsAllowsAnyHost: on TrueNAS an EMPTY host ACL denies every
// initiator. Creating a subsystem with no hosts and allow_any_host=false would
// silently take the whole backend offline, so "no hostNQNs configured" must
// mean allow_any_host=true — exactly the reasoning the iSCSI backend applies to
// an empty initiator group.
func TestNVMeEmptyHostNQNsAllowsAnyHost(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-open", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s := n.firstSubsys()
	if s == nil {
		t.Fatal("no subsystem was created")
	}
	if allow, _ := s["allow_any_host"].(bool); !allow {
		t.Fatalf("an empty hostNQNs list must set allow_any_host, got %v", s)
	}
	if hosts, links := n.aclCounts(); hosts != 0 || links != 0 {
		t.Fatalf("no host ACL may be written when none was configured, got hosts=%d links=%d", hosts, links)
	}

	// With host NQNs configured the subsystem is closed and the ACL written.
	n2 := newNAS(t)
	b2 := n2.backend()
	if _, err := b2.Create(ctx, createReq("pvc-acl", 1<<30, map[string]string{
		ParamHostNQNs: "nqn.2014-08.org.nvmexpress:uuid:node-a, nqn.2014-08.org.nvmexpress:uuid:node-b",
	})); err != nil {
		t.Fatalf("Create with host NQNs: %v", err)
	}
	s2 := n2.firstSubsys()
	if allow, _ := s2["allow_any_host"].(bool); allow {
		t.Fatalf("a configured host ACL must close the subsystem, got %v", s2)
	}
	if hosts, links := n2.aclCounts(); hosts != 2 || links != 2 {
		t.Fatalf("want two hosts bound to the subsystem, got hosts=%d links=%d", hosts, links)
	}
}

// TestNVMeRDMARefusedWhenUnsupported: the appliance reports nvmet.global.rdma =
// false and the nodes have no RDMA NICs. Creating an RDMA port there yields a
// port nothing can connect to, and the volume would fail at attach time on the
// node with no explanation. Refuse at provisioning instead.
func TestNVMeRDMARefusedWhenUnsupported(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	_, err := b.Create(ctx, createReq("pvc-rdma", 1<<30, map[string]string{ParamTransport: "rdma"}))
	if !errors.Is(err, ErrRDMAUnavailable) {
		t.Fatalf("want ErrRDMAUnavailable, got %v", err)
	}
	if got := n.s.CallsTo("nvmet.subsys.create"); got != 0 {
		t.Fatalf("nothing may be created for an impossible transport, got %d subsystem creates", got)
	}
	if got := n.s.CallsTo("nvmet.port.create"); got != 0 {
		t.Fatalf("an unconnectable port must never be created, got %d", got)
	}
	if n.hasDataset("Pool0/k8s/pvc-rdma") {
		t.Fatal("no zvol may be left behind by a refused transport")
	}

	// An unknown transport is rejected outright rather than passed through.
	if _, err := b.Create(ctx, createReq("pvc-bogus", 1<<30, map[string]string{ParamTransport: "carrier-pigeon"})); err == nil {
		t.Fatal("an unknown transport must be rejected")
	}

	// On an appliance that does report RDMA, the port is created for it. This
	// path is IMPLEMENTED BUT UNVALIDATED against real hardware.
	n.enableRDMA()
	vol, err := b.Create(ctx, createReq("pvc-rdma", 1<<30, map[string]string{ParamTransport: "rdma"}))
	if err != nil {
		t.Fatalf("Create over rdma: %v", err)
	}
	if vol.Context["transport"] != "rdma" {
		t.Fatalf("transport = %q, want rdma", vol.Context["transport"])
	}
	if p := n.firstPort(); p == nil || p["addr_trtype"] != "RDMA" {
		t.Fatalf("want an RDMA port, got %v", n.firstPort())
	}
}

// TestNVMeThickRestoreKeepsItsReservation pins thick provisioning across a
// clone.
//
// refreservation is what makes sparse: "false" mean anything, and a ZFS clone
// inherits it no more than it inherits the ownership marker: verified on a real
// appliance, a clone of a 1 GiB thick zvol came back with refreservation=none
// and source=DEFAULT. The volume an operator asked to be guaranteed space was
// silently thin, and would meet ENOSPC on write -- the exact failure thick
// provisioning is bought to prevent.
func TestNVMeThickRestoreKeepsItsReservation(t *testing.T) {
	ctx := context.Background()
	n := newNAS(t)
	b := n.backend()

	thick := map[string]string{ParamSparse: "false"}
	if _, err := b.Create(ctx, createReq("pvc-src", 1<<30, thick)); err != nil {
		t.Fatalf("Create source: %v", err)
	}
	req := createReq("pvc-restored", 1<<30, thick)
	req.SourceSnapshot = "Pool0/k8s/pvc-src@snap1"
	if _, err := b.Create(ctx, req); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("the clone was not created")
	}
	res, _ := ds["refreservation"].(map[string]any)
	if res == nil || res["value"] == "" || res["value"] == nil {
		t.Fatalf("clone carries no refreservation — sparse:\"false\" was silently lost")
	}
	// "auto" is the only value that is right: ZFS computes volsize plus the
	// metadata overhead for this pool's geometry, which no caller can derive.
	if got := res["value"]; got != "auto" {
		t.Fatalf("clone refreservation = %v, want \"auto\"", got)
	}
}

// TestNVMeCrashMidCloneDoesNotLeak: a controller that dies between the clone and
// the stamping leaves a zvol carrying no ownership marker. The retry used to
// see a zvol at the right path, treat the volume as already finished, and go on
// to publish it -- producing a WORKING volume that the delete guard would then
// refuse to remove for the rest of its life, because VerifyOwned finds no
// marker. Silent success and a permanent leak is the worst of the three
// possible outcomes here.
func TestNVMeCrashMidCloneDoesNotLeak(t *testing.T) {
	ctx := context.Background()
	n := newNAS(t)
	b := n.backend()

	if _, err := b.Create(ctx, createReq("pvc-src", 1<<30, nil)); err != nil {
		t.Fatalf("Create source: %v", err)
	}
	// The abandoned clone: cloned from this request's snapshot, never stamped.
	n.putDataset(map[string]any{
		"id": "Pool0/k8s/pvc-restored", "type": "VOLUME",
		"volsize": map[string]any{"parsed": int64(1 << 30)},
		"origin": map[string]any{"value": "POOL0/K8S/PVC-SRC@SNAP1",
			"rawvalue": "Pool0/k8s/pvc-src@snap1", "source": "LOCAL"},
		"user_properties": map[string]any{},
	})

	req := createReq("pvc-restored", 1<<30, nil)
	req.SourceSnapshot = "Pool0/k8s/pvc-src@snap1"
	if _, err := b.Create(ctx, req); err != nil {
		t.Fatalf("retry after a crash mid-clone: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("the volume disappeared")
	}
	if got, _ := userProp(ds, volume.OwnerProperty); got != volume.OwnerValue {
		t.Fatalf("resumed clone marker = %q — the delete guard would refuse to "+
			"remove this volume forever", got)
	}
	if got, _ := userProp(ds, volume.ProtocolProperty); got != Protocol {
		t.Errorf("resumed clone protocol = %q, want %q", got, Protocol)
	}
}
