package node

import (
	"os"
	"path/filepath"
	"strings"
)

// iscsiTargetInUse reports whether any device belonging to this portal and
// target is still mounted on the host.
//
// It exists because this driver maps every volume as a LUN on ONE SHARED
// target (see internal/backend/iscsi), so the session is shared by every iSCSI
// volume staged on the node. Logging out is therefore not a per-volume
// operation at all: it tears down the data path of every sibling volume that is
// still mounted. Verified on real hardware — two iSCSI PVCs on one node, and
// deleting the first pod left the second answering "Input/output error" on a
// mount whose device had gone away.
//
// The node's own staging mount is already gone by the time this runs, so
// anything still found here belongs to another volume.
func (n *Node) iscsiTargetInUse(portal, iqn string) (bool, error) {
	devices, err := n.devicesOfTarget(portal, iqn)
	if err != nil {
		return false, err
	}
	if len(devices) == 0 {
		return false, nil
	}
	entries, err := n.mounts()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		for _, name := range n.deviceNamesOf(e.source) {
			if devices[name] {
				return true, nil
			}
		}
	}
	return false, nil
}

// devicesOfTarget returns the kernel names (sdc, sdd, …) of every LUN the host
// has for one portal and target, read from /dev/disk/by-path — which names each
// link "ip-<portal>-iscsi-<iqn>-lun-<n>" and points it at the disk.
func (n *Node) devicesOfTarget(portal, iqn string) (map[string]bool, error) {
	dir := filepath.Join(n.hostRoot(), "dev", "disk", "by-path")
	links, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No by-path directory at all: the host layout is not one this can
			// reason about, so it must not conclude "nothing is in use".
			return nil, err
		}
		return nil, err
	}
	prefix := "ip-" + portal + "-iscsi-" + iqn + "-lun-"
	out := map[string]bool{}
	for _, l := range links {
		if !strings.HasPrefix(l.Name(), prefix) {
			continue
		}
		dest, err := os.Readlink(filepath.Join(dir, l.Name()))
		if err != nil {
			continue
		}
		out[filepath.Base(dest)] = true
	}
	return out, nil
}

// deviceNamesOf maps one mount source to the kernel device names behind it.
//
// A plain disk is itself. A multipath map is its members, because with
// multipath the mounted source is the mapper device while the target's LUNs are
// the paths underneath it — so comparing the mapper name alone would find no
// match and log the session out from under a live multipath volume.
func (n *Node) deviceNamesOf(source string) []string {
	if !strings.HasPrefix(source, "/dev/") {
		return nil
	}
	name := filepath.Base(source)
	if strings.HasPrefix(source, "/dev/mapper/") {
		if dest, err := os.Readlink(filepath.Join(n.hostRoot(), "dev", "mapper", name)); err == nil {
			name = filepath.Base(dest)
		}
	}
	slaves, err := os.ReadDir(filepath.Join(n.hostRoot(), "sys", "block", name, "slaves"))
	if err != nil || len(slaves) == 0 {
		return []string{name}
	}
	out := make([]string, 0, len(slaves)+1)
	out = append(out, name)
	for _, s := range slaves {
		out = append(out, s.Name())
	}
	return out
}
