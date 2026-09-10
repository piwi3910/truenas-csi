package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// staleHost builds a host whose kernel holds devices for LUNs of one target.
// Each entry names the device and the wwid the kernel reports for it.
func staleHost(t *testing.T, portal, iqn string, luns map[int]struct{ dev, wwid string }, mounted []string) string {
	t.Helper()
	root := hostRoot(t)
	byPath := filepath.Join(root, "dev", "disk", "by-path")
	byID := filepath.Join(root, "dev", "disk", "by-id")
	for _, d := range []string{byPath, byID, filepath.Join(root, "proc")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for lun, e := range luns {
		devDir := filepath.Join(root, "sys", "block", e.dev, "device")
		if err := os.MkdirAll(devDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(devDir, "wwid"), []byte("naa."+e.wwid+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The delete attribute is the kernel's interface for forgetting a disk.
		if err := os.WriteFile(filepath.Join(devDir, "delete"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(byPath, "ip-"+portal+"-iscsi-"+iqn+"-lun-"+itoa(lun))
		if err := os.Symlink("../../"+e.dev, link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../"+e.dev, filepath.Join(byID, "scsi-3"+e.wwid)); err != nil {
			t.Fatal(err)
		}
	}
	table := ""
	for _, d := range mounted {
		table += "/dev/" + d + " /var/lib/kubelet/x ext4 rw 0 0\n"
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	out := ""
	for i > 0 {
		out = string(rune('0'+i%10)) + out
		i /= 10
	}
	return out
}

// deleted reports whether the kernel was asked to forget a device.
func deleted(t *testing.T, root, dev string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "sys", "block", dev, "device", "delete"))
	if err != nil {
		t.Fatalf("read delete attribute for %s: %v", dev, err)
	}
	return string(b) == "1"
}

const (
	stalePortal = "192.168.10.253:3260"
	staleIQN    = "iqn.2005-10.org.freenas.ctl:csi-pool0-k8s"
)

// TestDropStaleTargetDevicesRemovesOnlyRecycledLUNs is the safety envelope. The
// whole point of removing a device is that it is safe to remove: it is on our
// target, it is not the volume being staged, and nothing has it mounted.
func TestDropStaleTargetDevicesRemovesOnlyRecycledLUNs(t *testing.T) {
	const want = "6589cfc0000002be12ecd2689a164171"
	root := staleHost(t, stalePortal, staleIQN, map[int]struct{ dev, wwid string }{
		0: {"sdb", "6589cfc0000005997b02e9e47100d1c1"}, // stale: a deleted volume
		1: {"sdc", "6589cfc000000abef0b7def785da117c"}, // live: a sibling is using it
		2: {"sdd", want},                               // the volume being staged
	}, []string{"sdc"})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	// LUN 0 is the one this volume was mapped to, and the one a deleted volume
	// still occupies.
	if got := n.dropStaleTargetDevices(context.Background(), stalePortal, staleIQN, want, "0"); got != 1 {
		t.Fatalf("removed %d devices, want exactly 1", got)
	}
	if !deleted(t, root, "sdb") {
		t.Error("the stale device was left in place; the LUN it occupies has been reassigned " +
			"and the volume now living there can never be staged on this node")
	}
	if deleted(t, root, "sdc") {
		t.Error("removed a device a sibling volume has mounted — that volume's data path is now gone")
	}
	if deleted(t, root, "sdd") {
		t.Error("removed the device for the volume being staged")
	}

	// The same host, asked about a LUN whose device is mounted by a sibling and
	// about the LUN the wanted volume already occupies: neither may be touched.
	for _, lun := range []string{"1", "2"} {
		if got := n.dropStaleTargetDevices(context.Background(), stalePortal, staleIQN, want, lun); got != 0 {
			t.Errorf("removed %d devices at lun %s; a mounted sibling and the volume being "+
				"staged are both off limits", got, lun)
		}
	}
}

// TestDropStaleTargetDevicesNeedsALUN keeps the recovery from widening into a
// sweep. Without the LUN there is no way to name the one device in the way, and
// removing every idle device of the target would race a sibling's stage.
func TestDropStaleTargetDevicesNeedsALUN(t *testing.T) {
	root := staleHost(t, stalePortal, staleIQN, map[int]struct{ dev, wwid string }{
		0: {"sdb", "6589cfc0000005997b02e9e47100d1c1"},
	}, nil)
	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	if got := n.dropStaleTargetDevices(context.Background(), stalePortal, staleIQN, "abc", ""); got != 0 {
		t.Fatalf("removed %d devices with no LUN to go on; want 0", got)
	}
}

// TestDropStaleTargetDevicesLeavesEverythingAloneWhenItCannotTell keeps "could
// not ask" from being read as "stale". A device whose identity is unreadable
// might be anything, including a live volume.
func TestDropStaleTargetDevicesLeavesEverythingAloneWhenItCannotTell(t *testing.T) {
	root := staleHost(t, stalePortal, staleIQN, map[int]struct{ dev, wwid string }{
		0: {"sdb", "6589cfc0000005997b02e9e47100d1c1"},
	}, nil)
	if err := os.Remove(filepath.Join(root, "sys", "block", "sdb", "device", "wwid")); err != nil {
		t.Fatal(err)
	}
	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	if got := n.dropStaleTargetDevices(context.Background(), stalePortal, staleIQN, "abc", "0"); got != 0 {
		t.Fatalf("removed %d devices whose identity could not be read; want 0", got)
	}
}

// TestDropStaleTargetDevicesIgnoresOtherTargets is the Longhorn guard. This node
// runs another iSCSI initiator against other targets, and a device of theirs
// removed by us is their outage.
func TestDropStaleTargetDevicesIgnoresOtherTargets(t *testing.T) {
	root := staleHost(t, stalePortal, "iqn.2019-10.io.longhorn:pvc-other",
		map[int]struct{ dev, wwid string }{0: {"sdb", "60000000000000000e00000000010001"}}, nil)
	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	if got := n.dropStaleTargetDevices(context.Background(), stalePortal, staleIQN, "abc", "0"); got != 0 {
		t.Fatalf("removed %d devices belonging to another target; want 0", got)
	}
	if deleted(t, root, "sdb") {
		t.Error("removed another initiator's device")
	}
}

// TestUnstageRemovesThisVolumesDevice is the root-cause half: without it, every
// unstage leaves a device behind, and the LUN ids this driver recycles guarantee
// that some later volume lands on one of them.
func TestUnstageRemovesThisVolumesDevice(t *testing.T) {
	const naa = "6589cfc0000005997b02e9e47100d1c1"
	root := staleHost(t, stalePortal, staleIQN,
		map[int]struct{ dev, wwid string }{0: {"sdb", naa}}, nil)

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	n.dropDeviceForNAA(context.Background(), "0x"+naa)

	if !deleted(t, root, "sdb") {
		t.Fatal("unstage left this volume's device in the kernel; when the appliance gives " +
			"its LUN id to the next volume, that volume cannot be staged on this node")
	}
}
