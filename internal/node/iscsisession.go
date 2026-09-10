package node

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
		for _, name := range n.deviceNamesOfMount(e) {
			if devices[name] {
				return true, nil
			}
		}
	}
	return false, nil
}

// deviceNamesOfMount maps one mount table entry to the kernel device names it
// keeps busy.
//
// A filesystem mount names its device in the SOURCE field, and that is what
// this used to read — the whole of it. A raw block volume does not: the kubelet
// bind-mounts the device node onto a file, and the kernel records that mount
// with the source "udev" and the fstype "devtmpfs", never mentioning the disk.
//
// So every volumeMode: Block volume was invisible to the in-use check, and a
// node whose only iSCSI volumes were raw block ones logged out of the SHARED
// session the moment any one of them was unstaged. Measured on the cluster:
// two block PVCs on one node, deleting the first left the second's pod running
// with "can't open /dev/xvda: No such device or address", its data path gone.
//
// The device is therefore taken from what the mount POINTS AT when the source
// does not name one: a block bind mount's target is itself a device node, and
// its rdev names the disk no matter what string the source field carries.
func (n *Node) deviceNamesOfMount(e mountEntry) []string {
	if strings.HasPrefix(e.source, "/dev/") {
		return n.deviceNamesOf(e.source)
	}
	// Only the device filesystems are followed. Stat'ing an arbitrary mount
	// point would put a syscall on a path that may be a hung NFS mount, which
	// is exactly the hang this driver takes such care to stay out of.
	switch e.fsType {
	case "devtmpfs", "tmpfs", "udev":
	default:
		return nil
	}
	if name := n.blockDeviceAt(e.target); name != "" {
		return n.deviceNamesOf("/dev/" + name)
	}
	return nil
}

// blockDeviceAt names the disk behind a path that IS a block device node, or ""
// when the path is not one.
func (n *Node) blockDeviceAt(path string) string {
	dev, ok := blockDeviceNumber(filepath.Join(n.hostRoot(), strings.TrimPrefix(path, n.hostRoot())))
	if !ok {
		return ""
	}
	return n.deviceNameForNumber(dev)
}

// deviceNameForNumber turns a device number into the kernel name for that disk.
//
// /sys/dev/block/<major>:<minor> is a symlink into the device's own sysfs
// directory, whose base name is the kernel name — the same sdX the by-path
// links resolve to.
func (n *Node) deviceNameForNumber(dev uint64) string {
	link := filepath.Join(n.hostRoot(), "sys", "dev", "block",
		strconv.FormatUint(unixMajor(dev), 10)+":"+strconv.FormatUint(unixMinor(dev), 10))
	dest, err := os.Readlink(link)
	if err != nil {
		return ""
	}
	return filepath.Base(dest)
}

// blockDeviceNumber is the device number of a path that is a block device node,
// and whether it is one.
//
// It is a variable so a test can describe a block device node without the
// privilege to create one: mknod needs root, and a check that silently skips
// wherever the tests actually run is not a check.
var blockDeviceNumber = func(path string) (uint64, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode()&os.ModeDevice == 0 || fi.Mode()&os.ModeCharDevice != 0 {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Rdev), true //nolint:unconvert,gosec // Rdev is int32 on darwin, uint64 on linux
}

// unixMajor and unixMinor decode a Linux dev_t. They are spelled out rather
// than taken from a build-tagged package because this file has to compile on
// the developer's machine as well as on the node.
func unixMajor(dev uint64) uint64 {
	return ((dev >> 8) & 0xfff) | ((dev >> 32) & ^uint64(0xfff))
}

func unixMinor(dev uint64) uint64 {
	return (dev & 0xff) | ((dev >> 12) & ^uint64(0xff))
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
