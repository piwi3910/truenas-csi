package node

// Online expansion.
//
// The sequence here was verified against real hardware: after the controller grows
// the zvol, a session rescan scoped to our target and portal makes the kernel see
// the new size, and the filesystem is then grown in place. A file written before
// the resize was still readable after it.
//
//	iscsiadm -m node -T <iqn> -p <portal> -R
//	blockdev --getsize64 <device>      # until it changes
//	resize2fs <device>  |  xfs_growfs <mount point>
//
// What this must never do: unmount, re-login, or restart anything. The pod keeps
// running throughout. And NFS needs nothing at all — the appliance's refquota
// change is the entire expansion, visible to the client on its next statfs.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// Expand grows the node-side view of an already-grown volume.
func (n *Node) Expand(ctx context.Context, req ExpandRequest) (ExpandResponse, error) {
	ctx = obs.WithVolume(ctx, req.VolumeID)
	resp := ExpandResponse{CapacityBytes: req.CapacityBytes}

	var (
		device string
		rescan func() error
	)
	switch protocolOf(req.PublishContext) {
	case ProtocolNFS, ProtocolSMB:
		// Nothing to do on the node. Both file protocols serve a dataset whose
		// refquota is the size the pod sees, and it changed on the appliance.
		return resp, nil
	case ProtocolISCSI:
		portal, iqn, naa := req.PublishContext[KeyPortal], req.PublishContext[KeyIQN], req.PublishContext[KeyNAA]
		if portal == "" || iqn == "" || naa == "" {
			return resp, fmt.Errorf("%w: iscsi expansion needs %q, %q and %q in the publish context",
				ErrInvalidRequest, KeyPortal, KeyIQN, KeyNAA)
		}
		d, err := n.deviceFor(ctx, naa)
		if err != nil {
			return resp, err
		}
		device = d
		rescan = func() error { return iscsiRescan(ctx, n.exec, portal, iqn) }
	case ProtocolNVMe:
		serial := req.PublishContext[KeySerial]
		if serial == "" {
			return resp, fmt.Errorf("%w: nvme expansion needs %q in the publish context",
				ErrInvalidRequest, KeySerial)
		}
		d, err := resolveNVMeDevice(ctx, n.exec, n.hostRoot(), serial)
		if err != nil {
			return resp, err
		}
		device = d
		// The namespace rescan is scoped to the device resolved from our own
		// subsystem serial, so no other controller on the node is disturbed.
		rescan = func() error { return nvmeRescan(ctx, n.exec, device) }
	default:
		return resp, fmt.Errorf("%w: publish context names no supported protocol", ErrInvalidRequest)
	}

	before, haveBefore := n.deviceSize(ctx, device)
	if err := rescan(); err != nil {
		return resp, err
	}
	after := n.waitForGrowth(ctx, device, before, haveBefore)
	if after > 0 {
		resp.CapacityBytes = after
	}

	if req.VolumeCapability.Block {
		// A raw block volume has no filesystem to grow: the pod sees the larger
		// device the moment the rescan lands.
		return resp, nil
	}

	fsType := fsTypeOf(req.VolumeCapability, req.PublishContext)
	if err := n.requireFS(fsType); err != nil {
		return resp, err
	}
	target := req.VolumePath
	if target == "" {
		target = req.StagingPath
	}
	if err := n.growFilesystem(ctx, fsType, device, target); err != nil {
		return resp, err
	}
	return resp, nil
}

// growFilesystem grows a mounted filesystem in place. The two tools disagree about
// what they take: resize2fs grows a mounted ext4 given its device, xfs_growfs takes
// the mount point. Getting this backwards fails at the tool, not at the mount.
func (n *Node) growFilesystem(ctx context.Context, fsType, device, mountPoint string) error {
	switch fsType {
	case "ext2", "ext3", "ext4":
		if _, err := n.exec.Run(ctx, "resize2fs", device); err != nil {
			return fmt.Errorf("resize2fs %s: %w", device, err)
		}
		return nil
	case "xfs":
		if mountPoint == "" {
			return fmt.Errorf("%w: growing xfs needs the mount point", ErrInvalidRequest)
		}
		if _, err := n.exec.Run(ctx, "xfs_growfs", mountPoint); err != nil {
			return fmt.Errorf("xfs_growfs %s: %w", mountPoint, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: cannot grow filesystem %q", ErrInvalidRequest, fsType)
	}
}

// deviceSize reads the device's size in bytes, reporting whether it could.
func (n *Node) deviceSize(ctx context.Context, device string) (int64, bool) {
	out, err := n.exec.Run(ctx, "blockdev", "--getsize64", device)
	if err != nil {
		return 0, false
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || size <= 0 {
		return 0, false
	}
	return size, true
}

// waitForGrowth waits for the kernel to publish the new device size after a rescan,
// which is not instantaneous, and returns the size it settled on. A device whose
// size never changes is not an error: the volume may already have been at the new
// size from an earlier, partially completed expansion, and growing the filesystem
// is idempotent anyway.
func (n *Node) waitForGrowth(ctx context.Context, device string, before int64, haveBefore bool) int64 {
	deadline := time.Now().Add(expandWaitTimeout)
	last := before
	for {
		size, ok := n.deviceSize(ctx, device)
		if ok {
			last = size
			if !haveBefore || size != before {
				return size
			}
		}
		if !time.Now().Before(deadline) {
			obs.Logger(ctx).Warn("device size did not change after rescan; growing the filesystem anyway",
				"device", device, "size_bytes", last)
			return last
		}
		time.Sleep(expandPollInterval)
	}
}
