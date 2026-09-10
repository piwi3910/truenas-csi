package node

// Volume statistics.
//
// The numbers here are what the kubelet turns into kubelet_volume_stats_* metrics
// and what drives PVC usage alerts, so they must describe the volume rather than
// its host. For NFS that only holds because every volume gets a refquota at
// provisioning time — without one, statfs on the mount reports the whole pool and
// no usage alert can ever fire.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrVolumePathNotFound is returned when the path the CO asked about does not exist
// on this node. The gRPC adapter maps it to codes.NotFound, which is the code the
// CO uses to decide the volume is gone rather than the node broken.
var ErrVolumePathNotFound = errors.New("volume path is not present on this node")

// Stats reports byte and inode usage for a staged or published volume path,
// together with the volume's health condition as the monitor last observed it.
func (n *Node) Stats(_ context.Context, req StatsRequest) (StatsResponse, error) {
	if req.VolumePath == "" {
		return StatsResponse{}, fmt.Errorf("%w: no volume path", ErrInvalidRequest)
	}
	if _, err := os.Stat(req.VolumePath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return StatsResponse{}, fmt.Errorf("%w: %s", ErrVolumePathNotFound, req.VolumePath)
		}
		return StatsResponse{}, fmt.Errorf("stat %s: %w", req.VolumePath, err)
	}

	abnormalOf := func() (bool, string) {
		if n.health == nil {
			return false, ""
		}
		return n.health.Condition(req.VolumeID)
	}

	// A raw block volume has no filesystem, and statfs does not fail on its
	// path — it answers about the filesystem CONTAINING the device node, which
	// is devtmpfs. That is how a 1 GiB block PVC came to report 16.4 GB of
	// capacity on a real cluster: half the node's RAM, the same number for
	// every block volume on it, driving every kubelet_volume_stats_* metric and
	// every usage alert built on them.
	//
	// The device's own size is the honest answer, and it is the only one: how
	// much of a raw block device is "used" is a question only its consumer can
	// answer, and CSI says to report the capacity alone.
	if size, ok := n.blockVolumeSize(req.VolumePath); ok {
		abnormal, message := abnormalOf()
		return StatsResponse{Abnormal: abnormal, Message: message, Usage: []Usage{
			{Unit: UnitBytes, Total: size},
		}}, nil
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(req.VolumePath, &st); err != nil {
		return StatsResponse{}, fmt.Errorf("statfs %s: %w", req.VolumePath, err)
	}

	// Available is what an unprivileged process may still write, which is the
	// number a pod cares about; Total minus Available is therefore the usage a
	// pod can perceive, and it is not the same as Blocks-Bfree on a filesystem
	// with reserved blocks.
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	available := st.Bavail * bsize
	free := st.Bfree * bsize
	used := total - free

	inodesTotal := st.Files
	inodesFree := st.Ffree
	inodesUsed := inodesTotal - inodesFree

	abnormal, message := abnormalOf()

	return StatsResponse{Abnormal: abnormal, Message: message, Usage: []Usage{
		{
			Unit:      UnitBytes,
			Total:     int64(total),
			Used:      int64(used),
			Available: int64(available),
		},
		{
			Unit:      UnitInodes,
			Total:     int64(inodesTotal),
			Used:      int64(inodesUsed),
			Available: int64(inodesFree),
		},
	}}, nil
}

// blockVolumeSize is the size in bytes of the device published at path, and
// whether path is a block device at all.
//
// The size comes from sysfs rather than an ioctl or a helper binary: the node
// already reads this tree to map a device number to a name, the value is in
// 512-byte sectors by kernel convention regardless of the device's own logical
// block size, and it needs no privilege beyond the read the plugin already has.
func (n *Node) blockVolumeSize(path string) (int64, bool) {
	dev, ok := blockDeviceNumber(path)
	if !ok {
		return 0, false
	}
	name := n.deviceNameForNumber(dev)
	if name == "" {
		return 0, false
	}
	b, err := os.ReadFile(filepath.Join(n.hostRoot(), "sys", "block", name, "size"))
	if err != nil {
		// A partition, whose size lives under its parent disk's directory.
		b, err = os.ReadFile(filepath.Join(n.hostRoot(), "sys", "dev", "block",
			strconv.FormatUint(unixMajor(dev), 10)+":"+strconv.FormatUint(unixMinor(dev), 10),
			"size"))
		if err != nil {
			return 0, false
		}
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || sectors <= 0 {
		return 0, false
	}
	return sectors * 512, true
}
