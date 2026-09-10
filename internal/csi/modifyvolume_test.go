package csi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// modifyFake is a fake appliance holding exactly one dataset and recording
// every pool.dataset.update it is asked to perform.
//
// Recording the PATCH rather than only the outcome is what lets these tests
// assert the thing that matters most about the allowlist: that a refused
// property never reaches the middleware at all, as opposed to being sent and
// happening to be rejected there.
type modifyFake struct {
	mu      sync.Mutex
	ds      map[string]any
	patches []map[string]any
	// reject, when set, makes pool.dataset.update fail the way the appliance
	// does when ZFS itself refuses the change.
	reject *fake.RPCError
}

// prop is one ZFS property as pool.dataset.query reports it.
func prop(value, source string) map[string]any {
	return map[string]any{"value": value, "source": source, "rawvalue": value}
}

// ownedDataset is a dataset row for a volume this driver provisioned. kind is
// "FILESYSTEM" or "VOLUME"; props are the native ZFS properties it carries.
func ownedDataset(id, kind string, props map[string]any) map[string]any {
	ds := map[string]any{
		"id": id, "type": kind, "mountpoint": "/mnt/" + id,
		"user_properties": map[string]any{
			"io.truenas.csi:managed": prop("truenas-csi", "LOCAL"),
		},
	}
	for k, v := range props {
		ds[k] = v
	}
	return ds
}

// newModifyFake starts a fake appliance serving one dataset row.
func newModifyFake(t *testing.T, dataset map[string]any) (csipb.ControllerServer, *modifyFake) {
	t.Helper()
	m := &modifyFake{ds: dataset}
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.query", func([]json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal([]byte(healthyPool), &v); err != nil {
			return nil, err
		}
		return v, nil
	})
	s.Handle("pool.dataset.query", func(p []json.RawMessage) (any, error) {
		var filters [][]any
		_ = json.Unmarshal(p[0], &filters)
		want, _ := filters[0][2].(string)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ds == nil || m.ds["id"] != want {
			return []any{}, nil
		}
		return []any{m.ds}, nil
	})
	s.Handle("pool.dataset.update", func(p []json.RawMessage) (any, error) {
		var id string
		var patch map[string]any
		_ = json.Unmarshal(p[0], &id)
		_ = json.Unmarshal(p[1], &patch)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.reject != nil {
			return nil, m.reject
		}
		m.patches = append(m.patches, patch)
		// The appliance answers the updated row, and a second identical call
		// must then be a no-op — which only holds if the fake really stores
		// what it was told, in the lowercase spelling ZFS reports.
		for k, v := range patch {
			m.ds[k] = prop(strings.ToLower(toStr(v)), "LOCAL")
		}
		return m.ds, nil
	})
	return ctlWithServer(t, s), m
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

// updates returns the patches recorded so far.
func (m *modifyFake) updates() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any(nil), m.patches...)
}

// fsVolume is the id of the filesystem volume most of these tests act on. It
// reuses getVol so the ownership and confinement rules are the same ones the
// rest of the controller tests exercise.
const fsVolume = getVol

// zvolVolume is an iSCSI volume: the same dataset path served by a zvol, which
// is where the filesystem-only properties have to be refused.
const zvolVolume = "nas1/iscsi/Pool0/k8s/pvc-a"

func modify(t *testing.T, c csipb.ControllerServer, id string, mutable map[string]string) error {
	t.Helper()
	_, err := c.ControllerModifyVolume(context.Background(),
		&csipb.ControllerModifyVolumeRequest{VolumeId: id, MutableParameters: mutable})
	return err
}

// TestModifyVolumeAllowlistIsClosed is the regression that keeps this feature
// from becoming a way to corrupt a volume.
//
// mutable_parameters is chosen by whoever may write a VolumeAttributesClass,
// which is not the same authority as the pool's owner. Every key below is one
// that a passthrough implementation would happily forward to
// pool.dataset.update, and each would either destroy the volume's data, resize
// it behind ControllerExpandVolume's back, or make it unusable to the pods
// mounting it. All of them must be refused BY NAME, and none of them may reach
// the appliance.
func TestModifyVolumeAllowlistIsClosed(t *testing.T) {
	for _, key := range []string{
		// Capacity. Two paths writing a volume's size will disagree, and the
		// loser silently resizes a volume the CO thinks is another size.
		"quota", "refquota", "volsize", "reservation", "refreservation",
		// Immutable on a zvol after creation; ZFS will not change it.
		"volblocksize",
		// Breaks every pod writing to the volume.
		"readonly",
		// Detaches the data from the share serving it.
		"mountpoint",
		// A pool-wide memory commitment one claim must not make.
		"deduplication", "dedup",
		// Not on pool.dataset.update at all: allowlisting them would only
		// produce guaranteed appliance rejections.
		"primarycache", "secondarycache", "logbias",
		// The ownership marker itself: rewriting it would let a volume escape
		// or falsely claim the driver's own delete guard.
		"user_properties", "user_properties_update", "io.truenas.csi:managed",
		// Sharing and encryption, neither of which this driver models.
		"sharenfs", "sharesmb", "encryption", "keylocation",
		// Nonsense, which must be refused exactly as firmly.
		"", "SYNC", "sync ", "../../etc/passwd", "XXX_FakeKey",
	} {
		t.Run("refuses "+key, func(t *testing.T) {
			c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
			err := modify(t, c, fsVolume, map[string]string{key: "on"})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("key %q: code = %v, want InvalidArgument (err %v)",
					key, status.Code(err), err)
			}
			if key != "" && !strings.Contains(err.Error(), key) {
				t.Errorf("key %q: the refusal must name the unsupported key, got %q", key, err)
			}
			if got := f.updates(); len(got) != 0 {
				t.Fatalf("key %q reached the appliance as %v — a refused property must "+
					"never be sent to pool.dataset.update", key, got)
			}
		})
	}
}

// TestMutablePropertyAllowlistIsExactly pins the set itself, next to the
// reasoning for it. Adding a property is a deliberate act with a security
// argument attached; this test makes it impossible to do by accident.
func TestMutablePropertyAllowlistIsExactly(t *testing.T) {
	want := []string{"atime", "compression", "recordsize", "sync"}
	if got := mutablePropertyNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist = %v, want %v — adding or removing a mutable property "+
			"needs the reasoning in modifyvolume.go updated with it", got, want)
	}
}

// TestModifyVolumeRejectsValuesOutsideTheEnum: a known key with an unknown
// value is the driver's fault to catch, not the appliance's. Forwarding it
// would return Internal and make the CO retry a class that can never work.
func TestModifyVolumeRejectsValuesOutsideTheEnum(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"sync", "sometimes"},
		{"sync", "off"},
		{"compression", "brotli"},
		{"compression", "zstd-99"},
		{"atime", "maybe"},
		{"recordsize", "3K"},
		// 32M, not 2M: the appliance accepts every power of two up to 16M, and
		// this row used to assert the driver refuse 2M — encoding the very cap
		// that refused operators a size ZFS supports.
		{"recordsize", "32M"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
			err := modify(t, c, fsVolume, map[string]string{tc.key: tc.value})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err %v)", status.Code(err), err)
			}
			if len(f.updates()) != 0 {
				t.Fatal("a value outside the enum must not reach the appliance")
			}
		})
	}
}

// TestModifyVolumeAppliesProperties covers the normalisation the middleware
// forces on us: a VolumeAttributesClass is written in ZFS's lowercase spelling
// and pool.dataset.update only accepts the uppercase enum.
func TestModifyVolumeAppliesProperties(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		volume  string
		mutable map[string]string
		want    map[string]any
	}{{
		name: "the headline trade, written the natural way",
		kind: "FILESYSTEM", volume: fsVolume,
		mutable: map[string]string{"sync": "disabled"},
		want:    map[string]any{"sync": "DISABLED"},
	}, {
		name: "already uppercase is accepted too",
		kind: "FILESYSTEM", volume: fsVolume,
		mutable: map[string]string{"sync": "ALWAYS"},
		want:    map[string]any{"sync": "ALWAYS"},
	}, {
		name: "compression levels keep their suffix",
		kind: "FILESYSTEM", volume: fsVolume,
		mutable: map[string]string{"compression": "zstd-3"},
		want:    map[string]any{"compression": "ZSTD-3"},
	}, {
		name: "several properties travel in one update",
		kind: "FILESYSTEM", volume: fsVolume,
		mutable: map[string]string{"sync": "standard", "atime": "off", "recordsize": "1m"},
		want:    map[string]any{"sync": "STANDARD", "atime": "OFF", "recordsize": "1M"},
	}, {
		name: "a zvol takes the properties that apply to it",
		kind: "VOLUME", volume: zvolVolume,
		mutable: map[string]string{"sync": "disabled", "compression": "lz4"},
		want:    map[string]any{"sync": "DISABLED", "compression": "LZ4"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", tc.kind, nil))
			if err := modify(t, c, tc.volume, tc.mutable); err != nil {
				t.Fatalf("ControllerModifyVolume: %v", err)
			}
			got := f.updates()
			if len(got) != 1 {
				t.Fatalf("sent %d updates, want exactly 1: %v", len(got), got)
			}
			if !reflect.DeepEqual(got[0], tc.want) {
				t.Fatalf("patch = %v, want %v", got[0], tc.want)
			}
		})
	}
}

// TestModifyVolumeIsIdempotent is the CSI requirement: the CO retries this call,
// so applying the same class twice must succeed and change nothing.
func TestModifyVolumeIsIdempotent(t *testing.T) {
	c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
	class := map[string]string{"sync": "disabled", "compression": "lz4"}

	for i := 0; i < 3; i++ {
		if err := modify(t, c, fsVolume, class); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	if got := f.updates(); len(got) != 1 {
		t.Fatalf("three identical calls produced %d updates, want 1: %v", len(got), got)
	}
}

// TestModifyVolumeInheritIsSatisfiedBySource: INHERIT does not name a value, it
// asks the property to follow the parent. Comparing it as a string would make
// every retry look like a change and rewrite the dataset for ever.
func TestModifyVolumeInheritIsSatisfiedBySource(t *testing.T) {
	inherited := ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", map[string]any{
		"sync": prop("standard", "INHERITED"),
	})
	c, f := newModifyFake(t, inherited)
	if err := modify(t, c, fsVolume, map[string]string{"sync": "inherit"}); err != nil {
		t.Fatalf("ControllerModifyVolume: %v", err)
	}
	if got := f.updates(); len(got) != 0 {
		t.Fatalf("a property already inherited must not be rewritten, sent %v", got)
	}

	// A LOCAL value, on the other hand, has to be cleared.
	local := ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", map[string]any{
		"sync": prop("disabled", "LOCAL"),
	})
	c, f = newModifyFake(t, local)
	if err := modify(t, c, fsVolume, map[string]string{"sync": "inherit"}); err != nil {
		t.Fatalf("ControllerModifyVolume: %v", err)
	}
	got := f.updates()
	if len(got) != 1 || got[0]["sync"] != "INHERIT" {
		t.Fatalf("patch = %v, want a single sync=INHERIT", got)
	}
}

// TestModifyVolumeRefusesFilesystemPropertiesOnAZvol.
//
// atime and recordsize describe a filename namespace, which a zvol does not
// have: it is one block device, and ZFS refuses both on it. Catching that here
// rather than at the appliance is what makes the answer InvalidArgument ("fix
// the class") instead of Internal ("keep retrying").
func TestModifyVolumeRefusesFilesystemPropertiesOnAZvol(t *testing.T) {
	for _, key := range []string{"atime", "recordsize"} {
		t.Run(key, func(t *testing.T) {
			c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "VOLUME", nil))
			value := map[string]string{"atime": "off", "recordsize": "128K"}[key]
			err := modify(t, c, zvolVolume, map[string]string{key: value})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err %v)", status.Code(err), err)
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "zvol") {
				t.Errorf("the refusal must name the property and say why, got %q", err)
			}
			if len(f.updates()) != 0 {
				t.Fatal("a property that does not apply must not reach the appliance")
			}
		})
	}

	// The same properties are fine on a filesystem, which is the other half of
	// the claim: this is a per-kind rule, not a blanket refusal.
	c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
	if err := modify(t, c, fsVolume, map[string]string{"atime": "off"}); err != nil {
		t.Fatalf("atime on a filesystem: %v", err)
	}
	if len(f.updates()) != 1 {
		t.Fatal("atime on a filesystem must be applied")
	}
}

// TestModifyVolumeNotFound: every way of naming a volume that is not there is
// NotFound, because that is what decides whether the CO keeps retrying.
func TestModifyVolumeNotFound(t *testing.T) {
	c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
	for _, tc := range []struct {
		name string
		id   string
		want codes.Code
	}{
		{"no volume id", "", codes.InvalidArgument},
		{"unparseable id", "not-a-handle", codes.NotFound},
		{"unknown backend", "nope/nfs/Pool0/k8s/pvc-a", codes.NotFound},
		{"dataset is gone", "nas1/nfs/Pool0/k8s/pvc-ghost", codes.NotFound},
		{"outside the backend's parent dataset", "nas1/nfs/Pool0/other/pvc-a", codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := modify(t, c, tc.id, map[string]string{"sync": "standard"})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v, want %v (err %v)", got, tc.want, err)
			}
		})
	}
	if len(f.updates()) != 0 {
		t.Fatal("nothing above names a volume of ours; none may be modified")
	}
}

// TestModifyVolumeRefusesADatasetWeDoNotOwn.
//
// The marker is INHERITED from the parent, which is exactly the case a
// presence-only check gets wrong: an operator who ever set the property on the
// parent dataset by hand would otherwise have every dataset beneath it — their
// real data — writable by any VolumeAttributesClass.
func TestModifyVolumeRefusesADatasetWeDoNotOwn(t *testing.T) {
	ds := ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil)
	ds["user_properties"] = map[string]any{
		"io.truenas.csi:managed": prop("truenas-csi", "INHERITED"),
	}
	c, f := newModifyFake(t, ds)

	err := modify(t, c, fsVolume, map[string]string{"sync": "disabled"})
	if status.Code(err) == codes.OK {
		t.Fatal("a dataset this driver did not create must not be modified")
	}
	if len(f.updates()) != 0 {
		t.Fatalf("an unowned dataset was modified anyway: %v", f.updates())
	}
}

// TestModifyVolumeApplianceRefusalIsInternal.
//
// The distinction decides whether the CO retries. A property the driver
// understands and accepted, refused by the appliance, is a condition on the
// appliance — the class is still valid and will apply once the condition
// clears, so it must not come back as InvalidArgument.
func TestModifyVolumeApplianceRefusalIsInternal(t *testing.T) {
	c, f := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
	f.mu.Lock()
	f.reject = &fake.RPCError{Code: -32602, ErrName: "EINVAL",
		Reason: "[EROFS] pool is read-only"}
	f.mu.Unlock()

	err := modify(t, c, fsVolume, map[string]string{"sync": "disabled"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal (err %v)", status.Code(err), err)
	}
}

// TestModifyVolumeSyncDisabledIsLoud.
//
// sync=disabled means acknowledged writes can be lost on power failure. The
// driver does not refuse it — an informed operator is entitled to that trade —
// but it must never happen silently, so the warning naming the volume is part
// of the contract and not a nicety.
func TestModifyVolumeSyncDisabledIsLoud(t *testing.T) {
	var log bytes.Buffer
	obs.SetLogOutput(&log, slog.LevelDebug)
	t.Cleanup(func() { obs.SetLogOutput(nil, slog.LevelInfo) })

	c, _ := newModifyFake(t, ownedDataset("Pool0/k8s/pvc-a", "FILESYSTEM", nil))
	if err := modify(t, c, fsVolume, map[string]string{"sync": "disabled"}); err != nil {
		t.Fatalf("ControllerModifyVolume: %v", err)
	}
	out := log.String()
	if !strings.Contains(out, "WARN") {
		t.Errorf("sync=disabled must log at WARN, got:\n%s", out)
	}
	for _, want := range []string{fsVolume, "sync=disabled", "lose"} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning must mention %q, got:\n%s", want, out)
		}
	}

	// And again on a re-apply that changes nothing: an operator reconciling the
	// same class must still be told what the volume is running with.
	log.Reset()
	if err := modify(t, c, fsVolume, map[string]string{"sync": "disabled"}); err != nil {
		t.Fatalf("second ControllerModifyVolume: %v", err)
	}
	if !strings.Contains(log.String(), "WARN") {
		t.Errorf("re-applying sync=disabled must warn as well, got:\n%s", log.String())
	}
}

// TestCreateVolumeRejectsUnknownMutableParameters.
//
// A VolumeAttributesClass can be named on a claim at creation as well as later,
// so CreateVolume enforces the same allowlist — and must do so before it
// provisions anything, or a class nobody can fix leaves a dataset behind.
func TestCreateVolumeRejectsUnknownMutableParameters(t *testing.T) {
	shared = newCounting()
	c, s := ctlWith(t)

	_, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-vac", Parameters: params(), VolumeCapabilities: testCaps(),
		CapacityRange:     &csipb.CapacityRange{RequiredBytes: 1 << 30},
		MutableParameters: map[string]string{"XXX_FakeKey": "XXX_FakeValue"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err %v)", status.Code(err), err)
	}
	if shared.creates.Load() != 0 {
		t.Fatal("a class the driver refuses must not provision a volume")
	}
	for _, call := range s.Calls() {
		if call != "auth.login_ex" && call != "system.info" {
			t.Fatalf("a refused class must reach no middleware call, saw %q", call)
		}
	}
}

// TestCreateVolumeAcceptsAnEmptyClass: a VolumeAttributesClass with no
// parameters is a legal object, and refusing it would wedge the CO in a retry
// loop over something retrying cannot fix.
func TestCreateVolumeAcceptsAnEmptyClass(t *testing.T) {
	shared = newCounting()
	c, _ := ctlWith(t)

	if _, err := c.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{
		Name: "pvc-empty-vac", Parameters: params(), VolumeCapabilities: testCaps(),
		CapacityRange:     &csipb.CapacityRange{RequiredBytes: 1 << 30},
		MutableParameters: map[string]string{},
	}); err != nil {
		t.Fatalf("an empty class must be accepted, got %v", err)
	}
}

// TestRecordsizeAcceptsEveryValueTheApplianceDoes.
//
// recordsize is the one modifiable property pool.dataset.update does NOT
// declare an enum for — its schema is a bare string — so the driver states the
// set itself. It stopped at 1M, and the appliance accepts up to 16M: measured
// on 25.10.6, where 2M, 4M, 8M and 16M were all accepted and reported back
// verbatim, while 256 and 32M were refused as "an invalid recordsize".
//
// An operator asking for a large recordsize — the usual choice for big
// sequential files — was therefore refused a setting the appliance supports,
// by the driver rather than by ZFS.
func TestRecordsizeAcceptsEveryValueTheApplianceDoes(t *testing.T) {
	var rs *mutableProperty
	for i := range mutableProperties {
		if mutableProperties[i].name == "recordsize" {
			rs = &mutableProperties[i]
		}
	}
	if rs == nil {
		t.Fatal("recordsize is no longer a modifiable property")
	}
	for _, v := range []string{"512", "1K", "128K", "1M", "2M", "4M", "8M", "16M"} {
		if !rs.values[v] {
			t.Errorf("recordsize %q is refused by the driver and accepted by the appliance", v)
		}
	}
	for _, v := range []string{"256", "32M", "3M"} {
		if rs.values[v] {
			t.Errorf("recordsize %q is accepted by the driver and refused by the appliance", v)
		}
	}
}
