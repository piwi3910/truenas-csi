package csi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/node"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/volume"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type controller struct {
	csipb.UnimplementedControllerServer
	reg   *backend.Registry
	cfg   *config.Config
	locks *VolumeLocks
	nodes backend.NodeResolver
}

// NewController builds the Controller service.
func NewController(reg *backend.Registry, cfg *config.Config) csipb.ControllerServer {
	csiRegistryBackend = func(name string) (backendPaths, error) {
		b, err := reg.Backend(name)
		if err != nil {
			return backendPaths{}, err
		}
		return backendPaths{pool: b.Pool, parent: b.ParentDataset}, nil
	}
	return NewControllerWithNodes(reg, cfg, backend.NewNodeResolver(cfg.NodeID))
}

// NewControllerWithNodes is NewController with the node resolver supplied.
//
// The resolver decides which machine an appliance-side access grant names, so
// it is the one dependency a caller outside a cluster cannot inherit: the
// in-cluster path falls back to LocalNodeResolver, which answers with the
// calling process's OWN interfaces. That is correct for a single-host
// deployment and wrong for the end-to-end suite, which runs on a workstation
// and publishes to a cluster node -- it would grant the workstation and leave
// the real node fenced out. Such callers pass a cluster-scoped resolver here.
func NewControllerWithNodes(reg *backend.Registry, cfg *config.Config,
	nodes backend.NodeResolver) csipb.ControllerServer {
	return &controller{reg: reg, cfg: cfg, locks: NewVolumeLocks(), nodes: nodes}
}

func (c *controller) ControllerGetCapabilities(context.Context, *csipb.ControllerGetCapabilitiesRequest) (*csipb.ControllerGetCapabilitiesResponse, error) {
	rpc := func(t csipb.ControllerServiceCapability_RPC_Type) *csipb.ControllerServiceCapability {
		return &csipb.ControllerServiceCapability{Type: &csipb.ControllerServiceCapability_Rpc{
			Rpc: &csipb.ControllerServiceCapability_RPC{Type: t}}}
	}
	return &csipb.ControllerGetCapabilitiesResponse{Capabilities: []*csipb.ControllerServiceCapability{
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
		rpc(csipb.ControllerServiceCapability_RPC_LIST_VOLUMES),
		rpc(csipb.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_CLONE_VOLUME),
		rpc(csipb.ControllerServiceCapability_RPC_GET_CAPACITY),
		// PUBLISH_UNPUBLISH_VOLUME is what gives this driver a fence at all:
		// without it the CO never calls ControllerUnpublishVolume, and nothing
		// ever revokes a node's appliance-side access to a volume.
		rpc(csipb.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
		// SINGLE_NODE_MULTI_WRITER declares that the SINGLE_NODE_SINGLE_WRITER
		// and SINGLE_NODE_MULTI_WRITER access modes are understood. The driver
		// has always HONOURED them; a CO that reads this capability is the only
		// one that will ever send them, so without it a ReadWriteOncePod PVC
		// never reaches the driver at all.
		rpc(csipb.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER),
		// GET_VOLUME lets the CO ask this driver what the APPLIANCE says about
		// one volume — its real size, and which nodes hold a grant on it.
		rpc(csipb.ControllerServiceCapability_RPC_GET_VOLUME),
		// GET_VOLUME_HEALTH is CSI v1.13's controller-side successor to the
		// alpha `volume_condition` field. It reports only what the controller
		// can actually observe from the appliance; a node's mount is the node
		// plugin's business and is reported through its own GET_VOLUME_HEALTH.
		rpc(csipb.ControllerServiceCapability_RPC_GET_VOLUME_HEALTH),
		// LIST_VOLUMES_PUBLISHED_NODES is answerable because each volume
		// carries its own publish ledger: ListVolumes already reads every
		// owned dataset's user properties, so the node ids come out of the
		// listing it has in hand rather than from a per-volume round trip.
		rpc(csipb.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES),
		// MODIFY_VOLUME is what makes a VolumeAttributesClass reach this driver
		// at all: without it the resizer never calls ControllerModifyVolume and
		// a class attached to a claim is inert. It is honest here because ZFS
		// really does let a mounted volume's `sync`, `compression`, `atime` and
		// `recordsize` change in place — see modifyvolume.go for the closed set
		// and why everything else is refused.
		rpc(csipb.ControllerServiceCapability_RPC_MODIFY_VOLUME),
	}}, nil
}

// resolveID builds the volume id from the request and the backend's configured
// pool and parent dataset.
//
// A StorageClass MAY restate pool and parentDataset, but they are operator
// policy: a mismatch is rejected rather than honoured, so a user-writable
// StorageClass cannot point the driver at another part of the pool.
func (c *controller) resolveID(name string, params map[string]string) (volume.ID, error) {
	backendName := params["backend"]
	if backendName == "" {
		if len(c.cfg.Backends) != 1 {
			return volume.ID{}, status.Error(codes.InvalidArgument,
				"storage class must set the 'backend' parameter when several backends are configured")
		}
		for n := range c.cfg.Backends {
			backendName = n
		}
	}
	b, ok := c.cfg.Backends[backendName]
	if !ok {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"%v: %q", backend.ErrUnknownBackend, backendName)
	}
	if p := params["pool"]; p != "" && p != b.Pool {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"storage class pool %q does not match backend %q pool %q", p, backendName, b.Pool)
	}
	if p := params["parentDataset"]; p != "" && p != b.ParentDataset {
		return volume.ID{}, status.Errorf(codes.InvalidArgument,
			"storage class parentDataset %q does not match backend %q parentDataset %q",
			p, backendName, b.ParentDataset)
	}
	proto := params["protocol"]
	if proto == "" {
		return volume.ID{}, status.Error(codes.InvalidArgument,
			"storage class must set the 'protocol' parameter (nfs or iscsi)")
	}
	id := volume.ID{Backend: backendName, Protocol: proto, Pool: b.Pool, Parent: b.ParentDataset, Name: name}
	id.Namespace = namespaceFor(b, params)
	if err := volume.Confine(id, b.Pool, b.ParentDataset); err != nil {
		return volume.ID{}, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return id, nil
}

// namespaceFor decides which namespace dataset a new volume belongs under, and
// returns "" for the flat layout.
//
// It returns "" in three cases, all of which are ordinary rather than faults:
// the backend has the feature off; the request carries no namespace at all
// (csi-sanity, a static provisioner, or a deployment whose external-provisioner
// runs without --extra-create-metadata); or the namespace is not a name
// Kubernetes could have produced.
//
// That last case is a fallback rather than a refusal on purpose. The parameter
// map is untrusted and a StorageClass author can write these keys by hand, so
// the value has to be checked — but failing CreateVolume over it would let
// anyone with StorageClass edit rights break provisioning, whereas provisioning
// flat costs only the accounting for that one volume, which is exactly what a
// deployment without the metadata already gets.
func namespaceFor(b config.Backend, params map[string]string) string {
	if !b.NamespaceQuotas.Enabled {
		return ""
	}
	ns := volume.IdentityFrom(params).PVCNamespace
	if ns == "" {
		return ""
	}
	if err := volume.ValidateNamespace(ns); err != nil {
		return ""
	}
	return ns
}

func (c *controller) CreateVolume(ctx context.Context, req *csipb.CreateVolumeRequest) (resp *csipb.CreateVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("CreateVolume", err, time.Since(start)) }()

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	id, err := c.resolveID(req.GetName(), req.GetParameters())
	if err != nil {
		return nil, err
	}
	// A claim may name a VolumeAttributesClass at creation as well as later, so
	// the same allowlist governs both paths. Validated here, before anything is
	// provisioned, so a class naming a property this driver refuses costs no
	// dataset — and so the InvalidArgument the spec requires for an unusable
	// class is what the caller gets.
	mod, err := parseModification(req.GetMutableParameters())
	if err != nil {
		return nil, err
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	b, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}

	size := req.GetCapacityRange().GetRequiredBytes()
	if size <= 0 {
		size = 1 << 30 // 1 GiB default, as CSI permits when no range is given
	}
	// Some backends cannot create a volume as small as the claim asks for --
	// TrueNAS refuses a refquota under 1 GiB, so every filesystem-backed claim
	// below that failed to provision at all, with a middleware schema error
	// that named neither the limit nor the field. Rounding up is what CSI
	// allows (the volume must be AT LEAST required_bytes) and it is honest,
	// because the reported capacity is what the appliance really applied.
	if min := b.MinimumCapacityBytes(); size < min {
		// limit_bytes is a ceiling the CO set deliberately. Silently exceeding
		// it would make the PV claim a size the user forbade, so a floor above
		// the ceiling is OutOfRange -- the code CSI reserves for exactly this.
		if limit := req.GetCapacityRange().GetLimitBytes(); limit > 0 && limit < min {
			return nil, status.Errorf(codes.OutOfRange,
				"backend %q cannot create a %s volume smaller than %d bytes, and the "+
					"request limits it to %d; ask for at least %d",
				id.Backend, id.Protocol, min, limit, min)
		}
		obs.Logger(ctx).Info("rounding the request up to the backend's minimum volume size",
			"requested", size, "minimum", min, "protocol", id.Protocol)
		size = min
	}
	if err := c.requireRoomOutsideReserve(ctx, id.Backend, size); err != nil {
		return nil, err
	}
	// The namespace's parent dataset has to exist before the volume beneath it
	// can be created — ZFS will not create intermediate datasets — and its
	// quota is what makes the refusal below more than bookkeeping.
	if err := c.requireRoomInNamespaceQuota(ctx, id, size); err != nil {
		return nil, err
	}
	// What this volume will require of a node, checked against where the CO
	// asked for it BEFORE anything is created. Without this the driver answered
	// with topology the chosen node cannot satisfy: the volume was created, the
	// claim bound, and the pod then failed PreBind for ever with "node affinity
	// doesn't match node" -- a real dataset on the appliance that nothing can
	// ever mount. Seen with a multipath StorageClass on nodes without
	// multipath-tools.
	if err := requireKnownParameters(b, req.GetParameters()); err != nil {
		return nil, err
	}

	topology := requiredTopology(id.Backend, id.Protocol, req.GetParameters())
	if err := requireTopologyReachable(req.GetAccessibilityRequirements(), topology); err != nil {
		return nil, err
	}

	cr := backend.CreateRequest{ID: id, CapacityBytes: size, Params: req.GetParameters()}
	if src := req.GetVolumeContentSource(); src != nil {
		if s := src.GetSnapshot(); s != nil {
			if err := c.requireSnapshotExists(ctx, s.GetSnapshotId()); err != nil {
				return nil, err
			}
			srcBackend, zfsID, sErr := backend.SnapshotSource(s.GetSnapshotId())
			if sErr != nil {
				return nil, status.Errorf(codes.NotFound, "snapshot %q not found", s.GetSnapshotId())
			}
			if srcBackend != id.Backend {
				return nil, status.Errorf(codes.InvalidArgument,
					"snapshot %q lives on backend %q but the volume would be created on %q",
					s.GetSnapshotId(), srcBackend, id.Backend)
			}
			// The backend talks to ZFS, which knows nothing of the backend
			// prefix the CSI id carries.
			cr.SourceSnapshot = zfsID
		} else if v := src.GetVolume(); v != nil {
			// Cloning a volume is a snapshot plus a clone. The intermediate
			// snapshot is named after the new volume so a retry finds it again
			// rather than making a second one.
			srcID, perr := volume.ParseID(v.GetVolumeId())
			if perr != nil {
				return nil, status.Errorf(codes.NotFound, "source volume %q not found", v.GetVolumeId())
			}
			if err := c.requireVolumeExists(ctx, srcID); err != nil {
				return nil, err
			}
			snap, serr := c.reg.CreateSnapshot(ctx, srcID, "csi-clone-"+sanitiseSnapshotName(req.GetName()))
			if serr != nil {
				return nil, toStatus(serr)
			}
			_, zfsID, sErr := backend.SnapshotSource(snap.ID)
			if sErr != nil {
				return nil, toStatus(sErr)
			}
			cr.SourceSnapshot = zfsID
		}
	}

	vol, err := b.Create(ctx, cr)
	if err != nil {
		return nil, toStatus(err)
	}
	obs.Logger(ctx).Info("volume created", "capacity", vol.CapacityBytes, "protocol", id.Protocol)

	// Applied after the volume exists, because the properties are set on the
	// dataset and some of them depend on whether it turned out to be a
	// filesystem or a zvol. A failure here fails CreateVolume: a claim that
	// asked for sync=disabled and silently got the default would be a worse
	// outcome than a claim that stays Pending with the reason on it. Create is
	// idempotent, so the CO's retry finds the same dataset and tries again.
	if !mod.empty() {
		ds, mErr := c.volumeDataset(ctx, vol.ID)
		if mErr != nil {
			return nil, mErr
		}
		if mErr := c.applyModification(ctx, vol.ID, ds, mod); mErr != nil {
			return nil, mErr
		}
	}

	// Everything the node can be told before an attach travels in the volume
	// context, so a PersistentVolume carries the server, share and identity of
	// its volume without a round trip. What is decided per attachment — the
	// iSCSI LUN, above all — is deliberately NOT here: it does not exist yet,
	// and ControllerPublishVolume returns it. PublishContext is read-only, so a
	// volume it cannot describe yet is skipped rather than provisioned open.
	vctx := map[string]string{}
	for k, v := range vol.Context {
		vctx[k] = v
	}
	if pc, pcErr := b.PublishContext(ctx, vol.ID); pcErr == nil {
		for k, v := range pc {
			// A credential is not a locator, and this map becomes the
			// PersistentVolume's volumeAttributes -- a cluster-scoped object,
			// stored unencrypted, readable by anything holding `get pv`, and
			// kept for the life of the volume. The CHAP secret was landing
			// there in plaintext, and because one CHAP credential serves a
			// whole backend's shared target, a single PV read exposed every
			// iSCSI volume on that appliance.
			//
			// The node does not need it here: it reads the publish context
			// delivered with the attachment, and prefers a node-stage Secret
			// over both. See chapCredentials in internal/node/iscsi.go.
			if sensitivePublishKeys[k] {
				continue
			}
			vctx[k] = v
		}
	}
	// A few StorageClass parameters are read by the NODE rather than by a
	// backend, so nothing else would carry them: a backend populates the
	// context with its own protocol keys, and the parameters themselves never
	// leave the controller. Copied by an explicit allowlist, never wholesale --
	// a StorageClass also carries backend selection, share options and
	// credential references, none of which the node should receive.
	//
	// Without this the I/O limits work on a static PersistentVolume and silently
	// do nothing on a dynamically provisioned one, which is the shape of bug
	// that gets discovered during an incident.
	for _, k := range node.NodeParameterKeys {
		if v, ok := req.GetParameters()[k]; ok && v != "" {
			vctx[k] = v
		}
	}
	// The claim name, for the node's per-volume I/O metrics. Kubernetes gives
	// it to CreateVolume but NOT to NodePublishVolume, so without this echo the
	// `pvc` label was permanently empty and every dashboard had to join through
	// kube-state-metrics to name the claim a series belongs to. Echoing it
	// costs one string in the PV's volumeAttributes and gives the node plugin
	// no new credentials or lookups.
	if name := volume.IdentityFrom(req.GetParameters()).PVCName; name != "" {
		vctx[node.KeyPVCName] = name
	}
	out := &csipb.Volume{
		VolumeId:           vol.ID.String(),
		CapacityBytes:      vol.CapacityBytes,
		VolumeContext:      vctx,
		AccessibleTopology: topology,
	}
	if cr.SourceSnapshot != "" {
		out.ContentSource = &csipb.VolumeContentSource{Type: &csipb.VolumeContentSource_Snapshot{
			Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: cr.SourceSnapshot}}}
	}
	return &csipb.CreateVolumeResponse{Volume: out}, nil
}

func (c *controller) DeleteVolume(ctx context.Context, req *csipb.DeleteVolumeRequest) (resp *csipb.DeleteVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("DeleteVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		// An unparseable id cannot name anything we created, so there is
		// nothing to delete. CSI requires success here.
		obs.Logger(ctx).Warn("ignoring delete for unparseable volume id", "error", err)
		return &csipb.DeleteVolumeResponse{}, nil
	}
	ctx = obs.WithVolume(ctx, id.String())

	b, ok := c.cfg.Backends[id.Backend]
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "%v: %q", backend.ErrUnknownBackend, id.Backend)
	}
	if err := volume.Confine(id, b.Pool, b.ParentDataset); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	if err := be.Delete(ctx, id); err != nil {
		return nil, toStatus(err)
	}
	c.reclaimNamespaceDataset(ctx, id)
	return &csipb.DeleteVolumeResponse{}, nil
}

// reclaimNamespaceDataset removes a namespace's parent dataset once its last
// volume has gone, so a namespace that existed for an afternoon does not leave
// an empty dataset behind for ever.
//
// It is best-effort by design and never fails the DeleteVolume that triggered
// it: the volume the CO asked about IS gone, and reporting an error would make
// the CO retry a delete that has already succeeded. A dataset left behind is
// visible in the TrueNAS UI and costs nothing; a DeleteVolume stuck in a retry
// loop blocks the PersistentVolume from being released.
func (c *controller) reclaimNamespaceDataset(ctx context.Context, id volume.ID) {
	if id.Namespace == "" {
		return
	}
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		obs.Logger(ctx).Warn("skipping namespace dataset reclamation: appliance unreachable",
			"namespace", id.Namespace, "error", obs.Redact(err.Error()))
		return
	}
	deleted, err := backend.ReclaimNamespace(ctx, cl, id.Pool, id.Parent, id.Namespace)
	if err != nil {
		obs.Logger(ctx).Warn("namespace dataset was not reclaimed",
			"namespace", id.Namespace, "error", obs.Redact(err.Error()))
		return
	}
	if !deleted {
		obs.Logger(ctx).Debug("namespace dataset kept: it still holds datasets",
			"namespace", id.Namespace)
	}
}

func (c *controller) ControllerExpandVolume(ctx context.Context, req *csipb.ControllerExpandVolumeRequest) (resp *csipb.ControllerExpandVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerExpandVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity range is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	ctx = obs.WithVolume(ctx, id.String())
	want := req.GetCapacityRange().GetRequiredBytes()
	if want <= 0 {
		return nil, status.Error(codes.InvalidArgument, "required_bytes must be positive")
	}

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	got, err := be.Expand(ctx, id, want)
	if err != nil {
		return nil, toStatus(err)
	}
	// Node expansion is about the DEVICE, not about the filesystem, so it is
	// decided by the protocol and not by whether the volume is raw block.
	//
	// A raw block volume has no filesystem to grow, and that is what the old
	// test read: block => nothing for the node to do. But an iSCSI or NVMe
	// initiator caches the device's size, so without a node-side rescan the pod
	// keeps seeing the OLD size while the PersistentVolumeClaim reports the new
	// one. Measured on hardware: a Block PVC grown 1Gi -> 3Gi reported 3Gi to
	// Kubernetes while blockdev --getsize64 in the pod still said 1073741824,
	// and stayed that way until the volume was re-attached. The node plugin has
	// always handled the block case correctly -- rescan, then skip the
	// filesystem resize -- it was simply never asked.
	//
	// NFS and SMB genuinely need nothing: the size a pod sees is the dataset's
	// refquota, which changed on the appliance.
	return &csipb.ControllerExpandVolumeResponse{
		CapacityBytes:         got,
		NodeExpansionRequired: nodeExpansionRequired(id.Protocol),
	}, nil
}

// nodeExpansionRequired reports whether the node must act after the controller
// has grown a volume. See ControllerExpandVolume for why this is a question
// about the protocol and not about the access type.
func nodeExpansionRequired(protocol string) bool {
	switch protocol {
	case node.ProtocolNFS, node.ProtocolSMB:
		return false
	default:
		return true
	}
}

// ControllerPublishVolume grants one node appliance-side access to a volume.
//
// The grant is recorded in the volume's own publish ledger as well as made on
// the appliance, because the appliance cannot answer "which node holds this".
// A shared iSCSI target has no per-node LUN, and an export's host list does not
// say which entry belongs to whom — so the single-writer rule below, and the
// revoke that has to work after the Node object is deleted, both read from the
// ledger rather than from appliance state.
func (c *controller) ControllerPublishVolume(ctx context.Context, req *csipb.ControllerPublishVolumeRequest) (resp *csipb.ControllerPublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerPublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if req.GetNodeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "node id is required")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	mode := req.GetVolumeCapability().GetAccessMode().GetMode()
	if !supportsAccessMode(id.Protocol, mode) {
		return nil, status.Errorf(codes.InvalidArgument,
			"protocol %s does not support access mode %s", id.Protocol, mode)
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	if err := c.requireVolumeExists(ctx, id); err != nil {
		return nil, err
	}
	node, err := c.nodes.Resolve(ctx, req.GetNodeId())
	if err != nil {
		if errors.Is(err, backend.ErrNodeNotFound) {
			return nil, status.Errorf(codes.NotFound, "node %q does not exist", req.GetNodeId())
		}
		return nil, status.Errorf(codes.Internal, "resolving node %q: %v", req.GetNodeId(), obs.Redact(err.Error()))
	}

	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	grants, err := backend.ReadGrants(ctx, cl, id)
	if err != nil {
		return nil, toStatus(err)
	}
	if others := grants.Others(node.ID); len(others) > 0 && singleNode(mode) {
		// The whole point of the fence: a SINGLE_NODE volume must not be
		// reachable from two nodes at once, and the second publisher is the one
		// that has to be refused. FailedPrecondition rather than AlreadyExists
		// is what tells the CO to keep retrying until the first node's
		// ControllerUnpublishVolume lands.
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is published to %v with access mode %s and cannot also be published to %s",
			id, others, mode, node.ID)
	}

	pub, err := c.publisher(ctx, id)
	if err != nil {
		return nil, err
	}
	pc, err := pub.Publish(ctx, id, node)
	if err != nil {
		return nil, toStatus(err)
	}

	// Recorded only after the grant succeeded: a ledger entry for access that
	// was never made would make the next single-writer check refuse a healthy
	// publish.
	grants[node.ID] = node.Addrs
	if err := backend.SaveGrants(ctx, cl, id, grants); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerPublishVolumeResponse{PublishContext: pc}, nil
}

// ControllerUnpublishVolume revokes a node's appliance-side access — the fence.
//
// It is deliberately forgiving about everything except the revoke itself: an
// unparseable id, a deleted volume and a node that was never published are all
// success, because CSI requires the CO to be able to retire an attachment whose
// backing objects have already gone. What it will NOT do is report success
// while the appliance still serves the data.
func (c *controller) ControllerUnpublishVolume(ctx context.Context, req *csipb.ControllerUnpublishVolumeRequest) (resp *csipb.ControllerUnpublishVolumeResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("ControllerUnpublishVolume", err, time.Since(start)) }()

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		// An unparseable id cannot name anything this driver published.
		obs.Logger(ctx).Warn("ignoring unpublish for unparseable volume id", "error", err)
		return &csipb.ControllerUnpublishVolumeResponse{}, nil
	}
	ctx = obs.WithVolume(ctx, id.String())

	release, ok := c.locks.TryAcquire(id.String())
	if !ok {
		return nil, status.Errorf(codes.Aborted, "another operation is in progress for volume %s", id)
	}
	defer release()

	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return nil, toStatus(err)
	}
	grants, err := backend.ReadGrants(ctx, cl, id)
	if err != nil {
		if errors.Is(err, backend.ErrVolumeGone) {
			return &csipb.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, toStatus(err)
	}

	// An empty node id means "from every node", which is what the CSI spec says
	// and also what a fencing controller wants when it no longer trusts any of
	// them.
	targets := []string{req.GetNodeId()}
	if req.GetNodeId() == "" {
		targets = grants.Nodes()
	}
	if len(targets) == 0 {
		return &csipb.ControllerUnpublishVolumeResponse{}, nil
	}

	pub, err := c.publisher(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, nodeID := range targets {
		// The ledger, not the Node object, is what the revoke is written from.
		// Fencing a node most often happens because that node is gone, and a
		// resolve that fails then would leave its addresses on the share
		// forever — the exact hole this call exists to close.
		node := backend.NodeRef{ID: nodeID, Addrs: grants[nodeID]}
		if live, rErr := c.nodes.Resolve(ctx, nodeID); rErr == nil {
			node.IQN, node.NQN = live.IQN, live.NQN
			node.Addrs = union(node.Addrs, live.Addrs)
		}
		if err := pub.Unpublish(ctx, id, node); err != nil {
			return nil, toStatus(err)
		}
		delete(grants, nodeID)
	}
	if err := backend.SaveGrants(ctx, cl, id, grants); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.ControllerUnpublishVolumeResponse{}, nil
}

// publisher returns the backend for a volume as a Publisher.
func (c *controller) publisher(ctx context.Context, id volume.ID) (backend.Publisher, error) {
	be, err := c.reg.For(ctx, id.Backend, id.Protocol)
	if err != nil {
		return nil, toStatus(err)
	}
	pub, ok := be.(backend.Publisher)
	if !ok {
		return nil, status.Errorf(codes.Internal,
			"protocol %s cannot grant or revoke node access", id.Protocol)
	}
	return pub, nil
}

// union merges two address lists without duplicates, in a stable order.
func union(a, b []string) []string {
	set := map[string]bool{}
	for _, v := range append(append([]string{}, a...), b...) {
		if v != "" {
			set[v] = true
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func (c *controller) ValidateVolumeCapabilities(ctx context.Context, req *csipb.ValidateVolumeCapabilitiesRequest) (*csipb.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	id, err := volume.ParseID(req.GetVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown volume %q", req.GetVolumeId())
	}
	if err := c.requireVolumeExists(ctx, id); err != nil {
		return nil, err
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !supportsAccessMode(id.Protocol, cap.GetAccessMode().GetMode()) {
			return &csipb.ValidateVolumeCapabilitiesResponse{
				Message: fmt.Sprintf("protocol %s does not support access mode %s",
					id.Protocol, cap.GetAccessMode().GetMode()),
			}, nil
		}
	}
	return &csipb.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csipb.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities()},
	}, nil
}

func (c *controller) CreateSnapshot(ctx context.Context, req *csipb.CreateSnapshotRequest) (resp *csipb.CreateSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("CreateSnapshot", err, time.Since(start)) }()

	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
	}
	if req.GetSourceVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "source volume id is required")
	}
	id, err := volume.ParseID(req.GetSourceVolumeId())
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "unknown source volume %q", req.GetSourceVolumeId())
	}
	name := sanitiseSnapshotName(req.GetName())
	if err := c.rejectSnapshotNameReuse(ctx, id, name); err != nil {
		return nil, err
	}
	s, err := c.reg.CreateSnapshot(ctx, id, name)
	if err != nil {
		return nil, toStatus(err)
	}
	return &csipb.CreateSnapshotResponse{Snapshot: &csipb.Snapshot{
		SnapshotId: s.ID, SourceVolumeId: req.GetSourceVolumeId(),
		// SizeBytes is the source volume's provisioned size. Omitting it left
		// every VolumeSnapshot with an empty status.restoreSize, which in turn
		// let external-provisioner accept a restore claim smaller than the
		// volume the snapshot came from.
		SizeBytes:    s.SizeBytes,
		CreationTime: timestamppb.New(s.CreationTime), ReadyToUse: s.ReadyToUse,
	}}, nil
}

func (c *controller) DeleteSnapshot(ctx context.Context, req *csipb.DeleteSnapshotRequest) (resp *csipb.DeleteSnapshotResponse, err error) {
	start := time.Now()
	defer func() { obs.ObserveCSI("DeleteSnapshot", err, time.Since(start)) }()

	if req.GetSnapshotId() == "" {
		return nil, status.Error(codes.InvalidArgument, "snapshot id is required")
	}
	// An unparseable id cannot name a snapshot this driver created, so there is
	// nothing to delete. CSI requires success, exactly as for DeleteVolume.
	if _, _, err := backend.SnapshotSource(req.GetSnapshotId()); err != nil {
		obs.Logger(ctx).Warn("ignoring delete for unparseable snapshot id",
			"snapshot_id", req.GetSnapshotId())
		return &csipb.DeleteSnapshotResponse{}, nil
	}
	if err := c.reg.DeleteSnapshot(ctx, req.GetSnapshotId()); err != nil {
		return nil, toStatus(err)
	}
	return &csipb.DeleteSnapshotResponse{}, nil
}

// sanitiseSnapshotName keeps a CSI snapshot name usable as a ZFS snapshot name.
func sanitiseSnapshotName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.', r == ':':
			return r
		}
		return '-'
	}, n)
	if len(n) > 200 {
		n = n[:200]
	}
	return n
}

// toStatus preserves an already-typed gRPC status and otherwise maps unknown
// failures to Internal. Middleware errnames are deliberately not consulted:
// they report EINVAL for conditions whose real errno is something else.
func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		if status.Code(err) != codes.Unknown {
			return err
		}
	}
	return status.Error(codes.Internal, obs.Redact(err.Error()))
}

// requireVolumeExists returns NotFound when the dataset behind a volume id is
// absent. Several CSI calls must distinguish "you named nothing" from a real
// failure, and only a query can tell them apart — the middleware's errname
// cannot.
func (c *controller) requireVolumeExists(ctx context.Context, id volume.ID) error {
	cl, err := c.reg.Client(ctx, id.Backend)
	if err != nil {
		return toStatus(err)
	}
	ds, err := cl.DatasetQuery(ctx, id.DatasetPath())
	if err != nil {
		return toStatus(err)
	}
	if ds == nil {
		return status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	return nil
}

// requireSnapshotExists returns NotFound for a snapshot that is not there.
func (c *controller) requireSnapshotExists(ctx context.Context, snapshotID string) error {
	backendName, zfsID, err := backend.SnapshotSource(snapshotID)
	if err != nil {
		return status.Errorf(codes.NotFound, "snapshot %q does not exist", snapshotID)
	}
	cl, err := c.reg.Client(ctx, backendName)
	if err != nil {
		return toStatus(err)
	}
	snap, err := cl.SnapshotQuery(ctx, zfsID)
	if err != nil {
		return toStatus(err)
	}
	if snap == nil {
		return status.Errorf(codes.NotFound, "snapshot %q does not exist", snapshotID)
	}
	return nil
}

// rejectSnapshotNameReuse enforces the CSI rule that one snapshot name may not
// refer to two different source volumes.
func (c *controller) rejectSnapshotNameReuse(ctx context.Context, source volume.ID, name string) error {
	cl, err := c.reg.Client(ctx, source.Backend)
	if err != nil {
		return toStatus(err)
	}
	b, err := c.reg.Backend(source.Backend)
	if err != nil {
		return toStatus(err)
	}
	snaps, err := cl.SnapshotList(ctx, b.Pool+"/"+b.ParentDataset)
	if err != nil {
		return toStatus(err)
	}
	want := source.DatasetPath()
	for _, s := range snaps {
		if snapshotNameOf(s.ID) != name {
			continue
		}
		if s.Dataset != want {
			return status.Errorf(codes.AlreadyExists,
				"snapshot name %q already exists for source volume %s", name, s.Dataset)
		}
	}
	return nil
}

// snapshotNameOf returns the part after the @ in a ZFS snapshot id.
func snapshotNameOf(id string) string {
	if i := strings.LastIndex(id, "@"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// requiredTopology names the node capabilities a volume needs, so the scheduler
// will not place a pod on a node that cannot attach or mount it.
//
// Publishing capability labels from the node is only half of topology: without
// the matching requirement here, every node looks equally able and the pod is
// scheduled somewhere that then fails to mount.
// commonParameters are the StorageClass keys the CSI layer itself reads, as
// opposed to the ones a backend reads.
var commonParameters = []string{
	"backend", "protocol", "pool", "parentDataset", "fsType", "multipath",
}

// sensitivePublishKeys are publish-context entries that must never be copied
// into the volume context, because that map is persisted verbatim in the
// PersistentVolume.
//
// Membership is by what the value IS, not by what it is called: chapSecretRef
// is a tag naming the credential on the appliance and is safe to persist, while
// chapSecret is the credential itself and is not.
var sensitivePublishKeys = map[string]bool{
	"chapSecret": true,
	"password":   true,
}

// CommonParameters is the set the CSI layer itself reads, exported so a test
// can check the accepted set against what the sources actually read.
func CommonParameters() []string { return append([]string(nil), commonParameters...) }

// requireKnownParameters refuses a StorageClass carrying a key nothing reads.
//
// An unknown key used to be ignored in silence, and a StorageClass is
// immutable: a class saying nfsVersionn: "3" provisioned NFSv4 and reported
// success, and the same typo in maproot, mode or networks drops a security
// setting the operator believes is in force. The failure is deliberately at the
// first claim rather than in a log line nobody reads.
func requireKnownParameters(b backend.Backend, params map[string]string) error {
	known := map[string]bool{}
	for _, k := range commonParameters {
		known[k] = true
	}
	for _, k := range b.AcceptedParameters() {
		known[k] = true
	}
	for _, k := range node.NodeParameterKeys {
		known[k] = true
	}

	var unknown []string
	for k := range params {
		// Everything the CO injects lives under this prefix -- the claim's name
		// and namespace, the provisioner's own identity, the node-stage secret
		// references. None of it comes from the operator, and the set grows
		// with Kubernetes rather than with this driver.
		if strings.HasPrefix(k, "csi.storage.k8s.io/") || known[k] {
			continue
		}
		unknown = append(unknown, k)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)

	accepted := make([]string, 0, len(known))
	for k := range known {
		accepted = append(accepted, k)
	}
	sort.Strings(accepted)

	msg := fmt.Sprintf("storage class sets %s, which this driver does not read",
		strings.Join(quoteAll(unknown), ", "))
	if s := nearestParameter(unknown[0], accepted); s != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", s)
	}
	return status.Errorf(codes.InvalidArgument, "%s. Accepted: %s",
		msg, strings.Join(accepted, ", "))
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strconv.Quote(s)
	}
	return out
}

// nearestParameter suggests the accepted key closest to a rejected one, so the
// common case -- a typo -- is answered rather than merely reported. It returns
// "" when nothing is close enough to be worth guessing at.
func nearestParameter(got string, accepted []string) string {
	best, bestDist := "", 0
	for _, cand := range accepted {
		d := editDistance(strings.ToLower(got), strings.ToLower(cand))
		if best == "" || d < bestDist {
			best, bestDist = cand, d
		}
	}
	// A third of the length, so "nfsVersionn" finds "nfsVersion" and an
	// unrelated word suggests nothing.
	if bestDist > 0 && bestDist <= 1+len(got)/3 {
		return best
	}
	return ""
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(min(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// requireTopologyReachable refuses a volume the CO has asked to place where it
// cannot work.
//
// external-provisioner sends the chosen node's topology in requisite when the
// StorageClass binds late, which is the only chance to notice that the node
// lacks something the volume needs. ResourceExhausted is the code the CSI spec
// reserves for "unable to provision in accessible_topology", and the one that
// lets the caller retry somewhere else rather than give up.
//
// A key the candidate does not mention is NOT treated as a mismatch. Node
// plugins roll separately from the controller -- the DaemonSet is OnDelete --
// so a node that has not yet learned to publish a newly added key would
// otherwise become unschedulable for every volume during an upgrade. Only a key
// the node publishes with a DIFFERENT value is a refusal, which is exactly the
// case that matters: multipath=false, xfs=false.
func requireTopologyReachable(req *csipb.TopologyRequirement, want []*csipb.Topology) error {
	if req == nil || len(want) == 0 {
		return nil
	}
	candidates := req.GetRequisite()
	if len(candidates) == 0 {
		candidates = req.GetPreferred()
	}
	if len(candidates) == 0 {
		return nil
	}
	need := want[0].GetSegments()
	var missing []string
	for _, c := range candidates {
		have := c.GetSegments()
		ok := true
		for k, v := range need {
			if got, present := have[k]; present && got != v {
				ok = false
				if !slices.Contains(missing, k) {
					missing = append(missing, k)
				}
			}
		}
		if ok {
			return nil
		}
	}
	sort.Strings(missing)
	return status.Errorf(codes.ResourceExhausted,
		"no node the scheduler offered can serve this volume: it needs %s. "+
			"The node plugin publishes what each node supports; a volume created here "+
			"would bind and then never mount",
		strings.Join(missing, ", "))
}

func requiredTopology(backendName, protocol string, params map[string]string) []*csipb.Topology {
	segments := map[string]string{
		node.TopologyKey(node.Capability(protocol)): "true",
	}
	// Tooling is not enough: a node can have every binary this volume needs and
	// no route to the appliance it lives on. Requiring the backend's own
	// reachability label keeps the scheduler off those nodes.
	if backendName != "" {
		segments[node.BackendTopologyKey(backendName)] = "true"
	}
	if fs := params["fsType"]; fs != "" && fs != "ext4" {
		segments[node.TopologyKey(node.Capability(fs))] = "true"
	} else if protocol == "iscsi" || protocol == "nvme" {
		segments[node.TopologyKey(node.CapExt4)] = "true"
	}
	if params["multipath"] == "true" {
		segments[node.TopologyKey(node.CapMultipath)] = "true"
	}
	return []*csipb.Topology{{Segments: segments}}
}
