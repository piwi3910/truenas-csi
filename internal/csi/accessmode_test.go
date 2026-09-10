package csi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/node"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
)

// allAccessModes is every mode the spec defines, so a test that walks them
// cannot miss one by listing the modes it already thought about.
var allAccessModes = []csipb.VolumeCapability_AccessMode_Mode{
	csipb.VolumeCapability_AccessMode_UNKNOWN,
	csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
	csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
	csipb.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
	csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
	csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
}

// TestRawBlockProtocolsNeverGetAMultiNodeMode is the test this whole file
// exists for.
//
// A zvol served as an iSCSI LUN or an NVMe-oF namespace is a byte range with no
// arbitration of any kind. Two nodes mounting ext4 on it read-write each
// believe they own the journal and the allocation bitmaps, and the filesystem
// is destroyed — usually minutes or hours after the second mount, so the damage
// is not traced back to the mount that caused it. Granting one of these
// protocols a MULTI_NODE mode is therefore not a feature regression, it is data
// loss, and it is exactly the change somebody widening SMB's access modes later
// is most likely to make by accident.
func TestRawBlockProtocolsNeverGetAMultiNodeMode(t *testing.T) {
	multiNode := []csipb.VolumeCapability_AccessMode_Mode{
		csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	}
	for protocol, class := range protocolAccessClass {
		if class != classRawBlock {
			continue
		}
		for _, m := range multiNode {
			if supportsAccessMode(protocol, m) {
				t.Fatalf("protocol %q is a raw block device and must never accept %s: "+
					"two nodes writing the same zvol corrupt the filesystem on it",
					protocol, m)
			}
		}
	}
	// And the classification itself must not have been quietly widened.
	for _, protocol := range []string{"iscsi", "nvme"} {
		if accessClassOf(protocol) != classRawBlock {
			t.Fatalf("protocol %q must stay classified as raw block: it serves a zvol, "+
				"and the client's kernel owns the filesystem on it", protocol)
		}
	}
}

// TestSupportsAccessMode pins the whole matrix, so widening or narrowing any
// protocol is a deliberate edit to this table rather than a side effect.
func TestSupportsAccessMode(t *testing.T) {
	cases := []struct {
		protocol string
		mode     csipb.VolumeCapability_AccessMode_Mode
		want     bool
	}{
		// A shared filesystem is served by a server that arbitrates it.
		{"nfs", csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true},
		{"nfs", csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, true},
		{"nfs", csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, true},
		// SMB is a shared filesystem too: serving one share to many clients at
		// once is what the protocol is for, and refusing RWX for it was a bug
		// that made every RWX SMB PVC unbindable.
		{"smb", csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, true},
		{"smb", csipb.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER, true},
		{"smb", csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, true},
		{"smb", csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true},
		// Raw block: one node, whichever flavour of it.
		{"iscsi", csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true},
		{"iscsi", csipb.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER, true},
		{"iscsi", csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, true},
		{"iscsi", csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false},
		{"nvme", csipb.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER, true},
		{"nvme", csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY, false},
		// UNKNOWN is not a mode any volume can be served under.
		{"nfs", csipb.VolumeCapability_AccessMode_UNKNOWN, false},
		{"iscsi", csipb.VolumeCapability_AccessMode_UNKNOWN, false},
		// An unclassified protocol falls back to the safe answer, not to "yes".
		{"gluster-over-carrier-pigeon", csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER, false},
		{"gluster-over-carrier-pigeon", csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, true},
	}
	for _, tc := range cases {
		t.Run(tc.protocol+"/"+tc.mode.String(), func(t *testing.T) {
			if got := supportsAccessMode(tc.protocol, tc.mode); got != tc.want {
				t.Fatalf("supportsAccessMode(%q, %s) = %t, want %t", tc.protocol, tc.mode, got, tc.want)
			}
		})
	}
}

// TestSharedFilesystemsAcceptEveryRealMode: the point of the shared class is
// that the appliance arbitrates access, so there is no mode to withhold.
func TestSharedFilesystemsAcceptEveryRealMode(t *testing.T) {
	for protocol, class := range protocolAccessClass {
		if class != classSharedFilesystem {
			continue
		}
		for _, m := range allAccessModes {
			want := m != csipb.VolumeCapability_AccessMode_UNKNOWN
			if got := supportsAccessMode(protocol, m); got != want {
				t.Fatalf("supportsAccessMode(%q, %s) = %t, want %t", protocol, m, got, want)
			}
		}
	}
}

// TestEveryShippedProtocolIsClassified reads the backend packages rather than
// the registry, because the registry only holds what a given test binary
// imported — and a new protocol package is exactly the thing that would not be
// imported here.
//
// A protocol added without a classification defaults to raw block, which is
// safe but silently wrong for a fifth shared filesystem. This makes that
// omission a failing test at the moment the protocol is added.
func TestEveryShippedProtocolIsClassified(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "backend", "*"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^const Protocol = "([^"]+)"`)
	found := 0
	for _, dir := range dirs {
		files, gErr := filepath.Glob(filepath.Join(dir, "*.go"))
		if gErr != nil {
			t.Fatal(gErr)
		}
		for _, f := range files {
			b, rErr := os.ReadFile(f)
			if rErr != nil {
				t.Fatal(rErr)
			}
			m := re.FindSubmatch(b)
			if m == nil {
				continue
			}
			found++
			protocol := string(m[1])
			if _, ok := protocolAccessClass[protocol]; !ok {
				t.Errorf("%s ships protocol %q, which protocolAccessClass does not classify: "+
					"decide whether it serves a shared filesystem or a raw block device, "+
					"because the default (raw block) silently refuses every RWX claim",
					f, protocol)
			}
		}
	}
	if found < 4 {
		t.Fatalf("found only %d protocol packages; this test is no longer reading them", found)
	}
}

// TestBlockVolumesRefusedForFileProtocols.
//
// NFS and SMB serve a filesystem the appliance arbitrates; neither can hand a
// node a block device. CreateVolume accepted volumeMode: Block against them
// anyway, so the claim BOUND and a dataset was provisioned that nothing could
// ever use — the pod then sat Pending with MapVolume.MapPodDevice failing
// "protocol \"nfs\" has no block device", observed on a real cluster.
//
// The node's refusal was correct and correctly typed; the problem is where it
// happened. CSI requires CreateVolume to answer INVALID_ARGUMENT when the
// requested capabilities cannot be served, so that no volume is created.
func TestBlockVolumesRefusedForFileProtocols(t *testing.T) {
	block := &csipb.VolumeCapability{
		AccessType: &csipb.VolumeCapability_Block{Block: &csipb.VolumeCapability_BlockVolume{}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	mount := &csipb.VolumeCapability{
		AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
		AccessMode: &csipb.VolumeCapability_AccessMode{
			Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
	for _, tc := range []struct {
		protocol string
		cap      *csipb.VolumeCapability
		ok       bool
	}{
		{"nfs", block, false},
		{"smb", block, false},
		{"nfs", mount, true},
		{"smb", mount, true},
		{"iscsi", block, true},
		{"nvme", block, true},
		{"iscsi", mount, true},
	} {
		name := tc.protocol
		if tc.cap.GetBlock() != nil {
			name += "-block"
		} else {
			name += "-mount"
		}
		t.Run(name, func(t *testing.T) {
			err := requireSupportedCapabilities(tc.protocol, []*csipb.VolumeCapability{tc.cap})
			switch {
			case tc.ok && err != nil:
				t.Fatalf("%s must be accepted: %v", name, err)
			case !tc.ok && status.Code(err) != codes.InvalidArgument:
				t.Fatalf("code = %v, want InvalidArgument (err %v)", status.Code(err), err)
			}
		})
	}
}

// TestRequiredTopologyFollowsTheCapabilityFilesystem.
//
// Kubernetes conveys the filesystem through the reserved
// csi.storage.k8s.io/fstype StorageClass parameter, which the external
// provisioner CONSUMES: it strips the reserved keys and puts the value in the
// volume capability's mount fs_type. So a driver reading only params never sees
// it.
//
// requiredTopology read only params["fsType"], a driver-specific spelling.
// Measured on a real cluster: a class setting csi.storage.k8s.io/fstype=xfs
// produced a volume formatted xfs whose PV required the ext4 topology label. On
// a cluster where some nodes lack xfsprogs the scheduler would place the pod on
// a node that cannot format or grow it — the exact failure this function's own
// comment says it exists to prevent.
func TestRequiredTopologyFollowsTheCapabilityFilesystem(t *testing.T) {
	mount := func(fs string) []*csipb.VolumeCapability {
		return []*csipb.VolumeCapability{{
			AccessType: &csipb.VolumeCapability_Mount{
				Mount: &csipb.VolumeCapability_MountVolume{FsType: fs}},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		}}
	}
	key := func(c node.Capability) string { return node.TopologyKey(c) }

	for _, tc := range []struct {
		name    string
		params  map[string]string
		caps    []*csipb.VolumeCapability
		wantKey string
	}{
		{
			name:    "capability names xfs, params say nothing",
			params:  map[string]string{},
			caps:    mount("xfs"),
			wantKey: key(node.CapXFS),
		},
		{
			name:    "capability names ext4",
			params:  map[string]string{},
			caps:    mount("ext4"),
			wantKey: key(node.CapExt4),
		},
		{
			name:    "capability wins over the driver-specific parameter",
			params:  map[string]string{"fsType": "ext4"},
			caps:    mount("xfs"),
			wantKey: key(node.CapXFS),
		},
		{
			name:    "no capability filesystem falls back to the parameter",
			params:  map[string]string{"fsType": "xfs"},
			caps:    mount(""),
			wantKey: key(node.CapXFS),
		},
		{
			name:    "block volumes need no filesystem tooling at all",
			params:  map[string]string{},
			caps:    mount(""),
			wantKey: key(node.CapExt4), // the historical default for a zvol
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := requiredTopology("nas1", "iscsi", tc.params, tc.caps)
			if len(got) != 1 {
				t.Fatalf("want one topology, got %d", len(got))
			}
			if _, ok := got[0].GetSegments()[tc.wantKey]; !ok {
				t.Fatalf("topology %v does not require %q; the scheduler could place "+
					"this volume on a node that cannot format it",
					got[0].GetSegments(), tc.wantKey)
			}
		})
	}
}

// TestEveryFilesystemTopologyKeyIsOnePublishedByNodes is the invariant that
// makes a filesystem requirement satisfiable at all.
//
// requiredTopology used to spell the requirement with the filesystem's own name,
// so a StorageClass asking for ext3 — which this driver fully supports, and
// which the node formats with mkfs.ext3 — produced a PV requiring
// csi.truenas.watteel.com/ext3. No node publishes that key: nodes publish
// capabilities, and ext2/ext3/ext4 all live under the ext4 capability. The PV
// bound, and then every pod using it stayed Pending with the scheduler naming a
// label rather than the filesystem.
func TestEveryFilesystemTopologyKeyIsOnePublishedByNodes(t *testing.T) {
	// Every key a node publishes from its own preflight. The backend key is
	// published separately, per configured backend, so this test asks for no
	// backend and every remaining key must come from this set.
	published := map[string]bool{}
	for _, c := range node.CapabilityOrder() {
		published[node.TopologyKey(c)] = true
	}

	for _, fs := range []string{"ext2", "ext3", "ext4", "xfs"} {
		t.Run(fs, func(t *testing.T) {
			caps := []*csipb.VolumeCapability{{
				AccessType: &csipb.VolumeCapability_Mount{
					Mount: &csipb.VolumeCapability_MountVolume{FsType: fs}},
			}}
			if err := requireSupportedCapabilities("iscsi", caps); err != nil {
				t.Fatalf("%s is a filesystem this driver supports, but CreateVolume refuses it: %v", fs, err)
			}
			for key := range requiredTopology("", "iscsi", nil, caps)[0].GetSegments() {
				if !published[key] {
					t.Errorf("a %s volume requires topology key %q, which no node publishes "+
						"(nodes publish %v) — every pod using it would stay Pending forever",
						fs, key, published)
				}
			}
		})
	}
}

// TestUnsupportedFilesystemIsRefusedAtCreateVolume keeps the refusal where the
// user can read it. Encoding an unmakeable filesystem in the topology instead
// binds a PV that no node can ever satisfy, and the only diagnostic is a
// scheduler message about a label nobody has heard of.
func TestUnsupportedFilesystemIsRefusedAtCreateVolume(t *testing.T) {
	caps := []*csipb.VolumeCapability{{
		AccessType: &csipb.VolumeCapability_Mount{
			Mount: &csipb.VolumeCapability_MountVolume{FsType: "btrfs"}},
	}}
	err := requireSupportedCapabilities("iscsi", caps)
	if err == nil {
		t.Fatal("CreateVolume accepted btrfs, which the node cannot make; the PVC would bind " +
			"and the pod would then be unschedulable with no mention of the filesystem")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("want InvalidArgument for an unsupported filesystem, got %v", got)
	}
	if !strings.Contains(err.Error(), "btrfs") {
		t.Errorf("the message must name the filesystem the user asked for; got %q", err)
	}
}
