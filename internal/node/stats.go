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
	"syscall"
)

// ErrVolumePathNotFound is returned when the path the CO asked about does not exist
// on this node. The gRPC adapter maps it to codes.NotFound, which is the code the
// CO uses to decide the volume is gone rather than the node broken.
var ErrVolumePathNotFound = errors.New("volume path is not present on this node")

// Stats reports byte and inode usage for a staged or published volume path.
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

	return StatsResponse{Usage: []Usage{
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
