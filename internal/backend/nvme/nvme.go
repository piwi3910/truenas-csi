// Package nvme provisions NVMe-oF block volumes on TrueNAS: one zvol per
// volume, exported as a namespace inside a subsystem of its own, bound to a
// shared transport port.
//
// ONE SUBSYSTEM PER VOLUME is the defining decision of this package, and it is
// deliberately the opposite of the iSCSI backend's one shared target. The
// reason is structural rather than stylistic:
//
//   - An NVMe namespace only exists INSIDE a subsystem, and a subsystem is what
//     an initiator connects to. A shared subsystem would therefore expose every
//     volume's namespace to every node that connects — the same exposure the
//     iSCSI backend accepts for LUNs, but without iSCSI's excuse, because here
//     the alternative costs nothing.
//   - Host ACLs (nvmet.host + nvmet.host_subsys) are a property of the
//     subsystem. Per-volume subsystems make the ACL per-volume too.
//   - Teardown is clean: deleting a volume deletes its subsystem outright, so
//     no shared object accumulates dangling namespaces, and there is no
//     equivalent of iSCSI's LUN-id allocation problem to get wrong.
//
// The port is the one shared object, because it is an appliance-wide listener:
// it is queried first and created only when absent, and it is never removed on
// a volume's rollback or delete.
//
// Everything the create path issues was verified against a live TrueNAS
// 25.10.6 (see .procoder/notes/truenas-api-findings.md):
//
//	nvmet.subsys.create {name, allow_any_host}  -> subnqn = <basenqn>:<name>, plus serial
//	nvmet.namespace.create {subsys_id, device_type: ZVOL, device_path: zvol/<dataset>}
//	nvmet.port.create {addr_trtype: TCP, addr_traddr: <ip>, addr_trsvcid: 4420}
//	nvmet.port_subsys.create {port_id, subsys_id}
package nvme

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/retention"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Protocol is the StorageClass "protocol" value this backend serves.
const Protocol = "nvme"

// StorageClass parameter names.
const (
	// ParamTransport is "tcp" (default) or "rdma".
	ParamTransport = "transport"
	// ParamHostNQNs is a comma-separated list of initiator NQNs allowed to
	// connect. EMPTY means allow_any_host — see ensureSubsystem.
	ParamHostNQNs = "hostNQNs"
	// ParamSparse controls thin provisioning of the zvol.
	ParamSparse = "sparse"
	// ParamVolBlockSize overrides the appliance's recommended volblocksize.
	ParamVolBlockSize = "volblocksize"
	// ParamPortAddress pins the address the port listens on, for an appliance
	// with more than one usable address.
	ParamPortAddress = "portAddress"
	// ParamPort overrides the transport service id (the TCP port).
	ParamPort = "port"
)

// Transport values for ParamTransport.
const (
	TransportTCP  = "tcp"
	TransportRDMA = "rdma"
)

// DefaultPort is the IANA-assigned NVMe-oF port and the one the live probe used.
const DefaultPort = 4420

// maxSubsysName bounds the subsystem name. An NQN is capped at 223 characters
// and the appliance's basenqn already spends ~60 of them, so a long PVC name is
// hashed down rather than allowed to make a volume unprovisionable.
const maxSubsysName = 96

// ErrShrinkNotAllowed is returned for an expansion request smaller than the
// current size. The middleware refuses a zvol shrink too, but its message says
// nothing an operator can act on, so the driver refuses first.
var ErrShrinkNotAllowed = errors.New("volume cannot be shrunk")

// ErrVolumeNotFound is returned when an operation names a zvol that is not there.
var ErrVolumeNotFound = errors.New("volume does not exist")

// ErrRDMAUnavailable is returned when a StorageClass asks for RDMA on an
// appliance that reports nvmet.global.rdma = false.
//
// RDMA/RoCE is IMPLEMENTED BUT UNVALIDATED: the validation appliance reports
// rdma=false and the cluster's RK3588 nodes have no RDMA NICs, so not one line
// of the RDMA path has ever moved a byte. Refusing here is the whole point —
// creating an RDMA port on an appliance that cannot serve it produces an export
// nothing can connect to, and the failure would surface much later as an
// unexplained attach timeout on a node.
var ErrRDMAUnavailable = errors.New("this appliance does not support NVMe over RDMA")

func init() { backend.Register(Protocol, New) }

// nvmeBackend provisions NVMe-oF volumes on one appliance.
type nvmeBackend struct {
	c      truenas.API
	pool   string
	parent string
	// retire is the delete-protection policy. Its zero value is "off", which is
	// the default and takes exactly the destroy path this driver always took.
	retire retention.Policy
}

// New builds the backend for one appliance. It matches backend.Factory.
func New(c truenas.API, opts backend.Options) backend.Backend {
	return &nvmeBackend{c: c, pool: opts.Pool, parent: opts.Parent, retire: opts.Retention}
}

// Protocol implements backend.Backend.
func (b *nvmeBackend) Protocol() string { return Protocol }

// Params is the StorageClass configuration this backend understands.
type Params struct {
	Transport string
	HostNQNs  []string

	Sparse       bool
	VolBlockSize string

	PortAddress string
	Port        int
}

// parseParams applies the documented defaults.
func parseParams(m map[string]string) (Params, error) {
	p := Params{Transport: TransportTCP, Sparse: true, Port: DefaultPort}

	switch t := strings.ToLower(strings.TrimSpace(m[ParamTransport])); t {
	case "":
	case TransportTCP, TransportRDMA:
		p.Transport = t
	default:
		return Params{}, fmt.Errorf("storage class parameter %s must be %q or %q, got %q",
			ParamTransport, TransportTCP, TransportRDMA, t)
	}

	if v := strings.TrimSpace(m[ParamSparse]); v != "" {
		s, err := strconv.ParseBool(v)
		if err != nil {
			return Params{}, fmt.Errorf("storage class parameter %s must be true or false, got %q", ParamSparse, v)
		}
		p.Sparse = s
	}
	if v := strings.TrimSpace(m[ParamPort]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 65535 {
			return Params{}, fmt.Errorf("storage class parameter %s must be a TCP port, got %q", ParamPort, v)
		}
		p.Port = n
	}
	p.VolBlockSize = strings.TrimSpace(m[ParamVolBlockSize])
	p.PortAddress = strings.TrimSpace(m[ParamPortAddress])
	for _, nqn := range strings.Split(m[ParamHostNQNs], ",") {
		if nqn = strings.TrimSpace(nqn); nqn != "" {
			p.HostNQNs = append(p.HostNQNs, nqn)
		}
	}
	return p, nil
}

// subsystemName derives the subsystem name, which is also the NQN's suffix.
//
// Truncation alone would be a data-exposure bug rather than a cosmetic one: two
// PVC names sharing a long prefix would collapse onto ONE subsystem, so two
// volumes would be reachable through the same NQN. The hash suffix is what
// keeps distinct volumes distinct.
func subsystemName(id volume.ID) string {
	safe := sanitizeNQN("csi-" + id.Name)
	if len(safe) <= maxSubsysName {
		return safe
	}
	sum := sha256.Sum256([]byte(id.Name))
	suffix := "-" + hex.EncodeToString(sum[:])[:12]
	return safe[:maxSubsysName-len(suffix)] + suffix
}

// sanitizeNQN reduces a string to the characters an NQN suffix accepts.
func sanitizeNQN(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// devicePath is the middleware's spelling of a zvol as a namespace backing
// device, verified live: "zvol/<pool>/<parent>/<name>".
func devicePath(id volume.ID) string { return "zvol/" + id.DatasetPath() }

// Create provisions a zvol and exports it as the only namespace of its own
// subsystem, bound to the shared transport port.
//
// Every step is query-then-act, so a retried CreateVolume converges on what is
// already there. Anything this call creates is torn down in reverse order if a
// later step fails: a zvol with no namespace is invisible to Kubernetes, which
// means nothing will ever delete it.
func (b *nvmeBackend) Create(ctx context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	ctx = obs.WithVolume(ctx, r.ID.String())
	p, err := parseParams(r.Params)
	if err != nil {
		return nil, err
	}

	// The transport is checked against the appliance BEFORE anything is
	// created, so an impossible request leaves nothing behind at all.
	global, err := b.c.NVMeGlobalConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading NVMe-oF global config: %w", err)
	}
	if p.Transport == TransportRDMA && !global.RDMA {
		return nil, fmt.Errorf("%w: set transport=tcp, or enable RDMA on the appliance", ErrRDMAUnavailable)
	}

	var rollback []func()
	undo := func() {
		for i := len(rollback) - 1; i >= 0; i-- {
			rollback[i]()
		}
	}

	if err := b.ensureZvol(ctx, r, p, &rollback); err != nil {
		undo()
		return nil, err
	}

	subsys, err := b.ensureSubsystem(ctx, r.ID, p, &rollback)
	if err != nil {
		undo()
		return nil, err
	}
	if err := b.ensureNamespace(ctx, r.ID, subsys.ID, &rollback); err != nil {
		undo()
		return nil, err
	}
	port, err := b.ensurePort(ctx, p, &rollback)
	if err != nil {
		undo()
		return nil, err
	}
	// The subsystem is deliberately NOT bound to the port here. That binding is
	// the fence: a subsystem reachable through a port from the moment it is
	// provisioned is discoverable by every node on the fabric, whether or not
	// anything has attached it. ControllerPublishVolume creates the binding and
	// ControllerUnpublishVolume removes it.
	if err := b.c.SetUserProperty(ctx, r.ID.DatasetPath(), volume.NVMePortProperty,
		strconv.Itoa(port.ID)); err != nil {
		undo()
		return nil, fmt.Errorf("recording the NVMe-oF port on %s: %w", r.ID.DatasetPath(), err)
	}
	if err := b.ensureHostACL(ctx, subsys.ID, p, &rollback); err != nil {
		undo()
		return nil, err
	}

	portal, err := portalAddress(b.c, port)
	if err != nil {
		undo()
		return nil, err
	}

	obs.Logger(ctx).Info("created NVMe-oF volume",
		"subsystem", subsys.Name, "nqn", subsys.SubNQN, "transport", p.Transport)
	return &backend.Volume{
		ID:            r.ID,
		CapacityBytes: r.CapacityBytes,
		Context:       publishContext(subsys, portal, port.TRType),
	}, nil
}

// ensureZvol creates the volume's zvol, or restores it from a snapshot.
func (b *nvmeBackend) ensureZvol(ctx context.Context, r backend.CreateRequest, p Params, rollback *[]func()) error {
	dsPath := r.ID.DatasetPath()
	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return fmt.Errorf("querying zvol %s: %w", dsPath, err)
	}
	if ds != nil {
		return nil
	}

	if r.SourceSnapshot != "" {
		if err := b.cloneZvol(ctx, r, rollback); err != nil {
			return err
		}
		// Recorded on this path too, from THIS request: a clone gets the
		// identity of the volume it becomes, never the one its origin carried.
		backend.RecordIdentity(ctx, b.c, dsPath, volume.IdentityFrom(r.Params))
		return nil
	}

	blocksize := p.VolBlockSize
	if blocksize == "" {
		if blocksize, err = b.c.RecommendedZvolBlocksize(ctx, r.ID.Pool); err != nil {
			return fmt.Errorf("asking for the recommended volblocksize: %w", err)
		}
	}
	if _, err := b.c.DatasetCreate(ctx, truenas.DatasetSpec{
		Name:         dsPath,
		Type:         "VOLUME",
		VolSize:      r.CapacityBytes,
		Sparse:       p.Sparse,
		VolBlockSize: blocksize,
		UserProperties: map[string]string{
			volume.OwnerProperty:    volume.OwnerValue,
			volume.ProtocolProperty: "nvme",
		},
	}); err != nil {
		return fmt.Errorf("creating zvol %s: %w", dsPath, err)
	}
	*rollback = append(*rollback, func() {
		_ = b.c.DatasetDelete(context.WithoutCancel(ctx), dsPath, true, true)
	})
	backend.RecordIdentity(ctx, b.c, dsPath, volume.IdentityFrom(r.Params))
	return nil
}

// cloneZvol restores a volume from a snapshot.
//
// A ZFS clone inherits NEITHER the ownership marker NOR any size stamp of its
// own. Both are therefore set explicitly here: an unstamped clone fails the
// delete guard forever, so every restored volume would leak.
func (b *nvmeBackend) cloneZvol(ctx context.Context, r backend.CreateRequest, rollback *[]func()) error {
	dsPath := r.ID.DatasetPath()
	if err := b.c.SnapshotClone(ctx, r.SourceSnapshot, dsPath); err != nil {
		return fmt.Errorf("cloning %s into %s: %w", r.SourceSnapshot, dsPath, err)
	}
	*rollback = append(*rollback, func() {
		_ = b.c.DatasetDelete(context.WithoutCancel(ctx), dsPath, true, true)
	})

	if err := b.c.SetUserProperty(ctx, dsPath, volume.OwnerProperty, volume.OwnerValue); err != nil {
		return fmt.Errorf("stamping the restored volume %s: %w", dsPath, err)
	}
	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return fmt.Errorf("querying the restored volume %s: %w", dsPath, err)
	}
	if ds == nil {
		return fmt.Errorf("%w: %s vanished after cloning", ErrVolumeNotFound, dsPath)
	}
	if r.CapacityBytes > 0 && ds.VolSize.Parsed < r.CapacityBytes {
		if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"volsize": r.CapacityBytes}); err != nil {
			return fmt.Errorf("sizing the restored volume %s: %w", dsPath, err)
		}
	}
	return nil
}

// Delete removes a volume, refusing anything this driver did not create.
//
// The ownership check runs FIRST and no destructive call is issued when it
// fails. The appliance holds ~20 TiB of live data whose datasets sit beside the
// driver's own; a volume handle is not evidence of ownership, and a marker
// inherited from the parent dataset is not either — only a LOCAL one is.
//
// Teardown then runs in the reverse of creation order, each step tolerating an
// object that is already gone, because DeleteVolume is retried after a partial
// failure and must converge.
func (b *nvmeBackend) Delete(ctx context.Context, id volume.ID) error {
	ctx = obs.WithVolume(ctx, id.String())
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return fmt.Errorf("querying zvol %s: %w", dsPath, err)
	}
	if ds != nil {
		if err := verifyOwned(ds); err != nil {
			return err
		}
	}

	name := subsystemName(id)
	subsys, err := b.c.NVMeSubsysByName(ctx, name)
	if err != nil {
		return fmt.Errorf("querying subsystem %s: %w", name, err)
	}
	if subsys != nil {
		bindings, err := b.c.NVMePortSubsysList(ctx, subsys.ID)
		if err != nil {
			return fmt.Errorf("listing port bindings of %s: %w", name, err)
		}
		for _, ps := range bindings {
			if err := b.c.NVMePortSubsysDelete(ctx, ps.ID); err != nil {
				return fmt.Errorf("unbinding subsystem %s from its port: %w", name, err)
			}
		}
		hosts, err := b.c.NVMeHostSubsysList(ctx, subsys.ID)
		if err != nil {
			return fmt.Errorf("listing host ACLs of %s: %w", name, err)
		}
		for _, hs := range hosts {
			if err := b.c.NVMeHostSubsysDelete(ctx, hs.ID); err != nil {
				return fmt.Errorf("revoking a host ACL on %s: %w", name, err)
			}
		}
	}

	// The namespace is removed even when the subsystem is already gone: it is
	// what holds the zvol open, and a leftover namespace makes the zvol
	// undeletable forever.
	ns, err := b.c.NVMeNamespaceByDevice(ctx, devicePath(id))
	if err != nil {
		return fmt.Errorf("querying namespace for %s: %w", dsPath, err)
	}
	if ns != nil {
		if err := b.c.NVMeNamespaceDelete(ctx, ns.ID); err != nil {
			return fmt.Errorf("deleting namespace for %s: %w", dsPath, err)
		}
	}
	if subsys != nil {
		if err := b.c.NVMeSubsysDelete(ctx, subsys.ID); err != nil {
			return fmt.Errorf("deleting subsystem %s: %w", name, err)
		}
	}
	// The port is appliance-wide and shared by every volume: it is never
	// deleted here.

	if ds == nil {
		return nil // already gone: DeleteVolume is idempotent by contract
	}
	// The namespace and its subsystem are gone; only the zvol is left. Dispose
	// destroys it, exactly as this line always did, unless delete protection is
	// on — in which case it is renamed into the graveyard instead. The rename
	// can only happen HERE, after the export is removed: pool.dataset.rename
	// performs no safety checks of its own.
	if err := retention.Dispose(ctx, b.c, b.retire, id, func(ctx context.Context) error {
		return deleteZvolWhenReleased(ctx, b.c, dsPath)
	}); err != nil {
		return fmt.Errorf("disposing of zvol %s: %w", dsPath, err)
	}
	obs.Logger(ctx).Info("deleted NVMe-oF volume", "dataset", dsPath, "subsystem", name)
	return nil
}

// zvolReleaseTimeout bounds how long a delete waits for the kernel to let go.
var zvolReleaseTimeout = 30 * time.Second

// deleteZvolWhenReleased retries a zvol delete while the appliance reports EBUSY.
//
// Removing the namespace does not immediately release the underlying zvol: the
// kernel target keeps the device open for a moment afterwards, and a delete
// issued in that window fails with "dataset is busy". This is the same
// behaviour observed for iSCSI extents against a real appliance, where the very
// next attempt succeeds. Retrying is correct here precisely because the object
// is ours and already unexported; giving up would leak a zvol on every delete.
func deleteZvolWhenReleased(ctx context.Context, c truenas.API, dsPath string) error {
	deadline := time.Now().Add(zvolReleaseTimeout)
	delay := 200 * time.Millisecond
	for {
		err := c.DatasetDelete(ctx, dsPath, true, true)
		if err == nil {
			return nil
		}
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

// isBusy reports whether the appliance refused because the dataset is still in
// use. The rule lives in internal/truenas so that the retire path, which faces
// the same post-teardown window, cannot answer it differently.
func isBusy(err error) bool { return truenas.IsBusy(err) }

// verifyOwned adapts the middleware's dataset to the ownership guard, which
// deliberately knows nothing about the client.
func verifyOwned(ds *truenas.Dataset) error {
	props := map[string]volume.Property{}
	for k, v := range ds.UserProperties {
		props[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	return volume.VerifyOwned(&volume.Dataset{ID: ds.ID, UserProperties: props})
}

// Expand grows a zvol. Shrink is refused here rather than at the middleware,
// which reports it in terms an operator cannot act on.
func (b *nvmeBackend) Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error) {
	ctx = obs.WithVolume(ctx, id.String())
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return 0, fmt.Errorf("querying zvol %s: %w", dsPath, err)
	}
	if ds == nil {
		return 0, fmt.Errorf("%w: %s", ErrVolumeNotFound, dsPath)
	}

	current := ds.VolSize.Parsed
	if bytes < current {
		return 0, fmt.Errorf("%w: %s is %d bytes, requested %d", ErrShrinkNotAllowed, dsPath, current, bytes)
	}
	if bytes == current {
		return current, nil
	}
	if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"volsize": bytes}); err != nil {
		return 0, fmt.Errorf("growing zvol %s: %w", dsPath, err)
	}
	return bytes, nil
}

// PublishContext resolves everything the node needs to attach the volume.
//
// It reads live state rather than remembering what Create returned, because the
// node may attach long after the controller that provisioned the volume died.
func (b *nvmeBackend) PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) {
	ctx = obs.WithVolume(ctx, id.String())
	name := subsystemName(id)

	subsys, err := b.c.NVMeSubsysByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("querying subsystem %s: %w", name, err)
	}
	if subsys == nil {
		return nil, fmt.Errorf("%w: subsystem %s", ErrVolumeNotFound, name)
	}

	bindings, err := b.c.NVMePortSubsysList(ctx, subsys.ID)
	if err != nil {
		return nil, fmt.Errorf("listing port bindings of %s: %w", name, err)
	}
	if len(bindings) == 0 {
		return nil, fmt.Errorf("%w: subsystem %s is bound to no port", ErrVolumeNotFound, name)
	}
	ports, err := b.c.NVMePortList(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying NVMe-oF ports: %w", err)
	}
	for _, ps := range bindings {
		for i := range ports {
			if ports[i].ID != ps.PortID.ID {
				continue
			}
			portal, err := portalAddress(b.c, &ports[i])
			if err != nil {
				return nil, err
			}
			return publishContext(subsys, portal, ports[i].TRType), nil
		}
	}
	return nil, fmt.Errorf("%w: no port serves subsystem %s", ErrVolumeNotFound, name)
}

// publishContext renders the node's attach parameters.
//
// The serial is the load-bearing value: the node resolves the device from
// /dev/disk/by-id by it rather than scanning or assuming an index. The nodes
// have their own local NVMe disks, so an index is never a device identity.
func publishContext(s *truenas.NVMeSubsystem, portal, trtype string) map[string]string {
	return map[string]string{
		"protocol":  Protocol,
		"portal":    portal,
		"nqn":       s.SubNQN,
		"serial":    s.Serial,
		"transport": strings.ToLower(trtype),
	}
}
