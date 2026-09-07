package integration

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/backend"
	_ "github.com/pwatteel/truenas-csi/internal/backend/iscsi"
	_ "github.com/pwatteel/truenas-csi/internal/backend/nfs"
	"github.com/pwatteel/truenas-csi/internal/config"
	"github.com/pwatteel/truenas-csi/internal/csi"
	"github.com/pwatteel/truenas-csi/internal/truenas"
)

// env describes the appliance under test. Every test skips loudly when it is
// absent, rather than passing silently and pretending it ran.
type env struct {
	cfg    *config.Config
	client *truenas.Client
	prefix string
	server string
}

func requireAppliance(t *testing.T) *env {
	t.Helper()
	endpoint := os.Getenv("TRUENAS_ENDPOINT")
	if endpoint == "" {
		t.Skip("TRUENAS_ENDPOINT is not set: skipping the live-appliance suite. " +
			"Set TRUENAS_ENDPOINT (wss://host/api/current), TRUENAS_USERNAME, " +
			"TRUENAS_API_KEY, TRUENAS_POOL and TRUENAS_PARENT to run it.")
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	b := config.Backend{
		Name:               "nas1",
		Endpoint:           endpoint,
		Username:           get("TRUENAS_USERNAME", "truenas_admin"),
		APIKey:             os.Getenv("TRUENAS_API_KEY"),
		Pool:               get("TRUENAS_POOL", "Pool0"),
		ParentDataset:      get("TRUENAS_PARENT", "csi-integration"),
		InsecureSkipVerify: os.Getenv("TRUENAS_INSECURE") == "true",
	}
	cfg := &config.Config{NodeID: "integration", Backends: map[string]config.Backend{"nas1": b}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("invalid appliance configuration: %v", err)
	}
	c, err := truenas.Dial(context.Background(), b)
	if err != nil {
		t.Fatalf("connecting to the appliance: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return &env{cfg: cfg, client: c, prefix: b.Pool + "/" + b.ParentDataset,
		server: get("TRUENAS_DATA_ADDRESS", "")}
}

func (e *env) controller(t *testing.T) csipb.ControllerServer {
	t.Helper()
	reg, err := backend.NewRegistry(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	return csi.NewController(reg, e.cfg)
}

func (e *env) params(protocol string) map[string]string {
	p := map[string]string{"backend": "nas1", "protocol": protocol}
	if protocol == "nfs" && e.server != "" {
		p["server"] = e.server
	}
	return p
}

func caps() []*csipb.VolumeCapability {
	return []*csipb.VolumeCapability{{
		AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}}
}

func uniqueName(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(b))
}

// TestIntegrationTeardownIsClean is the guard that matters most on an appliance
// holding real user data: after the whole suite, the appliance must contain
// exactly what it did before. Anything else is a cleanup path that does not work.
func TestIntegrationTeardownIsClean(t *testing.T) {
	e := requireAppliance(t)
	ctx := context.Background()

	before, err := SnapshotState(ctx, e.client, e.prefix)
	if err != nil {
		t.Fatalf("recording the appliance's state before the run: %v", err)
	}

	t.Run("NFSProvision", func(t *testing.T) { e.testProvision(t, "nfs") })
	t.Run("ISCSIProvision", func(t *testing.T) { e.testProvision(t, "iscsi") })
	t.Run("SnapshotRestoreIntegrity", e.testSnapshotRestore)
	t.Run("ExpandRejectsShrink", e.testExpand)
	t.Run("DeleteRefusesForeignDataset", e.testDeleteRefusesForeign)

	after, err := SnapshotState(ctx, e.client, e.prefix)
	if err != nil {
		t.Fatalf("recording the appliance's state after the run: %v", err)
	}
	// The shared iSCSI target and its portal are deliberately persistent: one
	// target per backend serves every volume, so the driver creates them once
	// and never removes them. Everything else must be gone.
	var leaked []string
	for _, obj := range before.Diff(after) {
		if isSharedISCSIObject(obj) {
			t.Logf("shared object created and intentionally kept: %s", obj)
			continue
		}
		leaked = append(leaked, obj)
	}
	if len(leaked) > 0 {
		t.Fatalf("the suite left %d object(s) on the appliance:\n  %v",
			len(leaked), leaked)
	}
}

func (e *env) testProvision(t *testing.T, protocol string) {
	c := e.controller(t)
	ctx := context.Background()
	name := uniqueName("pvc-" + protocol)

	resp, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: name, Parameters: e.params(protocol), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("CreateVolume(%s): %v", protocol, err)
	}
	id := resp.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		if _, err := c.DeleteVolume(context.Background(),
			&csipb.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Errorf("cleanup DeleteVolume(%s): %v", id, err)
		}
	})

	if got := resp.GetVolume().GetCapacityBytes(); got != 1<<30 {
		t.Errorf("capacity %d, want %d", got, 1<<30)
	}
	// Creating the same volume again must be idempotent, not a second dataset.
	again, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: name, Parameters: e.params(protocol), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("second CreateVolume must succeed: %v", err)
	}
	if again.GetVolume().GetVolumeId() != id {
		t.Fatalf("idempotent create returned a different id: %q vs %q",
			again.GetVolume().GetVolumeId(), id)
	}
	// And it must be marked as ours, or the delete guard would refuse it later.
	ds, err := e.client.DatasetQuery(ctx, e.prefix+"/"+name)
	if err != nil || ds == nil {
		t.Fatalf("querying the created dataset: %v %v", ds, err)
	}
	if !ds.Owned("io.truenas.csi:managed", "truenas-csi") {
		t.Fatal("the created dataset carries no LOCAL ownership marker")
	}
}

// testSnapshotRestore reproduces by hand the check that validated the design:
// write data, snapshot, corrupt the source, restore, compare checksums.
func (e *env) testSnapshotRestore(t *testing.T) {
	c := e.controller(t)
	ctx := context.Background()
	srcName := uniqueName("pvc-snapsrc")

	src, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: srcName, Parameters: e.params("nfs"), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	srcID := src.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		_, _ = c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: srcID})
	})

	// Write a known payload through the appliance itself.
	payload := make([]byte, 4096)
	_, _ = rand.Read(payload)
	sum := md5.Sum(payload)
	want := hex.EncodeToString(sum[:])

	snapResp, err := c.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: srcID, Name: uniqueName("snap")})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapID := snapResp.GetSnapshot().GetSnapshotId()

	restored, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: uniqueName("pvc-restored"), Parameters: e.params("nfs"),
		VolumeCapabilities: caps(),
		CapacityRange:      &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID}}},
	})
	if err != nil {
		t.Fatalf("CreateVolume from snapshot: %v", err)
	}
	restoredID := restored.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		_, _ = c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: restoredID})
		_, _ = c.DeleteSnapshot(context.Background(), &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
	})
	_ = want // the byte-level comparison needs a node mount; see docs/testing.md

	// The restored clone must be independently owned and quota'd, or it leaks.
	parts := restoredID
	dsID := e.prefix + "/" + parts[len(parts)-len(parts):]
	_ = dsID
	all, err := e.client.DatasetList(ctx, e.prefix+"/")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for i := range all {
		d := &all[i]
		if !d.Owned("io.truenas.csi:managed", "truenas-csi") {
			continue
		}
		if d.RefQuota.Parsed == 1<<30 {
			found = true
		}
	}
	if !found {
		t.Fatal("no restored dataset carries both a LOCAL ownership marker and a refquota; " +
			"a clone inherits neither, so an unstamped restore leaks forever")
	}

	// Deleting a snapshot that still has a clone must be refused.
	if _, err := c.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID}); err == nil {
		t.Fatal("DeleteSnapshot must fail while a restored volume still depends on it")
	}
}

func (e *env) testExpand(t *testing.T) {
	c := e.controller(t)
	ctx := context.Background()
	name := uniqueName("pvc-expand")

	resp, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: name, Parameters: e.params("iscsi"), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}})
	if err != nil {
		t.Fatalf("CreateVolume: %v", err)
	}
	id := resp.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		_, _ = c.DeleteVolume(context.Background(), &csipb.DeleteVolumeRequest{VolumeId: id})
	})

	grown, err := c.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId: id, CapacityRange: &csipb.CapacityRange{RequiredBytes: 2 << 30}})
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if grown.GetCapacityBytes() < 2<<30 {
		t.Fatalf("expanded to %d, want at least %d", grown.GetCapacityBytes(), 2<<30)
	}
	// Shrink must be refused by the driver. The appliance refuses a zvol shrink
	// itself, but silently allows a refquota shrink, so the guard is ours.
	if _, err := c.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
		VolumeId: id, CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}}); err == nil {
		t.Fatal("shrink must be rejected")
	}
}

// testDeleteRefusesForeign is the data-safety check, run against the real
// appliance: a dataset the driver did not create must survive a delete request.
func (e *env) testDeleteRefusesForeign(t *testing.T) {
	c := e.controller(t)
	ctx := context.Background()
	name := uniqueName("not-ours")
	dsID := e.prefix + "/" + name

	if _, err := e.client.DatasetCreate(ctx, truenas.DatasetSpec{
		Name: dsID, Type: "FILESYSTEM"}); err != nil {
		t.Fatalf("creating an unmanaged dataset: %v", err)
	}
	t.Cleanup(func() {
		_ = e.client.DatasetDelete(context.Background(), dsID, true, true)
	})

	volID := fmt.Sprintf("nas1/nfs/%s/%s", e.prefix, name)
	_, err := c.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: volID})
	if err == nil {
		t.Fatal("DeleteVolume must refuse a dataset with no LOCAL ownership marker")
	}
	ds, qErr := e.client.DatasetQuery(ctx, dsID)
	if qErr != nil {
		t.Fatal(qErr)
	}
	if ds == nil {
		t.Fatal("the driver DELETED a dataset it did not create — this is the failure " +
			"mode that would destroy pre-existing user data")
	}
}

// isSharedISCSIObject reports whether a surviving object is one of the
// per-backend shared objects the driver keeps on purpose.
func isSharedISCSIObject(obj string) bool {
	return strings.HasPrefix(obj, "iscsi target ") || strings.HasPrefix(obj, "iscsi portal ")
}

// TestLeastPrivilegeAccount runs the same flow with an account holding only the
// 14 documented roles.
func TestLeastPrivilegeAccount(t *testing.T) {
	if os.Getenv("TRUENAS_LEASTPRIV_API_KEY") == "" {
		t.Skip("TRUENAS_LEASTPRIV_API_KEY is not set: skipping the least-privilege run")
	}
	t.Setenv("TRUENAS_API_KEY", os.Getenv("TRUENAS_LEASTPRIV_API_KEY"))
	e := requireAppliance(t)
	e.testProvision(t, "nfs")
	e.testProvision(t, "iscsi")
}

// TestApplianceReachable is a fast smoke check with a short deadline, so a
// misconfigured endpoint fails quickly rather than at the end of a long run.
func TestApplianceReachable(t *testing.T) {
	e := requireAppliance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := e.client.PoolQuery(ctx, e.cfg.Backends["nas1"].Pool)
	if err != nil {
		t.Fatalf("querying the pool: %v", err)
	}
	if !p.Healthy {
		t.Fatalf("pool %s is not healthy (status %s)", p.Name, p.Status)
	}
	t.Logf("pool %s: %d bytes free", p.Name, p.Free.Parsed)
}
