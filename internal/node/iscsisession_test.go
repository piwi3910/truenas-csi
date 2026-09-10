package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// iscsiRoot builds a host root describing one iSCSI target's devices and the
// host mount table, the way /dev/disk/by-path and /proc/mounts really look.
func sessionRoot(t *testing.T, byPath map[string]string, mounts []string, slaves map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "dev", "disk", "by-path")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, dev := range byPath {
		if err := os.Symlink("../../"+dev, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	for dm, members := range slaves {
		sd := filepath.Join(root, "sys", "block", dm, "slaves")
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, m := range members {
			if err := os.WriteFile(filepath.Join(sd, m), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		md := filepath.Join(root, "dev", "mapper")
		if err := os.MkdirAll(md, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../"+dm, filepath.Join(md, "mpath-"+dm)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"),
		[]byte(strings.Join(mounts, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestUnstageKeepsASharedSessionAliveForSiblingVolumes is the multi-volume bug.
//
// This driver maps every volume as a LUN on ONE shared target, so the iSCSI
// session is shared by every iSCSI volume staged on the node. Unstage logged
// out of it unconditionally, which tore down the session serving the volumes
// that were still mounted. Reproduced on real hardware: two iSCSI PVCs on one
// node, deleting the first pod left the second answering "Input/output error"
// on a mount whose device had gone away.
func TestUnstageKeepsASharedSessionAliveForSiblingVolumes(t *testing.T) {
	const portal, iqn = "192.168.10.253:3260", "iqn.2005-10.org.freenas.ctl:csi"
	byPath := map[string]string{
		"ip-" + portal + "-iscsi-" + iqn + "-lun-0": "sdc",
		"ip-" + portal + "-iscsi-" + iqn + "-lun-1": "sdd",
	}
	staged := "/dev/sdd /var/lib/kubelet/plugins/kubernetes.io/csi/csi.truenas.watteel.com/x/globalmount ext4 rw 0 0"
	other := "/dev/longhorn/pvc-1 /var/lib/kubelet/plugins/kubernetes.io/csi/driver.longhorn.io/y/globalmount ext4 rw 0 0"

	for _, tc := range []struct {
		name   string
		mounts []string
		logout bool
	}{
		{name: "a sibling volume is still staged", mounts: []string{staged, other}},
		{name: "nothing of ours is left", mounts: []string{other}, logout: true},
		{name: "no mounts at all", mounts: []string{}, logout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := sessionRoot(t, byPath, tc.mounts, nil)
			ex := &recordExec{}
			n := &Node{exec: ex, Root: root}
			req := UnstageRequest{VolumeID: "v", StagingPath: "/s",
				PublishContext: map[string]string{
					KeyProtocol: ProtocolISCSI, KeyPortal: portal, KeyIQN: iqn}}
			if err := n.unstageISCSI(context.Background(), req); err != nil {
				t.Fatalf("unstage: %v", err)
			}
			if got := ex.loggedOut(); got != tc.logout {
				t.Fatalf("logged out = %v, want %v — logging out of a SHARED session "+
					"while another volume is mounted on it kills that volume", got, tc.logout)
			}
		})
	}
}

// TestUnstageKeepsTheSessionWhenAMultipathSiblingIsMounted: with multipath the
// mounted source is the mapper device, and its members are the target's disks.
func TestUnstageKeepsTheSessionWhenAMultipathSiblingIsMounted(t *testing.T) {
	const portal, iqn = "192.168.10.253:3260", "iqn.2005-10.org.freenas.ctl:csi"
	byPath := map[string]string{"ip-" + portal + "-iscsi-" + iqn + "-lun-1": "sdd"}
	mounts := []string{"/dev/mapper/mpath-dm-0 /var/lib/kubelet/plugins/x/globalmount ext4 rw 0 0"}
	root := sessionRoot(t, byPath, mounts, map[string][]string{"dm-0": {"sdd"}})

	ex := &recordExec{}
	n := &Node{exec: ex, Root: root}
	req := UnstageRequest{VolumeID: "v", StagingPath: "/s",
		PublishContext: map[string]string{
			KeyProtocol: ProtocolISCSI, KeyPortal: portal, KeyIQN: iqn}}
	if err := n.unstageISCSI(context.Background(), req); err != nil {
		t.Fatalf("unstage: %v", err)
	}
	if ex.loggedOut() {
		t.Fatal("logged out while a multipath sibling still holds the target's disks")
	}
}

// recordExec records every command so a test can assert what ran.
type recordExec struct{ ran []string }

func (r *recordExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.ran = append(r.ran, name+" "+strings.Join(args, " "))
	return nil, nil
}

func (r *recordExec) loggedOut() bool {
	for _, c := range r.ran {
		if strings.Contains(c, "--logout") {
			return true
		}
	}
	return false
}

// TestARawBlockVolumeKeepsTheSharedSessionUp is the failure this whole file
// exists to prevent, in the one shape it did not cover.
//
// A filesystem mount names its device in the mount table's source field. A raw
// block volume does not: the kubelet bind-mounts the device node onto a file,
// and the kernel records that with the source "udev" and the fstype "devtmpfs",
// never naming the disk. Every volumeMode: Block volume was therefore invisible
// here, so a node whose only iSCSI volumes were block ones logged out of the
// SHARED session the moment any one of them was unstaged.
//
// Measured on the cluster: two block PVCs on one node, and deleting the first
// left the second's pod running with "can't open /dev/xvda: No such device or
// address".
func TestARawBlockVolumeKeepsTheSharedSessionUp(t *testing.T) {
	const (
		portal = "192.168.10.253:3260"
		iqn    = "iqn.2005-10.org.freenas.ctl:csi-pool0-k8s"
		major  = 8
		minor  = 48
	)
	root := t.TempDir()
	byPath := filepath.Join(root, "dev", "disk", "by-path")
	target := filepath.Join(root, "var", "lib", "kubelet", "plugins",
		"kubernetes.io", "csi", "volumeDevices", "publish", "pvc-blk", "uuid")
	for _, d := range []string{byPath, filepath.Join(root, "proc"),
		filepath.Join(root, "sys", "dev", "block"), filepath.Dir(target),
		filepath.Join(root, "sys", "block", "sdd")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../../../sdd", filepath.Join(root, "dev", "disk", "by-path",
		"ip-"+portal+"-iscsi-"+iqn+"-lun-0")); err != nil {
		t.Fatal(err)
	}
	// The kernel's major:minor to name mapping.
	if err := os.Symlink("../../devices/virtual/block/sdd",
		filepath.Join(root, "sys", "dev", "block", "8:48")); err != nil {
		t.Fatal(err)
	}
	// The published block device. mknod needs root, so the stat is stubbed
	// rather than skipping the test wherever it actually runs.
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := blockDeviceNumber
	t.Cleanup(func() { blockDeviceNumber = restore })
	blockDeviceNumber = func(path string) (uint64, bool) {
		if path != target {
			return 0, false
		}
		return mkdev(major, minor), true
	}
	// The mount table as the kernel really writes it for a block bind mount:
	// the source is "udev" and the disk is named nowhere.
	table := "udev " + strings.TrimPrefix(target, root) + " devtmpfs rw,nosuid,relatime 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	inUse, err := n.iscsiTargetInUse(portal, iqn)
	if err != nil {
		t.Fatal(err)
	}
	if !inUse {
		t.Fatal("the shared session was reported idle while a raw block volume is published " +
			"through it; unstaging any sibling would log out and take that volume's data " +
			"path down with it")
	}
}

func mkdev(major, minor uint64) uint64 {
	return ((major & 0xfff) << 8) | (minor & 0xff) | ((major &^ 0xfff) << 32) | ((minor &^ 0xff) << 12)
}
