// Package iscsi provisions block volumes on TrueNAS: one zvol per volume,
// exposed as an extent and mapped as a LUN on a single shared target.
//
// The shared target is the defining decision of this package. Initiator ACLs
// on TrueNAS belong to the target, not to individual LUNs, so every node logged
// into the target sees every LUN the driver has published. That is an accepted
// risk recorded in the spec: all cluster nodes are equally trusted, the
// initiator ACL defends the cluster boundary rather than one node from another,
// and RWO is enforced by Kubernetes above this layer. Nothing in this package
// should be "improved" into a target per volume without revisiting that
// decision, because it changes the security model in both directions.
package iscsi

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
const Protocol = "iscsi"

// maxExtentName is the appliance's ceiling on iscsi.extent.name. A longer name
// is rejected outright, so the driver derives one that fits rather than letting
// a long PVC name make a volume unprovisionable.
const maxExtentName = 64

// ErrShrinkNotAllowed is returned for an expansion request smaller than the
// current size. The middleware refuses a zvol shrink too, but its message says
// nothing an operator can act on, so the driver refuses first.
var ErrShrinkNotAllowed = errors.New("volume cannot be shrunk")

// ErrVolumeNotFound is returned when an operation names a zvol that is not there.
var ErrVolumeNotFound = errors.New("volume does not exist")

func init() { backend.Register(Protocol, New) }

// iscsiBackend provisions iSCSI volumes on one appliance.
type iscsiBackend struct {
	c      truenas.API
	pool   string
	parent string
	// retire is the delete-protection policy. Its zero value is "off", which is
	// the default and takes exactly the destroy path this driver always took.
	retire retention.Policy
}

// New builds the backend for one appliance. It matches backend.Factory.
func New(c truenas.API, opts backend.Options) backend.Backend {
	return &iscsiBackend{c: c, pool: opts.Pool, parent: opts.Parent, retire: opts.Retention}
}

// Protocol implements backend.Backend.
func (b *iscsiBackend) Protocol() string { return Protocol }

// Params is the StorageClass configuration this backend understands.
type Params struct {
	Pool   string
	Parent string

	// PortalID names an existing portal. When set the driver creates none.
	PortalID int
	// CHAP defaults to on: an unauthenticated target on a shared network is
	// reachable by anything that can route to it.
	CHAP bool
	// InitiatorACL defaults to on and restricts the target to NodeIQNs.
	InitiatorACL bool
	// NodeIQNs are the cluster's node initiator names.
	NodeIQNs []string

	Sparse       bool
	VolBlockSize string
}

// parseParams applies the documented defaults. Booleans default to the safe
// value — CHAP and the initiator ACL are ON unless the operator opts out.
func parseParams(pool, parent string, m map[string]string) (Params, error) {
	p := Params{Pool: pool, Parent: parent, CHAP: true, InitiatorACL: true, Sparse: true}

	var err error
	if p.CHAP, err = boolParam(m, "chap", true); err != nil {
		return Params{}, err
	}
	if p.InitiatorACL, err = boolParam(m, "initiatorACL", true); err != nil {
		return Params{}, err
	}
	if p.Sparse, err = boolParam(m, "sparse", true); err != nil {
		return Params{}, err
	}
	if v := strings.TrimSpace(m["portalID"]); v != "" {
		id, convErr := strconv.Atoi(v)
		if convErr != nil || id <= 0 {
			return Params{}, fmt.Errorf("storage class parameter portalID must be a positive integer, got %q", v)
		}
		p.PortalID = id
	}
	p.VolBlockSize = strings.TrimSpace(m["volblocksize"])
	for _, iqn := range strings.Split(m["nodeIQNs"], ",") {
		if iqn = strings.TrimSpace(iqn); iqn != "" {
			p.NodeIQNs = append(p.NodeIQNs, iqn)
		}
	}
	return p, nil
}

func boolParam(m map[string]string, key string, def bool) (bool, error) {
	v := strings.TrimSpace(m[key])
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("storage class parameter %s must be true or false, got %q", key, v)
	}
	return b, nil
}

// extentName derives an extent name that fits the appliance's 64-character
// limit while staying unique and deterministic.
//
// Truncation alone would be a data-corruption bug rather than a cosmetic one:
// two PVC names sharing a long prefix would collapse onto ONE extent, so two
// volumes would address the same zvol. The hash suffix is what keeps distinct
// volumes distinct.
func extentName(id volume.ID) string {
	safe := sanitizeExtent(id.Name)
	if len(safe) <= maxExtentName {
		return safe
	}
	sum := sha256.Sum256([]byte(id.Name))
	suffix := "-" + hex.EncodeToString(sum[:])[:12]
	return safe[:maxExtentName-len(suffix)] + suffix
}

func sanitizeExtent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '.', r == '_', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Create provisions a zvol, publishes it as an extent and maps it as a LUN on
// the shared target.
//
// Every step is query-then-act, so a retried CreateVolume converges on what is
// already there. Anything this call creates is torn down in reverse order if a
// later step fails: a zvol with no extent is invisible to Kubernetes, which
// means nothing will ever delete it.
func (b *iscsiBackend) Create(ctx context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	ctx = obs.WithVolume(ctx, r.ID.String())
	p, err := parseParams(b.pool, b.parent, r.Params)
	if err != nil {
		return nil, err
	}
	dsPath := r.ID.DatasetPath()

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

	name := extentName(r.ID)
	extent, err := b.c.ExtentByName(ctx, name)
	if err != nil {
		undo()
		return nil, fmt.Errorf("querying extent %s: %w", name, err)
	}
	if extent == nil {
		extent, err = b.c.ExtentCreate(ctx, name, dsPath)
		if err != nil {
			undo()
			return nil, fmt.Errorf("creating extent %s: %w", name, err)
		}
		id := extent.ID
		rollback = append(rollback, func() { _ = b.c.ExtentDelete(context.WithoutCancel(ctx), id) })
	}

	// The target, its portal, its CHAP credential and its initiator group are
	// still created here: they are appliance-wide objects every volume shares.
	// The LUN MAPPING is not. It moved to ControllerPublishVolume, because that
	// mapping is the fence — every node logged into the shared target can see
	// every LUN on it, so a volume mapped from the moment it is provisioned is
	// reachable by the whole cluster whether or not anything has attached it.
	_, iqn, err := ensureTarget(ctx, b.c, p)
	if err != nil {
		undo()
		return nil, err
	}

	pc, err := b.publishContext(ctx, p, iqn, extent.NAA, unmappedLUN)
	if err != nil {
		undo()
		return nil, err
	}

	return &backend.Volume{ID: r.ID, CapacityBytes: r.CapacityBytes, Context: pc}, nil
}

// ensureZvol creates the volume's zvol, or restores it from a snapshot.
func (b *iscsiBackend) ensureZvol(ctx context.Context, r backend.CreateRequest, p Params, rollback *[]func()) error {
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
			volume.ProtocolProperty: "iscsi",
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
func (b *iscsiBackend) cloneZvol(ctx context.Context, r backend.CreateRequest, rollback *[]func()) error {
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
func (b *iscsiBackend) Delete(ctx context.Context, id volume.ID) error {
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

	// The extent and its mapping are removed even when the zvol is already
	// gone: a half-deleted volume otherwise holds a LUN id on the shared
	// target forever.
	name := extentName(id)
	extent, err := b.c.ExtentByName(ctx, name)
	if err != nil {
		return fmt.Errorf("querying extent %s: %w", name, err)
	}
	if extent != nil {
		target, err := queryTarget(ctx, b.c, targetName(b.pool, b.parent))
		if err != nil {
			return err
		}
		if target != nil {
			mappings, err := b.c.TargetExtentList(ctx, target.ID)
			if err != nil {
				return fmt.Errorf("listing LUN mappings: %w", err)
			}
			for _, m := range mappings {
				if m.Extent != extent.ID {
					continue
				}
				if err := b.c.TargetExtentDelete(ctx, m.ID); err != nil {
					return fmt.Errorf("removing LUN %d: %w", m.LUNID, err)
				}
				releaseLUN(target.ID, m.LUNID)
			}
		}
		if err := b.c.ExtentDelete(ctx, extent.ID); err != nil {
			return fmt.Errorf("deleting extent %s: %w", name, err)
		}
	}

	if ds == nil {
		return nil // already gone: DeleteVolume is idempotent by contract
	}
	// The extent and its target mapping are gone; only the zvol is left. Dispose
	// destroys it, exactly as this line always did, unless delete protection is
	// on — in which case it is renamed into the graveyard instead. The rename
	// can only happen HERE, after the extent is removed: pool.dataset.rename
	// performs no safety checks and the middleware's own documentation names
	// iSCSI as a service a rename can disrupt.
	if err := retention.Dispose(ctx, b.c, b.retire, id, func(ctx context.Context) error {
		return deleteZvolWhenReleased(ctx, b.c, dsPath)
	}); err != nil {
		return fmt.Errorf("disposing of zvol %s: %w", dsPath, err)
	}
	obs.Logger(ctx).Info("deleted iSCSI volume", "dataset", dsPath)
	return nil
}

// zvolReleaseTimeout bounds how long a delete waits for the kernel to let go.
var zvolReleaseTimeout = 30 * time.Second

// deleteZvolWhenReleased retries a zvol delete while the appliance reports EBUSY.
//
// Removing an extent does not immediately release the underlying zvol: the
// kernel target keeps the device open for a moment afterwards, and a delete
// issued in that window fails with "dataset is busy". Observed against a real
// appliance, where the very next attempt succeeds. Retrying is correct here
// precisely because the object is ours and already unpublished; giving up would
// leak a zvol on every iSCSI volume deletion.
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
func (b *iscsiBackend) Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error) {
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
func (b *iscsiBackend) PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) {
	ctx = obs.WithVolume(ctx, id.String())
	name := targetName(b.pool, b.parent)

	target, err := queryTarget(ctx, b.c, name)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("shared target %s does not exist", name)
	}
	iqn, err := targetIQN(ctx, b.c, name)
	if err != nil {
		return nil, err
	}

	extentName := extentName(id)
	extent, err := b.c.ExtentByName(ctx, extentName)
	if err != nil {
		return nil, fmt.Errorf("querying extent %s: %w", extentName, err)
	}
	if extent == nil {
		return nil, fmt.Errorf("%w: extent %s", ErrVolumeNotFound, extentName)
	}

	mappings, err := b.c.TargetExtentList(ctx, target.ID)
	if err != nil {
		return nil, fmt.Errorf("listing LUN mappings: %w", err)
	}
	lun := -1
	for _, m := range mappings {
		if m.Extent == extent.ID {
			lun = m.LUNID
		}
	}
	if lun < 0 {
		return nil, fmt.Errorf("%w: extent %s is not mapped to %s", ErrVolumeNotFound, extentName, name)
	}

	p := Params{Pool: b.pool, Parent: b.parent}
	if len(target.Groups) > 0 {
		p.PortalID = target.Groups[0].Portal
		p.CHAP = strings.EqualFold(target.Groups[0].AuthMethod, "CHAP")
	}
	return b.publishContext(ctx, p, iqn, extent.NAA, lun)
}

// publishContext renders the node's attach parameters.
//
// The NAA is the load-bearing value: the node resolves the device from
// /dev/disk/by-id by it rather than scanning, which is the only way to stay
// correct on a node whose iSCSI stack is shared with another driver.
func (b *iscsiBackend) publishContext(ctx context.Context, p Params, iqn, naa string, lun int) (map[string]string, error) {
	portalID, err := ensurePortal(ctx, b.c, p)
	if err != nil {
		return nil, err
	}
	addr, err := portalAddress(ctx, b.c, portalID)
	if err != nil {
		return nil, err
	}

	pc := map[string]string{
		"portal": addr,
		"iqn":    iqn,
		"naa":    naa,
	}
	// An unmapped volume reports no LUN at all rather than a plausible-looking
	// zero: the node resolves its device by NAA, and a LUN number for a mapping
	// that does not exist is worse than its absence.
	if lun != unmappedLUN {
		pc["lun"] = strconv.Itoa(lun)
	}
	if p.CHAP {
		auth, err := ensureCHAP(ctx, b.c, targetName(p.Pool, p.Parent))
		if err != nil {
			return nil, err
		}
		if auth != nil {
			pc["chapUser"] = auth.User
			pc["chapSecret"] = auth.Secret
			// The tag is where the node reads the credential back from on the
			// appliance, so CHAP needs no Kubernetes Secret of its own.
			pc["chapSecretRef"] = strconv.Itoa(auth.Tag)
		}
	}
	return pc, nil
}
