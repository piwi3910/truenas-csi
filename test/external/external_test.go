// Package external_test guards the upstream Kubernetes external-storage E2E
// harness.
//
// Two very different things live here, and the split is deliberate:
//
//   - TestDriverDefinitionsMatchTheDriver runs in a plain `go test ./...`. It
//     needs no cluster, no appliance and no network, and it is the only thing
//     standing between a capability flag and silent drift. e2e.test unmarshals
//     these files STRICTLY, so a mistyped field name is a total run failure
//     discovered an hour into a live suite; and a flag that disagrees with the
//     driver produces a false failure, which is worse than no coverage.
//   - TestExternalStorageSuite shells out to run.sh and needs a real cluster
//     and a real appliance, so it is gated on TRUENAS_E2E_KUBECONFIG exactly as
//     the appliance suite is gated on TRUENAS_ENDPOINT.
package external_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/node"
	"gopkg.in/yaml.v3"
)

// protocols are the driver definitions in this directory, one per StorageClass
// protocol. The list is explicit rather than globbed so that adding a protocol
// backend without adding its definition fails here.
var protocols = []string{"nfs", "iscsi", "nvme", "smb"}

// blockProtocols serve a single block device and therefore cannot be shared
// between nodes; the rest are file shares and can. This is the same split
// supportsAccessMode() makes in internal/csi/controller.go, restated so the two
// cannot silently disagree.
var blockProtocols = map[string]bool{"iscsi": true, "nvme": true}

// knownTopFields, knownDriverInfoFields and knownCapabilities mirror the parts
// of k8s.io/kubernetes/test/e2e/storage/external.driverDefinition that these
// files use. Upstream unmarshals strictly, so an unknown key is fatal there;
// listing the keys here turns that into a fast local failure and documents
// exactly how much of the upstream schema this repository depends on.
var knownTopFields = map[string]bool{
	"StorageClass": true, "SnapshotClass": true, "DriverInfo": true,
	"InlineVolumes": true, "ClientNodeName": true, "Timeouts": true,
}

var knownDriverInfoFields = map[string]bool{
	"Name": true, "InTreePluginName": true, "MaxFileSize": true,
	"SupportedSizeRange": true, "SupportedFsType": true,
	"SupportedMountOption": true, "RequiredMountOption": true,
	"Capabilities": true, "RequiredAccessModes": true,
	"TopologyKeys": true, "NumAllowedTopologies": true,
	"StressTestOptions": true, "VolumeSnapshotStressTestOptions": true,
	"PerformanceTestOptions": true, "TimeoutsSpec": true,
}

var knownCapabilities = map[string]bool{
	"persistence": true, "block": true, "fsGroup": true,
	"volumeMountGroup": true, "exec": true, "snapshotDataSource": true,
	"pvcDataSource": true, "multipods": true, "RWX": true,
	"controllerExpansion": true, "nodeExpansion": true,
	"offlineExpansion": true, "onlineExpansion": true, "volumeLimits": true,
	"singleNodeVolume": true, "topology": true, "capacity": true,
	"readWriteOncePod": true, "multiplePVsSameID": true,
	"fsResizeFromSourceNotSupported": true,
}

// definition is the subset of the upstream schema this repository writes.
type definition struct {
	StorageClass  classRef   `yaml:"StorageClass"`
	SnapshotClass classRef   `yaml:"SnapshotClass"`
	DriverInfo    driverInfo `yaml:"DriverInfo"`
}

type classRef struct {
	FromExistingClassName string `yaml:"FromExistingClassName"`
}

type driverInfo struct {
	Name                 string          `yaml:"Name"`
	SupportedSizeRange   sizeRange       `yaml:"SupportedSizeRange"`
	SupportedFsType      map[string]any  `yaml:"SupportedFsType"`
	SupportedMountOption map[string]any  `yaml:"SupportedMountOption"`
	TopologyKeys         []string        `yaml:"TopologyKeys"`
	NumAllowedTopologies int             `yaml:"NumAllowedTopologies"`
	Capabilities         map[string]bool `yaml:"Capabilities"`
}

type sizeRange struct {
	Min string `yaml:"Min"`
	Max string `yaml:"Max"`
}

func definitionPath(protocol string) string {
	return "testdriver-" + protocol + ".yaml"
}

// loadDefinition parses one file twice: once into the typed shape the tests
// assert on, and once into a free-form map so unknown keys can be reported with
// the same strictness e2e.test applies.
func loadDefinition(t *testing.T, protocol string) (definition, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(definitionPath(protocol))
	if err != nil {
		t.Fatalf("reading the %s driver definition: %v", protocol, err)
	}
	var typed definition
	if err := yaml.Unmarshal(b, &typed); err != nil {
		t.Fatalf("parsing the %s driver definition: %v", protocol, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatalf("parsing the %s driver definition as a map: %v", protocol, err)
	}
	return typed, raw
}

// backendKeyPlaceholder is what the definitions carry in place of the
// per-backend topology key, which run.sh renders from the StorageClass under
// test because it is named after the operator's own backend.
const backendKeyPlaceholder = "__BACKEND_TOPOLOGY_KEY__"

func TestDriverDefinitionsMatchTheDriver(t *testing.T) {
	for _, protocol := range protocols {
		t.Run(protocol, func(t *testing.T) {
			def, raw := loadDefinition(t, protocol)

			// e2e.test rejects an unknown field outright, and it does so after
			// the cluster has been contacted. Catch it here instead.
			for k := range raw {
				if !knownTopFields[k] {
					t.Errorf("unknown top-level field %q: e2e.test unmarshals strictly and will refuse the file", k)
				}
			}
			if di, ok := raw["DriverInfo"].(map[string]any); ok {
				for k := range di {
					if !knownDriverInfoFields[k] {
						t.Errorf("unknown DriverInfo field %q: e2e.test unmarshals strictly and will refuse the file", k)
					}
				}
			}

			// The name is what binds a PersistentVolume to this plugin. A
			// mismatch would silently test whatever else is installed.
			if def.DriverInfo.Name != driver.DriverName {
				t.Errorf("DriverInfo.Name is %q, want %q", def.DriverInfo.Name, driver.DriverName)
			}

			// run.sh substitutes these. A definition that hard-codes a class
			// name would quietly test the wrong StorageClass.
			if def.StorageClass.FromExistingClassName != "__STORAGE_CLASS__" {
				t.Errorf("StorageClass.FromExistingClassName is %q, want the __STORAGE_CLASS__ placeholder",
					def.StorageClass.FromExistingClassName)
			}
			if def.SnapshotClass.FromExistingClassName != "__SNAPSHOT_CLASS__" {
				t.Errorf("SnapshotClass.FromExistingClassName is %q, want the __SNAPSHOT_CLASS__ placeholder",
					def.SnapshotClass.FromExistingClassName)
			}

			for name := range def.DriverInfo.Capabilities {
				if !knownCapabilities[name] {
					t.Errorf("capability %q is not one the framework knows; "+
						"an unrecognised key is silently ignored, which reads as coverage that never ran", name)
				}
			}

			// Every capability that is a fact about the driver rather than a
			// judgement call is asserted, so a future edit to either side has
			// to change both.
			assertCapability(t, def, "persistence", true)
			assertCapability(t, def, "block", blockProtocols[protocol])
			assertCapability(t, def, "RWX", !blockProtocols[protocol])
			assertCapability(t, def, "controllerExpansion", true)
			assertCapability(t, def, "snapshotDataSource", true)
			assertCapability(t, def, "pvcDataSource", true)
			assertCapability(t, def, "capacity", true)
			assertCapability(t, def, "volumeLimits", true)
			// GetPluginCapabilities advertises VolumeExpansion ONLINE only.
			assertCapability(t, def, "onlineExpansion", true)
			assertCapability(t, def, "offlineExpansion", false)
			// NodeGetCapabilities does not advertise VOLUME_MOUNT_GROUP.
			assertCapability(t, def, "volumeMountGroup", false)

			// A file share never sees mkfs, so it must not claim a filesystem
			// the node would be asked to create.
			if !blockProtocols[protocol] {
				if len(def.DriverInfo.SupportedFsType) != 1 {
					t.Errorf("%s is a file share: SupportedFsType must contain only the driver default, got %v",
						protocol, keys(def.DriverInfo.SupportedFsType))
				}
				if _, ok := def.DriverInfo.SupportedFsType[""]; !ok {
					t.Errorf("%s: SupportedFsType must contain the empty (driver default) entry, got %v",
						protocol, keys(def.DriverInfo.SupportedFsType))
				}
			} else {
				// requireFS() in internal/node/node.go is the authority on what
				// the node can create and grow.
				for fs := range def.DriverInfo.SupportedFsType {
					switch fs {
					case "", "ext2", "ext3", "ext4", "xfs":
					default:
						t.Errorf("%s: SupportedFsType names %q, which internal/node/node.go requireFS refuses",
							protocol, fs)
					}
				}
			}

			// The definition must declare EVERY key the node plugin publishes,
			// not just this protocol's.
			//
			// NodeGetInfo reports one segment per probed capability, so a
			// CSIStorageCapacity object's NodeTopology carries all of them.
			// Declaring one key made the upstream capacity suite look for an
			// object keyed on that alone, find none, and fail a driver that was
			// behaving correctly. This test asserted the same single key, so it
			// pinned the mistake rather than catching it.
			declared := map[string]bool{}
			for _, k := range def.DriverInfo.TopologyKeys {
				declared[k] = true
			}
			for _, c := range node.CapabilityOrder() {
				if k := node.TopologyKey(c); !declared[k] {
					t.Errorf("TopologyKeys omits %s, which the node plugin publishes; "+
						"the capacity suite then finds no object keyed on it", k)
				}
			}
			// The per-backend key is named after the operator's backend, so the
			// definition carries a placeholder that run.sh renders.
			if !declared[backendKeyPlaceholder] {
				t.Errorf("TopologyKeys omits %s, so the per-backend label the node "+
					"publishes is never declared", backendKeyPlaceholder)
			}
			if own := node.TopologyKey(node.Capability(protocol)); !declared[own] {
				t.Errorf("TopologyKeys omits this protocol's own key %s", own)
			}
			if def.DriverInfo.Capabilities["topology"] && def.DriverInfo.NumAllowedTopologies < 1 {
				t.Error("topology is claimed but NumAllowedTopologies is unset")
			}

			// CreateVolume defaults an absent capacity range to 1 GiB, so a
			// smaller minimum would test a path the driver never takes.
			if def.DriverInfo.SupportedSizeRange.Min != "1Gi" {
				t.Errorf("SupportedSizeRange.Min is %q, want 1Gi",
					def.DriverInfo.SupportedSizeRange.Min)
			}
		})
	}
}

// TestEveryRegisteredProtocolHasADefinition fails when a protocol backend is
// added without the driver definition that says what it can do. Conformance
// coverage that quietly stops at three of four protocols is the failure mode
// this guards.
func TestEveryRegisteredProtocolHasADefinition(t *testing.T) {
	for _, p := range protocols {
		if _, err := os.Stat(definitionPath(p)); err != nil {
			t.Errorf("no driver definition for protocol %q: %v", p, err)
		}
	}
}

// TestRunScriptIsExecutable catches the commonest way this harness breaks: the
// executable bit lost to a patch or a checkout.
func TestRunScriptIsExecutable(t *testing.T) {
	info, err := os.Stat("run.sh")
	if err != nil {
		t.Fatalf("run.sh: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("run.sh is not executable (mode %v)", info.Mode().Perm())
	}
}

// TestExternalStorageSuite runs the upstream suite against a live cluster and a
// live appliance. It skips loudly when the cluster is absent, rather than
// passing silently and pretending it ran.
func TestExternalStorageSuite(t *testing.T) {
	kubeconfig := os.Getenv("TRUENAS_E2E_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("TRUENAS_E2E_KUBECONFIG is not set: skipping the upstream external-storage suite. " +
			"Set TRUENAS_E2E_KUBECONFIG, TRUENAS_E2E_STORAGECLASS and TRUENAS_E2E_PROTOCOL to run it; " +
			"see test/external/README.md.")
	}
	if os.Getenv("TRUENAS_E2E_STORAGECLASS") == "" || os.Getenv("TRUENAS_E2E_PROTOCOL") == "" {
		t.Fatal("TRUENAS_E2E_KUBECONFIG is set but TRUENAS_E2E_STORAGECLASS or TRUENAS_E2E_PROTOCOL is not")
	}

	// Hours, not minutes: the suite provisions and destroys real volumes on a
	// real appliance. `go test` would kill it at ten minutes without this.
	timeout := 4 * time.Hour
	if v := os.Getenv("TRUENAS_E2E_GOTIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("TRUENAS_E2E_GOTIMEOUT: %v", err)
		}
		timeout = d
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "./run.sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the external-storage suite failed: %v", err)
	}
}

func assertCapability(t *testing.T, def definition, name string, want bool) {
	t.Helper()
	got, ok := def.DriverInfo.Capabilities[name]
	if !ok {
		t.Errorf("capability %q is unset; the framework would default it to false, "+
			"and an unstated capability is indistinguishable from a forgotten one", name)
		return
	}
	if got != want {
		t.Errorf("capability %q is %v, want %v", name, got, want)
	}
}

func keys(m map[string]any) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, fmt.Sprintf("%q", k))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
