package node

// This file defines the node plugin's service surface.
//
// IMPORTANT — the CSI protobuf package is deliberately NOT a dependency yet. The
// types below (StageRequest, PublishRequest, ExpandRequest, StatsRequest and their
// responses) are this package's own minimal stand-ins for the corresponding
// container-orchestration RPC messages, and the methods Stage, Unstage, Publish,
// Unpublish, Expand, Stats and GetInfo are plain Go methods rather than gRPC
// handlers. A later task adds a thin adapter that implements csi.NodeServer by
// translating csi.Node*Request into these structs and this package's sentinel
// errors into gRPC status codes:
//
//	ErrCapabilityUnavailable -> codes.FailedPrecondition
//	ErrDeviceNotFound        -> codes.Internal (after the resolve timeout)
//	ErrVolumePathNotFound    -> codes.NotFound
//	ErrInvalidRequest        -> codes.InvalidArgument
//
// Keeping the protobuf out of this package means the data path is testable with
// nothing but a fake Executor and a temporary directory, which is how every
// behaviour verified against real hardware is pinned here.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pwatteel/truenas-csi/internal/obs"
)

// MaxVolumesPerNode is the number of volumes this plugin advertises it can host.
// iSCSI sessions and NFS mounts are cheap, but the kubelet needs a finite number to
// schedule against.
const MaxVolumesPerNode = 128

// DefaultHostRoot is where the DaemonSet mounts the host's root filesystem. The node
// plugin reads the host mount table and the host's /dev/disk/by-id through it.
const DefaultHostRoot = "/host"

// Publish-context keys. The controller fills these in at ControllerPublishVolume (or
// at CreateVolume, for a driver without a controller publish step) and the CO hands
// them back verbatim on every node call. They are the node plugin's only channel of
// information about a volume, which is why this package needs no backend imports.
const (
	// KeyProtocol selects the data path: ProtocolNFS or ProtocolISCSI.
	KeyProtocol = "protocol"
	// KeyServer is the NFS server address.
	KeyServer = "server"
	// KeyShare is the exported NFS path. KeyExport is accepted as a synonym.
	KeyShare = "share"
	// KeyExport is a synonym for KeyShare.
	KeyExport = "export"
	// KeyNFSVersion selects the NFS protocol version; the default is DefaultNFSVersion.
	KeyNFSVersion = "nfsVersion"
	// KeyPortal is the iSCSI portal, host or host:port.
	KeyPortal = "portal"
	// KeyIQN is the iSCSI target name.
	KeyIQN = "iqn"
	// KeyNAA is the extent's NAA identifier, as returned by iscsi.extent.create,
	// in its "0x…" form.
	KeyNAA = "naa"
	// KeyCHAPUser and KeyCHAPSecret carry discovery/session CHAP credentials when
	// the target requires them.
	KeyCHAPUser   = "chapUser"
	KeyCHAPSecret = "chapSecret"
	// KeyFSType overrides the filesystem type when the volume capability does not
	// carry one.
	KeyFSType = "fsType"
)

// Protocol values for KeyProtocol.
const (
	ProtocolNFS   = "nfs"
	ProtocolISCSI = "iscsi"
)

// DefaultNFSVersion is the NFS version used when the publish context names none.
// v4 is the default deliberately: NFSv3 drags rpc-statd onto the host for locking,
// v4 does not, and the appliance serves both.
const DefaultNFSVersion = "4"

// ErrInvalidRequest is the sentinel for a request the node cannot interpret — a
// missing publish-context key, an empty path. The gRPC adapter maps it to
// codes.InvalidArgument.
var ErrInvalidRequest = errors.New("invalid node request")

// VolumeCapability is this package's stand-in for csi.VolumeCapability. Block
// distinguishes volumeMode: Block from a mounted filesystem, which changes almost
// everything the node does: no mkfs, no filesystem mount, no online grow.
type VolumeCapability struct {
	// Block is true for a raw block volume.
	Block bool
	// FsType is the filesystem to create and mount for a non-block volume.
	FsType string
	// MountFlags are extra -o options.
	MountFlags []string
	// Readonly requests a read-only mount.
	Readonly bool
}

// StageRequest is NodeStageVolume's payload.
type StageRequest struct {
	VolumeID         string
	StagingPath      string
	PublishContext   map[string]string
	VolumeCapability VolumeCapability
	// Secrets carries per-volume node-stage secrets; CHAP credentials are read
	// from here in preference to the publish context.
	Secrets map[string]string
}

// UnstageRequest is NodeUnstageVolume's payload.
type UnstageRequest struct {
	VolumeID       string
	StagingPath    string
	PublishContext map[string]string
}

// PublishRequest is NodePublishVolume's payload.
type PublishRequest struct {
	VolumeID         string
	StagingPath      string
	TargetPath       string
	PublishContext   map[string]string
	VolumeCapability VolumeCapability
	Readonly         bool
}

// UnpublishRequest is NodeUnpublishVolume's payload.
type UnpublishRequest struct {
	VolumeID   string
	TargetPath string
}

// ExpandRequest is NodeExpandVolume's payload.
type ExpandRequest struct {
	VolumeID         string
	VolumePath       string
	StagingPath      string
	PublishContext   map[string]string
	VolumeCapability VolumeCapability
	CapacityBytes    int64
}

// ExpandResponse is NodeExpandVolume's reply.
type ExpandResponse struct {
	CapacityBytes int64
}

// StatsRequest is NodeGetVolumeStats' payload.
type StatsRequest struct {
	VolumeID   string
	VolumePath string
}

// UsageUnit names what a Usage counts.
type UsageUnit string

const (
	// UnitBytes counts bytes.
	UnitBytes UsageUnit = "bytes"
	// UnitInodes counts inodes.
	UnitInodes UsageUnit = "inodes"
)

// Usage is one CSI VolumeUsage entry.
type Usage struct {
	Unit      UsageUnit
	Total     int64
	Used      int64
	Available int64
}

// StatsResponse is NodeGetVolumeStats' reply.
type StatsResponse struct {
	Usage []Usage
}

// NodeInfo is NodeGetInfo's reply.
type NodeInfo struct {
	NodeID             string
	MaxVolumesPerNode  int64
	AccessibleTopology map[string]string
}

// Executor runs one host binary and returns its combined output. Every mount,
// iscsiadm, mkfs and grow the node performs goes through it, so a test can observe
// the exact command line without a host.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// hostExec runs binaries in the host's mount namespace.
type hostExec struct {
	root string
}

// HostExec returns an Executor that runs host binaries in the host's mount
// namespace, entered through root — the path at which the DaemonSet mounts the
// host's / (DefaultHostRoot). Mounts must land in the host namespace, not in this
// container's, or the kubelet would never see them.
func HostExec(root string) Executor {
	if root == "" {
		root = DefaultHostRoot
	}
	return hostExec{root: root}
}

func (h hostExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return h.run(ctx, name, args...)
}

// HostModprobe loads a kernel module using the host's own modprobe.
func HostModprobe(root string) ModprobeFunc {
	e := hostExec{root: root}
	return func(ctx context.Context, module string) error {
		_, err := e.run(ctx, "/sbin/modprobe", module)
		return err
	}
}

// Node is the node plugin's data path. One instance lives for the lifetime of the
// process, which is what makes "warn once per node start" expressible.
type Node struct {
	// Root is the host filesystem root, DefaultHostRoot in production. Tests point
	// it at a temporary directory.
	Root string

	nodeID string
	pre    *Preflight
	exec   Executor

	// multipathWarn fires the single degradation warning per node start, not per
	// volume: a node without multipath-tools would otherwise log on every attach.
	multipathWarn sync.Once
}

// NewNode builds the node plugin for one node from its identity, the startup
// capability preflight and an Executor reaching the host.
func NewNode(nodeID string, p *Preflight, exec Executor) *Node {
	return &Node{
		Root:   DefaultHostRoot,
		nodeID: nodeID,
		pre:    p,
		exec:   exec,
	}
}

// GetInfo reports this node's identity, the capabilities the startup preflight
// found, and how many volumes it will host.
func (n *Node) GetInfo(_ context.Context) NodeInfo {
	return NodeInfo{
		NodeID:             n.nodeID,
		MaxVolumesPerNode:  MaxVolumesPerNode,
		AccessibleTopology: n.pre.TopologyLabels(),
	}
}

// Stage makes a volume usable on this node: an NFS mount, or an iSCSI login plus
// optional format and mount. It is idempotent — staging an already-staged path
// issues nothing and succeeds.
func (n *Node) Stage(ctx context.Context, req StageRequest) error {
	if req.VolumeID == "" {
		return fmt.Errorf("%w: no volume id", ErrInvalidRequest)
	}
	ctx = obs.WithVolume(ctx, req.VolumeID)

	switch protocolOf(req.PublishContext) {
	case ProtocolNFS:
		return n.stageNFS(ctx, req)
	case ProtocolISCSI:
		return n.stageISCSI(ctx, req)
	default:
		return fmt.Errorf("%w: publish context names no supported protocol", ErrInvalidRequest)
	}
}

// Unstage tears down what Stage built. Unstaging a path that is not mounted is a
// success: the kubelet retries Unstage after a partial failure and after a node
// reboot, and a second attempt must not fail the volume's teardown.
func (n *Node) Unstage(ctx context.Context, req UnstageRequest) error {
	if req.StagingPath == "" {
		return fmt.Errorf("%w: no staging path", ErrInvalidRequest)
	}
	ctx = obs.WithVolume(ctx, req.VolumeID)

	if err := n.unmountIfMounted(ctx, req.StagingPath); err != nil {
		return err
	}
	return n.unstageISCSI(ctx, req)
}

// Publish makes a staged volume visible at the pod's target path: a bind mount of
// the staging directory for a filesystem volume, or of the block device onto a file
// target for a raw block volume.
func (n *Node) Publish(ctx context.Context, req PublishRequest) error {
	if req.TargetPath == "" {
		return fmt.Errorf("%w: no target path", ErrInvalidRequest)
	}
	ctx = obs.WithVolume(ctx, req.VolumeID)

	if req.VolumeCapability.Block {
		return n.publishBlock(ctx, req)
	}

	mounted, err := n.isMounted(req.TargetPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := os.MkdirAll(req.TargetPath, 0o750); err != nil {
		return fmt.Errorf("create target path %s: %w", req.TargetPath, err)
	}
	if err := n.bindMount(ctx, req.StagingPath, req.TargetPath, req.Readonly || req.VolumeCapability.Readonly); err != nil {
		return err
	}
	return nil
}

// Unpublish removes the pod's view of the volume. An absent mount is a success.
func (n *Node) Unpublish(ctx context.Context, req UnpublishRequest) error {
	if req.TargetPath == "" {
		return fmt.Errorf("%w: no target path", ErrInvalidRequest)
	}
	ctx = obs.WithVolume(ctx, req.VolumeID)
	return n.unmountIfMounted(ctx, req.TargetPath)
}

// protocolOf reads the protocol out of a publish context, inferring it from the keys
// present when the controller did not name one.
func protocolOf(pc map[string]string) string {
	switch p := strings.ToLower(pc[KeyProtocol]); p {
	case ProtocolNFS, ProtocolISCSI:
		return p
	}
	if pc[KeyNAA] != "" || pc[KeyIQN] != "" {
		return ProtocolISCSI
	}
	if pc[KeyServer] != "" {
		return ProtocolNFS
	}
	return ""
}

// fsTypeOf resolves the filesystem to use, preferring the volume capability and
// falling back to the publish context, then to ext4.
func fsTypeOf(cap VolumeCapability, pc map[string]string) string {
	if cap.FsType != "" {
		return cap.FsType
	}
	if t := pc[KeyFSType]; t != "" {
		return t
	}
	return "ext4"
}

// requireFS fails a stage before any mount is attempted when the node lacks the
// tooling for the requested filesystem, so the operator sees "needs xfsprogs"
// instead of a mount(8) exit code.
func (n *Node) requireFS(fsType string) error {
	switch fsType {
	case "ext4", "ext3", "ext2":
		return n.pre.Require(CapExt4)
	case "xfs":
		return n.pre.Require(CapXFS)
	default:
		return fmt.Errorf("%w: filesystem %q is not supported by this driver", ErrInvalidRequest, fsType)
	}
}

// waitTimeouts bound the two places the node waits on the kernel. They are
// variables, not constants, so tests do not have to spend 30 seconds proving a
// timeout.
var (
	deviceWaitTimeout  = 30 * time.Second
	devicePollInterval = 200 * time.Millisecond
	expandWaitTimeout  = 30 * time.Second
	expandPollInterval = 200 * time.Millisecond
)
