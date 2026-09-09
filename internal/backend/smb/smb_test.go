package smb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// nas is a stateful stand-in for the appliance, mirroring the behaviours proven
// against the live box: a clone inherits neither the ownership marker nor the
// refquota, and an SMB dataset carries an ACL rather than a plain POSIX mode.
type nas struct {
	*fake.Server

	mu       sync.Mutex
	datasets map[string]*fakeDataset
	shares   map[string]*fakeShare // keyed by path
	nextID   int

	createPayloads []map[string]any
	setacls        []map[string]any
	setperms       []map[string]any
	sharePayloads  []map[string]any

	shareCreateErr error
}

type fakeShare struct {
	id      int
	name    string
	path    string
	purpose string
	// options is the nested object the appliance really returns. It is kept
	// whole, and not reduced to the two host lists, because a share's other
	// options must survive an access-list update — a LEGACY_SHARE carries a
	// dozen of them and an update replaces the object outright.
	options map[string]any
}

func (s *fakeShare) json() map[string]any {
	return map[string]any{"id": s.id, "path": s.path, "name": s.name,
		"purpose": s.purpose, "options": s.options}
}

// hostList reads one of the share's access lists back out.
func (s *fakeShare) hostList(key string) []string {
	raw, _ := s.options[key].([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if v, ok := e.(string); ok {
			out = append(out, v)
		}
	}
	return out
}

type fakeDataset struct {
	acltype   string
	refquota  int64
	marker    string
	source    string
	shareType string
	props     map[string]string
	comments  string // the ZFS comments field the TrueNAS UI shows
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
		"id":              id,
		"type":            "FILESYSTEM",
		"mountpoint":      "/mnt/" + id,
		"refquota":        map[string]any{"parsed": d.refquota},
		"user_properties": props,
		"comments":        map[string]any{"value": d.comments, "source": "LOCAL"},
	}
}

// setProp applies one {key, value} property entry, keeping the ownership
// marker in its own field so the existing guard tests keep reading it there.
func (d *fakeDataset) setProp(m map[string]any) {
	key, _ := m["key"].(string)
	val, _ := m["value"].(string)
	if key == volume.OwnerProperty {
		d.marker, d.source = val, "LOCAL"
		return
	}
	if d.props == nil {
		d.props = map[string]string{}
	}
	d.props[key] = val
}

func newNAS(t *testing.T) *nas {
	t.Helper()
	n := &nas{
		Server:   fake.Start(t, fake.Options{}),
		datasets: map[string]*fakeDataset{},
		shares:   map[string]*fakeShare{},
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
		ds.shareType, _ = payload["share_type"].(string)
		ds.comments, _ = payload["comments"].(string)
		if props, ok := payload["user_properties"].([]any); ok {
			for _, raw := range props {
				m, _ := raw.(map[string]any)
				ds.setProp(m)
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
		if a, ok := patch["acltype"].(string); ok {
			ds.acltype = a
		}
		if props, ok := patch["user_properties_update"].([]any); ok {
			for _, raw := range props {
				m, _ := raw.(map[string]any)
				ds.setProp(m)
			}
		}
		return ds.json(id), nil
	})

	n.Handle("pool.dataset.delete", func(p []json.RawMessage) (any, error) {
		var id string
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		delete(n.datasets, id)
		return true, nil
	})

	// A real ZFS clone inherits NEITHER the marker NOR refquota.
	n.Handle("pool.snapshot.clone", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		dst, _ := payload["dataset_dst"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		// A ZFS clone takes acltype from its POSITION in the hierarchy, never
		// from its origin: share_type SMB sets acltype LOCAL on the source, and
		// the clone lands under the parent dataset and inherits POSIX from it.
		// Verified on a real appliance.
		n.datasets[dst] = &fakeDataset{acltype: "POSIX"}
		return true, nil
	})

	n.Handle("filesystem.setacl", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		path, _ := payload["path"].(string)
		n.mu.Lock()
		defer n.mu.Unlock()
		// An NFSv4 dacl on a dataset whose acltype is POSIX fails the job with
		// a bare KeyError, which the middleware reports as "job N FAILED:
		// 'default'". Reproduced against a real appliance (job 31203).
		if ds := n.datasets[strings.TrimPrefix(path, "/mnt/")]; ds != nil && ds.acltype == "POSIX" {
			return nil, &fake.RPCError{Code: -32602, ErrName: "EFAULT", Reason: "job 31203 FAILED: 'default'"}
		}
		n.setacls = append(n.setacls, payload)
		return 1, nil
	})
	// Registered deliberately: a backend that wrongly used setperm must fail the
	// assertion, not blow up on an unknown method.
	n.Handle("filesystem.setperm", func(p []json.RawMessage) (any, error) {
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		n.mu.Lock()
		n.setperms = append(n.setperms, payload)
		n.mu.Unlock()
		return 1, nil
	})
	n.HandleValue("core.get_jobs", []any{map[string]any{"id": 1, "state": "SUCCESS"}})

	n.Handle("sharing.smb.create", func(p []json.RawMessage) (any, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		if n.shareCreateErr != nil {
			return nil, n.shareCreateErr
		}
		var payload map[string]any
		mustJSON(t, p[0], &payload)
		// The appliance rejects options without purpose:
		//   [EINVAL] data: Value error, You must set `purpose` if you set `options`.
		// A hardware run caught this after the fake had happily accepted it, so
		// the fake now enforces it -- a mock that is more permissive than the
		// real thing turns an integration failure into a release failure.
		if _, hasOpts := payload["options"]; hasOpts {
			if purpose, _ := payload["purpose"].(string); purpose == "" {
				return nil, fmt.Errorf(
					"[EINVAL] data: Value error, You must set `purpose` if you set `options`")
			}
		}
		n.sharePayloads = append(n.sharePayloads, payload)
		path, _ := payload["path"].(string)
		name, _ := payload["name"].(string)
		n.nextID++
		opts, _ := payload["options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		purpose, _ := payload["purpose"].(string)
		sh := &fakeShare{id: n.nextID, name: name, path: path, purpose: purpose, options: opts}
		n.shares[path] = sh
		return sh.json(), nil
	})

	n.Handle("sharing.smb.query", func(p []json.RawMessage) (any, error) {
		path := filterValue(t, p)
		n.mu.Lock()
		defer n.mu.Unlock()
		s, ok := n.shares[path]
		if !ok {
			return []any{}, nil
		}
		return []any{s.json()}, nil
	})

	n.Handle("sharing.smb.update", func(p []json.RawMessage) (any, error) {
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
			// The appliance replaces `options` wholesale, which is exactly why
			// the driver has to read-modify-write it -- and it refuses options
			// that arrive without a purpose, on update as well as on create.
			if opts, ok := patch["options"].(map[string]any); ok {
				purpose, _ := patch["purpose"].(string)
				if purpose == "" {
					return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL",
						Reason: "[EINVAL] data: Value error, You must set `purpose` if you set `options`"}
				}
				sh.purpose = purpose
				sh.options = opts
			}
			return sh.json(), nil
		}
		return nil, &fake.RPCError{Code: -32602, ErrName: "EINVAL", Reason: "[ENOENT] share"}
	})

	n.Handle("sharing.smb.delete", func(p []json.RawMessage) (any, error) {
		var id int
		mustJSON(t, p[0], &id)
		n.mu.Lock()
		defer n.mu.Unlock()
		for path, s := range n.shares {
			if s.id == id {
				delete(n.shares, path)
			}
		}
		return true, nil
	})

	return n
}

// share returns the appliance's view of the share for a path.
func (n *nas) share(path string) *fakeShare {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.shares[path]
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

func (n *nas) shareNames() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.sharePayloads))
	for _, p := range n.sharePayloads {
		s, _ := p["name"].(string)
		out = append(out, s)
	}
	return out
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
	return volume.ID{Backend: "nas1", Protocol: "smb", Pool: "Pool0", Parent: "k8s", Name: name}
}

func testRequest(name string, bytes int64) backend.CreateRequest {
	return backend.CreateRequest{
		ID:            testID(name),
		CapacityBytes: bytes,
		Params: map[string]string{
			"server": "192.168.10.253",
			// The RESERVED names. Kubernetes reads these off the StorageClass to
			// build the PersistentVolume's nodeStageSecretRef, which is the only
			// way credentials reach NodeStageVolume. This fixture used to carry
			// "secretName"/"secretNamespace", which look right and deliver
			// nothing -- see parseParams.
			"csi.storage.k8s.io/node-stage-secret-name":      "smb-creds",
			"csi.storage.k8s.io/node-stage-secret-namespace": "kube-system",
		},
	}
}

// TestSMBCreateSetsRefquota catches a create that omits refquota: the share then
// reports the WHOLE POOL (31T observed for a 10Gi volume).
func TestSMBCreateSetsRefquota(t *testing.T) {
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
		t.Fatal("pool.dataset.create carried no refquota — the share would report the whole pool")
	}
	if got := int64(q.(float64)); got != 10*gib {
		t.Fatalf("refquota = %d, want %d", got, 10*gib)
	}
}

// TestSMBCreateUsesShareTypeSMB catches a plain dataset: share_type SMB is what
// gives the dataset mode 0770 with an NFSv4 ACL instead of 0755 with none.
func TestSMBCreateUsesShareTypeSMB(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-1")
	if ds == nil {
		t.Fatal("dataset was not created")
	}
	if ds.shareType != "SMB" {
		t.Fatalf("share_type = %q, want SMB", ds.shareType)
	}
}

// TestSMBCreateSetsACLNotPerm catches the NFS permission model leaking into the
// SMB path: an SMB dataset carries an NFSv4 ACL, and filesystem.setperm would
// strip it rather than configure it.
func TestSMBCreateSetsACLNotPerm(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	if _, err := b.Create(context.Background(), testRequest("pvc-1", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := n.CallsTo("filesystem.setacl"); got != 1 {
		t.Fatalf("filesystem.setacl called %d times, want 1", got)
	}
	if got := n.CallsTo("filesystem.setperm"); got != 0 {
		t.Fatalf("filesystem.setperm called %d times — it would strip the SMB dataset's ACL", got)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	dacl, ok := n.setacls[0]["dacl"].([]any)
	if !ok || len(dacl) == 0 {
		t.Fatalf("filesystem.setacl carried no dacl: %v", n.setacls[0])
	}
}

// TestSMBCreateStampsOwnership catches an unmarked dataset, which the delete
// guard would later refuse to remove — leaking the volume forever.
func TestSMBCreateStampsOwnership(t *testing.T) {
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

// TestSMBCreateIsIdempotent catches a repeat CreateVolume that provisions twice
// or reports a conflict for an identical request.
func TestSMBCreateIsIdempotent(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	first, err := b.Create(context.Background(), testRequest("pvc-1", gib))
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second, err := b.Create(context.Background(), testRequest("pvc-1", gib))
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if first.Context["share"] != second.Context["share"] {
		t.Fatalf("share changed between identical creates: %q vs %q",
			first.Context["share"], second.Context["share"])
	}
	if got := n.CallsTo("pool.dataset.create"); got != 1 {
		t.Fatalf("pool.dataset.create called %d times, want 1", got)
	}
	if got := n.CallsTo("sharing.smb.create"); got != 1 {
		t.Fatalf("sharing.smb.create called %d times, want 1", got)
	}

	// A conflicting size must be reported, not silently reconciled.
	_, err = b.Create(context.Background(), testRequest("pvc-1", 2*gib))
	if got := status.Code(err); got != codes.AlreadyExists {
		t.Fatalf("code = %v, want AlreadyExists (err %v)", got, err)
	}
}

// TestSMBCreateRollsBackOnShareFailure catches a partial provision: a dataset
// with no share is an orphan no retry reconciles.
func TestSMBCreateRollsBackOnShareFailure(t *testing.T) {
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

// TestSMBDeleteVerifiesOwnership catches a delete guard that trusts presence
// alone: an unmarked or merely INHERITED marker is operator data.
func TestSMBDeleteVerifiesOwnership(t *testing.T) {
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
	if got := n.CallsTo("sharing.smb.delete"); got != 0 {
		t.Fatalf("sharing.smb.delete called %d times before the ownership check", got)
	}

	// An owned volume goes: share first, then dataset. Absent is success.
	if _, err := b.Create(context.Background(), testRequest("pvc-3", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Delete(context.Background(), testID("pvc-3")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n.dataset("Pool0/k8s/pvc-3") != nil {
		t.Fatal("dataset survived Delete")
	}
	if got := n.CallsTo("sharing.smb.delete"); got != 1 {
		t.Fatalf("sharing.smb.delete called %d times, want 1", got)
	}
	if err := b.Delete(context.Background(), testID("pvc-3")); err != nil {
		t.Fatalf("deleting an absent volume must succeed: %v", err)
	}
}

// TestSMBExpandRejectsShrink catches delegation of the shrink guard to the
// middleware, which SILENTLY PERMITS a refquota shrink below current usage.
func TestSMBExpandRejectsShrink(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-1", &fakeDataset{refquota: 10 * gib, marker: volume.OwnerValue, source: "LOCAL"})
	b := newBackend(t, n)

	_, err := b.Expand(context.Background(), testID("pvc-1"), 5*gib)
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

// TestSMBShareNameWithinLimit catches plain truncation. An SMB share name is
// capped at 80 characters; two long volume names sharing a prefix would collapse
// onto ONE share, so two volumes would address the same data.
func TestSMBShareNameWithinLimit(t *testing.T) {
	prefix := strings.Repeat("a", 90)
	names := []string{prefix + "-one", prefix + "-two"}

	n := newNAS(t)
	b := newBackend(t, n)
	for _, name := range names {
		if _, err := b.Create(context.Background(), testRequest(name, gib)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	got := n.shareNames()
	if len(got) != 2 {
		t.Fatalf("want 2 share creates, got %d (%v)", len(got), got)
	}
	for _, s := range got {
		if len(s) == 0 || len(s) > maxShareName {
			t.Fatalf("share name %q is %d chars, want 1..%d", s, len(s), maxShareName)
		}
	}
	if got[0] == got[1] {
		t.Fatalf("two distinct volumes collapsed onto one share name %q — they would address the same data", got[0])
	}
}

// TestSMBPublishContextCarriesNoPassword catches a credential leak: the node
// gets a Secret REFERENCE, never the password itself.
func TestSMBPublishContextCarriesNoPassword(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)

	r := testRequest("pvc-1", gib)
	r.Params["password"] = "hunter2"
	r.Params["uid"] = "1000"
	r.Params["gid"] = "1000"
	r.Params["fileMode"] = "0660"
	r.Params["dirMode"] = "0770"
	vol, err := b.Create(context.Background(), r)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pc, err := b.PublishContext(context.Background(), testID("pvc-1"))
	if err != nil {
		t.Fatalf("PublishContext: %v", err)
	}
	for _, m := range []map[string]string{pc, vol.Context} {
		for k, v := range m {
			lk, lv := strings.ToLower(k), strings.ToLower(v)
			if strings.Contains(lk, "password") || strings.Contains(lk, "passwd") {
				t.Fatalf("publish context key %q carries a credential", k)
			}
			if strings.Contains(lv, "hunter2") {
				t.Fatalf("publish context %q=%q leaks the password", k, v)
			}
		}
	}

	if pc["protocol"] != Protocol {
		t.Fatalf("protocol = %q, want %q", pc["protocol"], Protocol)
	}
	if pc["server"] != "192.168.10.253" {
		t.Fatalf("server = %q", pc["server"])
	}
	// The node mounts //server/<share NAME>, not a filesystem path.
	if pc["share"] == "" || strings.HasPrefix(pc["share"], "/") {
		t.Fatalf("share = %q, want an SMB share name rather than a path", pc["share"])
	}
	// SMB ownership is mount-time: the client maps uid/gid itself.
	for k, want := range map[string]string{
		"uid": "1000", "gid": "1000", "fileMode": "0660", "dirMode": "0770",
	} {
		if pc[k] != want {
			t.Fatalf("publish context %q = %q, want %q", k, pc[k], want)
		}
	}
	// The secret reference is NOT carried here. Kubernetes builds the
	// PersistentVolume's nodeStageSecretRef from the StorageClass's reserved
	// parameters before this driver is called, and never reads the volume
	// context for it -- echoing the reserved names into the context looked
	// right, delivered nothing, and was why no SMB volume could be mounted.
	for _, k := range []string{nodeStageSecretNameKey, nodeStageSecretNamespaceKey} {
		if _, ok := pc[k]; ok {
			t.Errorf("publish context carries %q, which Kubernetes does not read there "+
				"and which suggests the credentials are handled when they are not", k)
		}
	}
}

// TestSMBRestoreStampsClone catches the clone trap: a ZFS clone inherits neither
// the ownership marker nor refquota, so a restored volume would leak forever and
// report the whole pool.
func TestSMBRestoreStampsClone(t *testing.T) {
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
		t.Fatalf("clone refquota = %d, want %d — the share would report the whole pool", ds.refquota, 4*gib)
	}
	if ds.acltype != "NFSV4" {
		t.Fatalf("clone acltype = %q, want NFSV4 — an SMB share on a POSIX dataset "+
			"cannot carry the NFSv4 ACL the share needs", ds.acltype)
	}
	if got := n.CallsTo("filesystem.setacl"); got != 1 {
		t.Fatalf("filesystem.setacl ran %d times on the clone, want 1", got)
	}
	if err := b.Delete(context.Background(), testID("pvc-restored")); err != nil {
		t.Fatalf("Delete restored volume: %v", err)
	}
}

func TestSMBProtocolIsRegistered(t *testing.T) {
	for _, p := range backend.Protocols() {
		if p == Protocol {
			return
		}
	}
	t.Fatalf("smb is not registered: %v", backend.Protocols())
}

// TestDACLOmitsZeroPermissionEntries pins a failure the live appliance produced:
// mode 0770 gave everyone@ a "none" basic permission, and because no such preset
// exists the middleware validated the entry against the ADVANCED schema and
// rejected the whole ACL with
// "NFS4ACE_AdvancedPerms.BASIC: Extra inputs are not permitted".
func TestDACLOmitsZeroPermissionEntries(t *testing.T) {
	entries := dacl("0770")
	if len(entries) != 2 {
		t.Fatalf("mode 0770 should yield owner@ and group@ only, got %d entries: %+v",
			len(entries), entries)
	}
	for _, e := range entries {
		if e.Tag == truenas.ACLTagEveryone {
			t.Fatal("everyone@ is granted nothing by 0770 and must not appear in the ACL")
		}
		if p := e.Perms["BASIC"]; p == "" || p == truenas.ACLPermNone {
			t.Fatalf("entry %+v carries no usable basic permission", e)
		}
	}
	// A mode that does grant everyone something must still include it.
	if got := len(dacl("0775")); got != 3 {
		t.Fatalf("mode 0775 should yield three entries, got %d", got)
	}
}

// TestSMBParamsWithoutReservedKeysSucceeds pins the fact that the driver never
// sees the reserved node-stage-secret parameters at all.
//
// The external provisioner consumes every "csi.storage.k8s.io/*" parameter off
// the StorageClass and strips it before issuing CreateVolume -- it is how the
// provisioner learns which Secret to reference from the PersistentVolume, and
// the driver is deliberately not told. A CreateVolume-time gate demanding that
// key therefore rejects EVERY smb class, including a correctly written one:
// verified on a real cluster, where a class carrying both reserved keys still
// failed with "an smb StorageClass must name the Secret holding the SMB user".
func TestSMBParamsWithoutReservedKeysSucceeds(t *testing.T) {
	if _, err := parseParams(map[string]string{ParamServer: "nas1.example"}); err != nil {
		t.Fatalf("parseParams rejected a class the provisioner would have stripped: %v", err)
	}
}

// TestSMBParamsRejectSecretNameParameter keeps the operator-facing half of the
// lesson. secretName/secretNamespace look like they name the credential but
// nothing consumes them: Kubernetes reads only the reserved keys, so a class
// written this way provisions, binds, and then fails to mount on every pod.
// One clear error at the first claim beats that.
func TestSMBParamsRejectSecretNameParameter(t *testing.T) {
	for _, key := range []string{ParamSecretName, ParamSecretNamespace} {
		_, err := parseParams(map[string]string{key: "truenas-smb"})
		if err == nil {
			t.Fatalf("parseParams accepted the inert %q parameter", key)
		}
		if !strings.Contains(err.Error(), nodeStageSecretNameKey) {
			t.Errorf("%s: error does not point at the reserved key: %v", key, err)
		}
	}
}

// TestSMBRestoreStampsProtocol pins the protocol marker onto the clone path.
//
// Only the create path stamped it, so every cloned or snapshot-restored volume
// reached the appliance without one. Two readers fall back to a guess when it
// is missing -- the orphan reconciler and per-protocol capacity accounting --
// and for a FILESYSTEM the guess is "nfs", so a cloned SMB volume was reported
// under an nfs volume handle that names nothing.
func TestSMBRestoreStampsProtocol(t *testing.T) {
	n := newNAS(t)
	n.put("Pool0/k8s/pvc-src", &fakeDataset{refquota: gib, marker: volume.OwnerValue, source: "LOCAL"})
	b := newBackend(t, n)

	r := testRequest("pvc-restored", gib)
	r.SourceSnapshot = "Pool0/k8s/pvc-src@snap-1"
	if _, err := b.Create(context.Background(), r); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ds := n.dataset("Pool0/k8s/pvc-restored")
	if got := ds.props[volume.ProtocolProperty]; got != "smb" {
		t.Fatalf("clone protocol property = %q, want \"smb\" — the orphan report "+
			"and capacity accounting would both call this volume nfs", got)
	}
}
