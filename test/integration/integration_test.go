package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/csi"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// env describes the appliance under test. Every test skips loudly when it is
// absent, rather than passing silently and pretending it ran.
type env struct {
	cfg    *config.Config
	client *truenas.Client
	prefix string
	server string
	// nodeID is the node an appliance-side grant is written for. It is the
	// cluster node the node-side suite mounts from, not this machine.
	nodeID string
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
	t.Cleanup(func() { _ = c.Close() })
	return &env{cfg: cfg, client: c, prefix: b.Pool + "/" + b.ParentDataset,
		server: get("TRUENAS_DATA_ADDRESS", ""),
		nodeID: os.Getenv("TRUENAS_E2E_NODE")}
}

func (e *env) controller(t *testing.T) csipb.ControllerServer {
	t.Helper()
	reg, err := backend.NewRegistry(context.Background(), e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return csi.NewControllerWithNodes(reg, e.cfg, e.resolver(t))
}

// resolver returns a node resolver that answers for the CLUSTER's nodes.
//
// The default in-cluster resolver cannot be built from a workstation, and the
// fallback answers with this machine's own interfaces -- so a publish would add
// the workstation's addresses to the export and the cluster node would still be
// refused. Building one over the ambient kubeconfig makes the grant name the
// node that actually mounts.
func (e *env) resolver(t *testing.T) backend.NodeResolver {
	t.Helper()
	if e.nodeID == "" || e.nodeID == e.cfg.NodeID {
		return backend.NewLocalNodeResolver(e.cfg.NodeID)
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("building a kubeconfig client for node resolution: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kubernetes client: %v", err)
	}
	return backend.NewKubeNodeResolverFor(cs)
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
// documented roles.
//
// Running the WHOLE suite against such an account is what this is really for,
// and how docs/security.md's list is checked:
//
//	TRUENAS_USERNAME=<least-privilege user> TRUENAS_API_KEY=<its key> go test ./test/integration/
//
// Doing that found two roles the documented set was missing, both of which fail
// far from their cause: SHARING_NVME_TARGET_WRITE (without it nvmet.global.config
// returns EACCES) and SHARING_ISCSI_AUTH_WRITE, where the read-only role makes
// TrueNAS answer with the CHAP secret MASKED rather than refuse, so every iSCSI
// volume provisions and then fails to attach on the node.
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
