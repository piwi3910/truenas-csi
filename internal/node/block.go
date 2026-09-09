package node

// Raw block publishing — volumeMode: Block.
//
// A block volume never sees mkfs and never sees a filesystem mount. The pod asks
// for the device itself, so the node bind-mounts the block device onto a *file*
// target that the kubelet created the parent directory for. Binding a device onto
// a directory fails, which is why the target is created with os.Create rather than
// os.MkdirAll.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// publishBlock binds the resolved device onto the pod's device file target.
func (n *Node) publishBlock(ctx context.Context, req PublishRequest) error {
	// By protocol, not by NAA. This used to demand a NAA unconditionally, which
	// only iSCSI has: an NVMe namespace is found by its subsystem serial. Every
	// raw-block NVMe volume therefore failed NodePublishVolume with
	// `block volume needs "naa" in the publish context`, retried for ever, and
	// its pod sat in ContainerCreating -- while the same volume as a filesystem
	// worked. Found by the upstream conformance suite's block patterns.
	device, err := n.resolveBlockDevice(ctx, req.PublishContext, protocolOf(req.PublishContext))
	if err != nil {
		return err
	}

	mounted, err := n.isMounted(req.TargetPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := makeDeviceTarget(req.TargetPath); err != nil {
		return err
	}
	return n.bindMount(ctx, device, req.TargetPath, req.Readonly || req.VolumeCapability.Readonly)
}

// makeDeviceTarget creates the empty file a device bind mount needs, and the
// directory holding it. An existing file is left alone: a republish of the same
// volume must be a no-op, not a truncate.
func makeDeviceTarget(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create block target directory for %s: %w", path, err)
	}
	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		return fmt.Errorf("block target %s is a directory; a device cannot be bound onto one", path)
	case err == nil:
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create block target %s: %w", path, err)
	}
	return f.Close()
}
