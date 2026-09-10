package nfs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// nas is a minimal stateful stand-in for the appliance: enough dataset and
// share bookkeeping that idempotency, rollback and clone behaviour can be
// exercised the way the live box behaves.
type nas struct {
	*fake.Server

	mu sync.Mutex
	// datasets is id -> the dataset's observable state.
	datasets map[string]*fakeDataset
	shares   map[string]*fakeExport
	nextID   int

	createPayloads []map[string]any
	setperms       []map[string]any
	sharePayloads  []map[string]any

	shareCreateErr error
}

type fakeDataset struct {
	origin   string
	refquota int64
	marker   string // "" when unmarked
	source   string
	props    map[string]string
	comments string // the ZFS comments field the TrueNAS UI shows
}

// fakeExport mirrors the two fields that decide who may mount an NFS share.
//
// Both are modelled because the appliance ORs them: an export is reachable by
// everyone only when BOTH are empty, and a fake that tracked hosts alone could
// not tell a fenced export from an open one.
type fakeExport struct {
	id       int
	hosts    []string
	networks []string
	// disabled mirrors the appliance's own enabled switch, which a query
	// reports for every share. A fake that never returned it described a share
	// that cannot be turned off, which is not the appliance's behaviour.
	disabled bool
}

func (d *fakeDataset) json(id string) map[string]any {
	props := map[string]any{}
	if d.marker != "" {
		props[volume.OwnerProperty] = map[string]any{"value": d.marker, "source": d.source}
	}
	for k, v := range d.props {
		props[k] = map[string]any{"value": v, "source": "LOCAL"}
	}
	return map[string]any{
		"id":         id,
		"type":       "FILESYSTEM",
		"mountpoint": "/mnt/" + id,
		"refquota":   map[string]any{"parsed": d.refquota},
		// The middleware UPPERCASES origin's display form and keeps the true
		// name only in rawvalue — verified on a real appliance. Reproducing it
		// is the point: a fake that echoed the name verbatim let a comparison
		// against Value pass here and fail on hardware.
		"origin": map[string]any{
			"value": strings.ToUpper(d.origin), "rawvalue": d.origin, "source": "LOCAL"},
		"user_properties": props,
		"comments":        map[string]any{"value": d.comments, "source": "LOCAL"},
	}
}

func newNAS(t *testing.T) *nas {
	t.Helper()
	n := &nas{
		Server:   fake.Start(t, fake.Options{}),
		datasets: map[string]*fakeDataset{},
		shares:   map[string]*fakeExport{},
	}

	n.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		id := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return []any{}, nil
		}
		return []any{ds.json(id)}, nil
	})

	n.Handle("pool.dataset.create", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		id, _ := payload["name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		n.createPayloads = append(n.createPayloads, payload)
		ds := &fakeDataset{}
		if q, ok := payload["refquota"].(float64); ok {
			ds.refquota = int64(q)
		}
		if c, ok := payload["comments"].(string); ok {
			ds.comments = c
		}
		if props, ok := payload["user_properties"].([]any); ok {
			for _, raw := range props {
				m, _ := raw.(map[string]any)
				key, _ := m["key"].(string)
				val, _ := m["value"].(string)
				if key == volume.OwnerProperty {
					ds.marker, ds.source = val, "LOCAL"
					continue
				}
				if ds.props == nil {
					ds.props = map[string]string{}
				}
				ds.props[key] = val
			}
		}
		n.datasets[id] = ds
		return ds.json(id), nil
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
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] " + id}
		}
		if q, ok := patch["refquota"].(float64); ok {
			ds.refquota = int64(q)
		}
		if c, ok := patch["comments"].(string); ok {
			ds.comments = c
		}
		if props, ok := patch["user_properties_update"].([]any); ok {
			for _, raw := range props {
				m, _ := raw.(map[string]any)
				key, _ := m["key"].(string)
				val, _ := m["value"].(string)
				if key == volume.OwnerProperty {
					ds.marker, ds.source = val, "LOCAL"
					continue
				}
				if ds.props == nil {
					ds.props = map[string]string{}
				}
				ds.props[key] = val
			}
		}
		return ds.json(id), nil
	})

	// Renaming moves the dataset and everything on it, including its user
	// properties. Modelling that is what makes the ownership marker's survival
	// across a retire a fact rather than an assumption.
	n.Handle("pool.dataset.rename", func(p []json.RawMessage) (any, error) {
		var id string
		var opts map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &opts)
		dst, _ := opts["new_name"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		ds, ok := n.datasets[id]
		if !ok {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] " + id}
		}
		if _, taken := n.datasets[dst]; taken {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[EEXIST] " + dst}
		}
		delete(n.datasets, id)
		n.datasets[dst] = ds
		return nil, nil
	})

	n.Handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		var id string
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.datasets, id)
		return true, nil
	})

	n.Handle("pool.snapshot.clone", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		dst, _ := payload["dataset_dst"].(string)
		snap, _ := payload["snapshot"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		// A real ZFS clone inherits NEITHER the marker NOR refquota.
		n.datasets[dst] = &fakeDataset{origin: snap}
		return true, nil
	})

	n.Handle("filesystem.setperm", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		n.mu.Lock()
		n.setperms = append(n.setperms, payload)
		n.mu.Unlock()
		return 1, nil
	})
	n.HandleValue("core.get_jobs", []any{map[string]any{"id": 1, "state": "SUCCESS"}})

	n.Handle("sharing.nfs.create", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.shareCreateErr != nil {
			return nil, n.shareCreateErr
		}
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		path, _ := payload["path"].(string)
		n.nextID++
		sh := &fakeExport{
			id:       n.nextID,
			hosts:    strList(payload["hosts"]),
			networks: strList(payload["networks"]),
		}
		n.shares[path] = sh
		n.sharePayloads = append(n.sharePayloads, payload)
		return map[string]any{"id": sh.id, "path": path,
			"hosts": sh.hosts, "networks": sh.networks}, nil
	})

	n.Handle("sharing.nfs.query", func(p []json.RawMessage) (any, error) {
		path := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		sh, ok := n.shares[path]
		if !ok {
			return []any{}, nil
		}
		return []any{map[string]any{"id": sh.id, "path": path,
			"hosts": sh.hosts, "networks": sh.networks, "enabled": !sh.disabled}}, nil
	})

	n.Handle("sharing.nfs.update", func(p []json.RawMessage) (any, error) {
		var id int
		var patch map[string]any
		mustJSON(t, p[0], &id)
		mustJSON(t, p[1], &patch)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, sh := range n.shares {
			if sh.id != id {
				continue
			}
			if v, ok := patch["hosts"]; ok {
				sh.hosts = strList(v)
			}
			if v, ok := patch["networks"]; ok {
				sh.networks = strList(v)
			}
			return map[string]any{"id": sh.id, "hosts": sh.hosts, "networks": sh.networks}, nil
		}
		return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] share"}
	})

	n.Handle("sharing.nfs.delete", func(p []json.RawMessage) (any, error) {
		var id int
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		for path, sh := range n.shares {
			if sh.id == id {
				delete(n.shares, path)
			}
		}
		return true, nil
	})

	return n
}

// export returns the appliance's view of the share for a path.
func (n *nas) export(path string) *fakeExport {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shares[path]
}

// strList decodes a JSON string array the middleware would have received.
func strList(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func (n *nas) dataset(id string) *fakeDataset {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.datasets[id]
}

func (n *nas) put(id string, ds *fakeDataset) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.datasets[id] = ds
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

func newBackend(t *testing.T, n *nas) backend.Backend {
	t.Helper()
	c, err := truenas.Dial(context.Background(), config.Backend{
		Name: "nas1", Endpoint: n.URL(), Username: "truenas_admin", APIKey: "1-secret",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(c, backend.Options{Pool: "Pool0", Parent: "k8s"})
}

const gib = int64(1) << 30

func testID(name string) volume.ID {
	return volume.ID{Backend: "nas1", Protocol: "nfs", Pool: "Pool0", Parent: "k8s", Name: name}
}

func testRequest(name string, bytes int64) backend.CreateRequest {
	return backend.CreateRequest{
		ID:            testID(name),
		CapacityBytes: bytes,
		Params:        map[string]string{"server": "192.168.10.253"},
	}
}

// TestNFSCreateSetsRefquota catches a create that omits refquota: without it a
// pod's df reports the whole pool (31T observed for a 10Gi PVC), volume stats
// are meaningless and one PVC can eat the pool.
func TestNFSCreateSetsRefquota(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", 10*gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.createPayloads) != 1 {
		t.Fatalf("want exactly one pool.dataset.create, got %d", len(n.createPayloads))
	}
	q, ok := n.createPayloads[0]["refquota"]
	if !ok {
		t.Fatal("pool.dataset.create carried no refquota — the pod would see the whole pool")
	}
	if got := int64(q.(float64)); got != 10*gib {
		t.Fatalf("refquota = %d, want %d", got, 10*gib)
	}
}

// TestNFSCreateStampsOwnership catches an unmarked dataset, which the delete
// guard would later refuse to remove — leaking the volume forever.
func TestNFSCreateStampsOwnership(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("dataset was not created")
	}
	if ds.marker != volume.OwnerValue || ds.source != "LOCAL" {
		t.Fatalf("marker = %q source %q, want %q LOCAL", ds.marker, ds.source, volume.OwnerValue)
	}
}

// TestNFSCreateSetsPermissions catches a share exported before setperm ran: a
// fresh dataset is root:root 0755 and a non-root pod cannot write to it, and
// fsGroup does not fix it because kubelet skips fsGroup for NFS.
func TestNFSCreateSetsPermissions(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	r := testRequest("pvc-1", gib)
	r.Params["mode"] = "0770"
	r.Params["uid"] = "1000"
	r.Params["gid"] = "1001"
	if _, err := b.Create(context.Background(), r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n.mu.Lock()
	perms := append([]map[string]any(nil), n.setperms...)
	n.mu.Unlock()
	if len(perms) != 1 {
		t.Fatalf("want one filesystem.setperm, got %d", len(perms))
	}
	if perms[0]["mode"] != "0770" {
		t.Fatalf("mode = %v, want 0770", perms[0]["mode"])
	}
	if perms[0]["uid"].(float64) != 1000 || perms[0]["gid"].(float64) != 1001 {
		t.Fatalf("uid/gid = %v/%v, want 1000/1001", perms[0]["uid"], perms[0]["gid"])
	}
	if perms[0]["path"] != "/mnt/Pool0/k8s/pvc-1" {
		t.Fatalf("path = %v", perms[0]["path"])
	}

	setperm, share := -1, -1
	for i, call := range n.Calls() {
		switch call {
		case "filesystem.setperm":
			if setperm < 0 {
				setperm = i
			}
		case "sharing.nfs.create":
			if share < 0 {
				share = i
			}
		}
	}
	if setperm < 0 || share < 0 {
		t.Fatalf("missing calls: setperm=%d share=%d in %v", setperm, share, n.Calls())
	}
	if setperm > share {
		t.Fatalf("filesystem.setperm ran after sharing.nfs.create (%d > %d): the export would be "+
			"visible while still root:root 0755", setperm, share)
	}
}

// TestNFSCreateIsIdempotent catches a create that does not look before it leaps:
// CSI retries CreateVolume, and a second pool.dataset.create would fail the retry.
func TestNFSCreateIsIdempotent(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	first, err := b.Create(context.Background(), testRequest("pvc-1", 5*gib))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := b.Create(context.Background(), testRequest("pvc-1", 5*gib))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}

	if got := n.CallsTo("pool.dataset.create"); got != 1 {
		t.Fatalf("pool.dataset.create called %d times, want 1", got)
	}
	if first.ID != second.ID || first.CapacityBytes != second.CapacityBytes {
		t.Fatalf("second Create returned a different volume: %+v vs %+v", first, second)
	}
}

// TestNFSCreateConflictingSize catches a create that silently accepts a
// same-named volume of a different size, which CSI requires be reported as a
// conflict rather than returning the wrong capacity.
func TestNFSCreateConflictingSize(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", 5*gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := b.Create(context.Background(), testRequest("pvc-1", 10*gib))
	if err == nil {
		t.Fatal("want an error for an existing dataset of a different size")
	}
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err %v)", got, err)
	}
}

// TestNFSCreateRollsBackOnShareFailure catches a partial provision: a dataset
// left behind after a failed share is an orphan the retry cannot reconcile.
func TestNFSCreateRollsBackOnShareFailure(t *testing.T) {
	n := newNAS(t)
	n.mu.Lock()
	n.shareCreateErr = &fake.RPCError{Code: -32001, ErrName: "EFAULT", Reason: "share creation failed"}
	n.mu.Unlock()
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err == nil {
		t.Fatal("want an error when the share cannot be created")
	}
	if ds := n.dataset("Pool0/k8s/pvc-1"); ds != nil {
		t.Fatal("dataset survived a failed share creation — a partial volume was left behind")
	}
	if got := n.CallsTo("pool.dataset.delete"); got != 1 {
		t.Fatalf("pool.dataset.delete called %d times, want 1 (rollback)", got)
	}
}

// TestNFSDeleteVerifiesOwnership catches a delete guard that trusts presence
// alone: an unmarked or inherited-marker dataset is operator data.
func TestNFSDeleteVerifiesOwnership(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-1", &fakeDataset{refquota: gib}) // no marker at all
	n.put("Pool0/k8s/pvc-2", &fakeDataset{refquota: gib, marker: volume.OwnerValue, source: "INHERITED"})
	b := newBackend(t, n)

	for _, name := range []string{"pvc-1", "pvc-2"} {
		err := b.Delete(context.Background(), testID(name))
		if !errors.Is(err, volume.ErrNotManaged) {
			t.Fatalf("%s: want ErrNotManaged, got %v", name, err)
		}
	}
	if got := n.CallsTo("pool.dataset.delete"); got != 0 {
		t.Fatalf("pool.dataset.delete called %d times on unowned datasets — data loss", got)
	}
}

func TestNFSDeleteRemovesShareAndDataset(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(context.Background(), testID("pvc-1")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n.dataset("Pool0/k8s/pvc-1") != nil {
		t.Fatal("dataset survived Delete")
	}
	if got := n.CallsTo("sharing.nfs.delete"); got != 1 {
		t.Fatalf("sharing.nfs.delete called %d times, want 1", got)
	}
	// Deleting an absent volume is success.
	if err := b.Delete(context.Background(), testID("pvc-1")); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// TestNFSExpandRejectsShrink catches delegation of the shrink guard to the
// middleware, which SILENTLY PERMITS a refquota shrink below current usage.
func TestNFSExpandRejectsShrink(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-1", &fakeDataset{refquota: 10 * gib, marker: volume.OwnerValue, source: "LOCAL"})
	b := newBackend(t, n)

	_, err := b.Expand(context.Background(), testID("pvc-1"), 5*gib)
	if err == nil {
		t.Fatal("want an error shrinking 10GiB to 5GiB")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", got, err)
	}
	if got := n.CallsTo("pool.dataset.update"); got != 0 {
		t.Fatalf("pool.dataset.update issued %d times for a shrink — middleware would allow it", got)
	}
	if ds := n.dataset("Pool0/k8s/pvc-1"); ds.refquota != 10*gib {
		t.Fatalf("refquota = %d, want unchanged %d", ds.refquota, 10*gib)
	}

	size, err := b.Expand(context.Background(), testID("pvc-1"), 20*gib)
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if size != 20*gib {
		t.Fatalf("Expand returned %d, want %d", size, 20*gib)
	}
	if ds := n.dataset("Pool0/k8s/pvc-1"); ds.refquota != 20*gib {
		t.Fatalf("refquota = %d after grow, want %d", ds.refquota, 20*gib)
	}
}

// TestNFSRestoreStampsCloneAndSetsQuota catches the clone trap: a ZFS clone
// inherits neither the ownership marker nor refquota, so an unstamped restored
// volume leaks forever and reports the whole pool.
func TestNFSRestoreStampsCloneAndSetsQuota(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{refquota: gib, marker: volume.OwnerValue, source: "LOCAL"})
	b := newBackend(t, n)

	r := testRequest("pvc-restored", 4*gib)
	r.SourceSnapshot = "Pool0/k8s/pvc-src@snap-1"
	vol, err := b.Create(context.Background(), r)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if vol.CapacityBytes != 4*gib {
		t.Fatalf("CapacityBytes = %d, want %d", vol.CapacityBytes, 4*gib)
	}
	if got := n.CallsTo("pool.snapshot.clone"); got != 1 {
		t.Fatalf("pool.snapshot.clone called %d times, want 1", got)
	}
	if got := n.CallsTo("pool.dataset.create"); got != 0 {
		t.Fatalf("a restore must clone, not create: %d creates", got)
	}

	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds == nil {
		t.Fatal("clone was not created")
	}
	if ds.marker != volume.OwnerValue || ds.source != "LOCAL" {
		t.Fatalf("clone marker = %q/%q — a restored volume would leak forever", ds.marker, ds.source)
	}
	if ds.refquota != 4*gib {
		t.Fatalf("clone refquota = %d, want %d — the pod would see the whole pool", ds.refquota, 4*gib)
	}
	n.mu.Lock()
	perms := len(n.setperms)
	n.mu.Unlock()
	if perms != 1 {
		t.Fatalf("filesystem.setperm ran %d times on the clone, want 1", perms)
	}

	// The restored volume must be deletable by the ownership guard.
	if err := b.Delete(context.Background(), testID("pvc-restored")); err != nil {
		t.Fatalf("Delete restored volume: %v", err)
	}
}

func TestNFSPublishContext(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pc, err := b.PublishContext(context.Background(), testID("pvc-1"))
	if err != nil {
		t.Fatalf("PublishContext: %v", err)
	}
	if pc["server"] != "192.168.10.253" {
		t.Fatalf("server = %q", pc["server"])
	}
	if pc["share"] != "/mnt/Pool0/k8s/pvc-1" {
		t.Fatalf("share = %q", pc["share"])
	}
	// v3 pulls rpc-statd onto the node; v4 does not.
	if pc["nfsVersion"] != "4" {
		t.Fatalf("nfsVersion = %q, want 4", pc["nfsVersion"])
	}
}

func TestNFSConfinesVolumesToParent(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	r := testRequest("pvc-1", gib)
	r.ID.Parent = "elsewhere"
	_, err := b.Create(context.Background(), r)
	if err == nil {
		t.Fatal("want the volume refused for leaving the parent dataset")
	}
	if !errors.Is(err, volume.ErrOutsideParent) && !strings.Contains(err.Error(), "outside") {
		t.Fatalf("error should name the confinement failure, got %v", err)
	}
	if got := n.CallsTo("pool.dataset.create"); got != 0 {
		t.Fatalf("pool.dataset.create issued %d times outside the parent dataset", got)
	}
}

func TestNFSProtocolIsRegistered(t *testing.T) {
	found := false
	for _, p := range backend.Protocols() {
		if p == "nfs" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nfs is not registered: %v", backend.Protocols())
	}
}

// TestNFSPublishContextAfterRestart proves a restarted controller can still
// publish a volume it did not create in this process: the server address falls
// back to the appliance endpoint rather than a cache a restart would empty.
func TestNFSPublishContextAfterRestart(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	id := testID("pvc-restart")
	if _, err := b.Create(context.Background(), testRequest("pvc-restart", gib)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A second backend over the same appliance, standing in for a controller
	// that restarted and never saw the StorageClass.
	fresh := newBackend(t, n)
	pc, err := fresh.PublishContext(context.Background(), id)
	if err != nil {
		t.Fatalf("PublishContext after restart must not fail: %v", err)
	}
	if pc["server"] == "" {
		t.Fatal("server must fall back to the appliance host, not stay empty")
	}
}

// TestNFSRestoreResumesAfterACrashMidClone: a controller that dies between the
// clone and the stamping leaves a dataset with no marker and no refquota, and
// the retry used to compare that refquota (0 — a clone inherits none, verified
// on a real appliance) against the requested size and answer AlreadyExists.
// That is permanent, because the provisioner retries a deterministic failure,
// so the claim never bound and an operator had to destroy the dataset by hand.
func TestNFSRestoreResumesAfterACrashMidClone(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{refquota: gib, marker: volume.OwnerValue, source: "LOCAL"})
	n.put("Pool0/k8s/pvc-restored", &fakeDataset{origin: "Pool0/k8s/pvc-src@snap-1"})

	b := newBackend(t, n)
	r := testRequest("pvc-restored", gib)
	r.SourceSnapshot = "Pool0/k8s/pvc-src@snap-1"
	if _, err := b.Create(context.Background(), r); err != nil {
		t.Fatalf("retry after a crash mid-clone: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-restored")
	if ds.marker != volume.OwnerValue {
		t.Errorf("resumed clone still unmarked — it would leak forever")
	}
	if ds.refquota != gib {
		t.Errorf("resumed clone refquota = %d, want %d", ds.refquota, gib)
	}
	if ds.props[volume.ProtocolProperty] != "nfs" {
		t.Errorf("resumed clone protocol = %q, want nfs", ds.props[volume.ProtocolProperty])
	}
}

// TestDisabledExportIsRefusedRatherThanAdopted.
//
// A share carries its own enabled switch and the driver matched on PATH alone,
// so a disabled export was adopted silently: the volume reported provisioned
// and published while the appliance exported nothing, and the pod failed to
// mount with nothing pointing at the share. Verified on a real appliance, where
// sharing.nfs.update accepts {"enabled": false} and the share still answers a
// query at the same path.
func TestDisabledExportIsRefusedRatherThanAdopted(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-1", &fakeDataset{refquota: gib, marker: volume.OwnerValue, source: "LOCAL"})
	n.mu.Lock()
	n.shares["/mnt/Pool0/k8s/pvc-1"] = &fakeExport{id: 7, disabled: true}
	n.mu.Unlock()
	b := newBackend(t, n)

	_, err := b.Create(context.Background(), testRequest("pvc-1", gib))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition: a disabled export exports "+
			"nothing, so a volume using it can never be mounted (err %v)",
			status.Code(err), err)
	}
}
