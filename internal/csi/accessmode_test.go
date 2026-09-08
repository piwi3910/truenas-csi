package csi

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

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
