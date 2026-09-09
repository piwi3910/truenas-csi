package truenas

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

func dialFake(t *testing.T, s *fake.Server) *Client {
	t.Helper()
	c, err := Dial(context.Background(), backendFor(s.URL()))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestDatasetQueryAbsentReturnsNil pins the single most dangerous quirk of this
// API: for a missing dataset the middleware answers code -32602 with
// errname EINVAL, while the TRUE errno appears only as a "[ENOENT]" prefix
// inside reason. Trusting errname would turn NOT_FOUND into INVALID_ARGUMENT
// and break CSI idempotency.
func TestDatasetQueryAbsentReturnsNil(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		return nil, &fake.RPCError{
			Code: -32602, ErrName: "EINVAL",
			Reason: "[ENOENT] None: PoolDataset Pool0/k8s/nope does not exist",
		}
	})
	c := dialFake(t, s)
	ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/nope")
	if err != nil {
		t.Fatalf("a missing dataset must not be an error, got %v", err)
	}
	if ds != nil {
		t.Fatalf("want nil dataset, got %+v", ds)
	}
}

func TestDatasetQueryEmptyListReturnsNil(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("pool.dataset.query", []any{})
	c := dialFake(t, s)
	ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/nope")
	if err != nil || ds != nil {
		t.Fatalf("want (nil,nil), got (%+v,%v)", ds, err)
	}
}

func TestDatasetQueryDecodesRealShape(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	// This is the literal shape the appliance returned during research.
	s.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		var v any
		_ = json.Unmarshal([]byte(`[{
		  "id":"Pool0/k8s/pvc-1","type":"VOLUME","mountpoint":"",
		  "volsize":{"parsed":2147483648,"rawvalue":"2147483648","value":"2 GiB","source":"LOCAL"},
		  "refquota":{"parsed":null},
		  "user_properties":{"io.truenas.csi:managed":{"parsed":"truenas-csi","value":"truenas-csi","source":"LOCAL"}}
		}]`), &v)
		return v, nil
	})
	c := dialFake(t, s)
	ds, err := c.DatasetQuery(context.Background(), "Pool0/k8s/pvc-1")
	if err != nil || ds == nil {
		t.Fatalf("query: %+v %v", ds, err)
	}
	if ds.VolSize.Parsed != 2147483648 {
		t.Errorf("volsize decoded as %d", ds.VolSize.Parsed)
	}
	if !ds.Owned("io.truenas.csi:managed", "truenas-csi") {
		t.Error("dataset with a LOCAL marker should read as owned")
	}
}

func TestOwnedRequiresLocalSource(t *testing.T) {
	mk := func(src string) *Dataset {
		d := &Dataset{UserProperties: map[string]propField{}}
		d.UserProperties["io.truenas.csi:managed"] = propField{Property{Value: "truenas-csi", Source: src}}
		return d
	}
	if mk("INHERITED").Owned("io.truenas.csi:managed", "truenas-csi") {
		t.Fatal("an INHERITED marker must NOT read as owned — children inherit it, and " +
			"treating inheritance as ownership would let the driver delete pre-existing data")
	}
	if !mk("LOCAL").Owned("io.truenas.csi:managed", "truenas-csi") {
		t.Fatal("a LOCAL marker must read as owned")
	}
}

func TestSetPermPollsJobToCompletion(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("filesystem.setperm", 9615)
	polls := 0
	s.Handle("core.get_jobs", func([]json.RawMessage) (any, error) {
		polls++
		state := "RUNNING"
		if polls >= 3 {
			state = "SUCCESS"
		}
		return []any{map[string]any{"id": 9615, "state": state}}, nil
	})
	c := dialFake(t, s)
	if err := c.SetPerm(context.Background(), "/mnt/Pool0/k8s/pvc-1", "0777", 0, 0); err != nil {
		t.Fatalf("SetPerm: %v", err)
	}
	if polls < 3 {
		t.Fatalf("polled %d times, expected to wait for SUCCESS", polls)
	}
}

func TestSetPermFailsOnJobFailure(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("filesystem.setperm", 42)
	s.HandleValue("core.get_jobs", []any{
		map[string]any{"id": 42, "state": "FAILED", "error": "permission denied on /mnt/Pool0"},
	})
	c := dialFake(t, s)
	err := c.SetPerm(context.Background(), "/mnt/Pool0/k8s/pvc-1", "0777", 0, 0)
	if err == nil {
		t.Fatal("want an error when the job FAILS")
	}
	if !strings.Contains(err.Error(), "permission denied on /mnt/Pool0") {
		t.Fatalf("error should carry the job's own message, got %v", err)
	}
}

func TestSetPermTimesOut(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("filesystem.setperm", 7)
	s.HandleValue("core.get_jobs", []any{map[string]any{"id": 7, "state": "RUNNING"}})
	c := dialFake(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.SetPerm(ctx, "/mnt/Pool0/k8s/pvc-1", "0777", 0, 0)
	if err == nil {
		t.Fatal("a job that never completes must time out, not block forever")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %s to give up", time.Since(start))
	}
}

// TestSizeFieldAcceptsBothShapes pins a difference the live appliance revealed:
// pool.query returns plain integers for size and free, while
// pool.dataset.query wraps them in {"parsed": N}. Handling only the wrapper
// made every pool report 0 bytes free, which would make the scheduler treat a
// healthy 44 TB pool as full and leave every claim Pending.
func TestSizeFieldAcceptsBothShapes(t *testing.T) {
	var wrapped struct {
		Free sizeField `json:"free"`
	}
	if err := json.Unmarshal([]byte(`{"free":{"parsed":44861949222912}}`), &wrapped); err != nil {
		t.Fatal(err)
	}
	if wrapped.Free.Parsed != 44861949222912 {
		t.Errorf("wrapped shape decoded as %d", wrapped.Free.Parsed)
	}

	var plain struct {
		Free sizeField `json:"free"`
	}
	if err := json.Unmarshal([]byte(`{"free":44861949222912}`), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Free.Parsed != 44861949222912 {
		t.Errorf("plain integer shape decoded as %d — pool.query returns this form",
			plain.Free.Parsed)
	}

	var null struct {
		Free sizeField `json:"free"`
	}
	if err := json.Unmarshal([]byte(`{"free":null}`), &null); err != nil {
		t.Fatal(err)
	}
	if null.Free.Parsed != 0 {
		t.Errorf("null should decode as 0, got %d", null.Free.Parsed)
	}
}

// TestOriginDecodesTheMachineForm pins a middleware quirk that made clone
// detection silently useless.
//
// pool.dataset.query returns origin's "value" UPPERCASED — verified on a real
// appliance (25.10.6), which answered
//
//	{"parsed": "Pool0/k8s/osrc@Snap1", "rawvalue": "Pool0/k8s/osrc@Snap1",
//	 "value": "POOL0/K8S/OSRC@SNAP1"}
//
// so every comparison of Origin against a real dataset id failed. Nothing
// errored: the dependent-clone refusal simply never managed to name the clones
// it exists to name, and any other reader of Origin would silently see no
// clone at all.
func TestOriginDecodesTheMachineForm(t *testing.T) {
	const body = `{"id":"Pool0/k8s/oclone","type":"FILESYSTEM",
	  "origin":{"parsed":"Pool0/k8s/osrc@Snap1","rawvalue":"Pool0/k8s/osrc@Snap1",
	            "source":"NONE","value":"POOL0/K8S/OSRC@SNAP1"}}`
	var ds Dataset
	if err := json.Unmarshal([]byte(body), &ds); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := ds.Origin.RawValue; got != "Pool0/k8s/osrc@Snap1" {
		t.Fatalf("Origin.RawValue = %q, want the true snapshot name", got)
	}
}

// TestClonesOfMatchesRealNames drives the same quirk through the caller.
func TestClonesOfMatchesRealNames(t *testing.T) {
	srv := fake.Start(t, fake.Options{})
	srv.Handle("pool.dataset.query", func([]json.RawMessage) (any, error) {
		return []any{
			map[string]any{"id": "Pool0/k8s/src", "type": "FILESYSTEM"},
			map[string]any{"id": "Pool0/k8s/clone", "type": "FILESYSTEM",
				"origin": map[string]any{
					"parsed": "Pool0/k8s/src@s1", "rawvalue": "Pool0/k8s/src@s1",
					"source": "NONE", "value": "POOL0/K8S/SRC@S1"}},
		}, nil
	})
	got := dialFake(t, srv).clonesOf(context.Background(), "Pool0/k8s/src")
	if len(got) != 1 || got[0] != "Pool0/k8s/clone" {
		t.Fatalf("clonesOf = %v, want [Pool0/k8s/clone] — the refusal could never "+
			"name the volume blocking the delete", got)
	}
}
