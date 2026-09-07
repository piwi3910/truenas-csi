package node

// NFS staging. An NFS volume needs nothing but a mount: the appliance owns the
// filesystem, its permissions and its quota, so the node never formats, never grows
// and never resolves a device.

import (
	"context"
	"fmt"
	"os"
)

// stageNFS mounts the export at the staging path, once. It is safe to call
// repeatedly: when the host already reports the staging path as a mount point the
// call returns success without touching the host.
func (n *Node) stageNFS(ctx context.Context, req StageRequest) error {
	if err := n.pre.Require(CapNFS); err != nil {
		return err
	}
	server := req.PublishContext[KeyServer]
	share := req.PublishContext[KeyShare]
	if share == "" {
		share = req.PublishContext[KeyExport]
	}
	if server == "" || share == "" {
		return fmt.Errorf("%w: nfs volume needs %q and %q in the publish context", ErrInvalidRequest, KeyServer, KeyShare)
	}
	if req.StagingPath == "" {
		return fmt.Errorf("%w: no staging path", ErrInvalidRequest)
	}

	mounted, err := n.isMounted(req.StagingPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("create staging path %s: %w", req.StagingPath, err)
	}

	opts := []string{"vers=" + nfsVersion(req.PublishContext)}
	opts = append(opts, req.VolumeCapability.MountFlags...)
	if req.VolumeCapability.Readonly {
		opts = append(opts, "ro")
	}
	return n.mount(ctx, "nfs", server+":"+share, req.StagingPath, opts)
}

// nfsVersion reads the requested NFS version, defaulting to v4. The default is not
// arbitrary: mounting v3 pulls rpc-statd onto the host for lock recovery, and a node
// without it mounts but then hangs on the first lock, whereas v4 carries locking in
// the protocol itself.
func nfsVersion(pc map[string]string) string {
	if v := pc[KeyNFSVersion]; v != "" {
		return v
	}
	return DefaultNFSVersion
}
