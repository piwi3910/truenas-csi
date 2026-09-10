package node

// iSCSI attach.
//
// Two rules govern every line of this file, and both come from what is actually
// running on the target nodes:
//
//  1. worker-21 carries live Longhorn sessions and node records in the same
//     /etc/iscsi and /var/lib/iscsi this driver uses. So every iscsiadm invocation
//     is scoped with both -T <our iqn> and -p <our portal>. No --logoutall, no
//     unscoped -o delete, no global session rescan: any of those detach Longhorn's
//     volumes out from under running pods.
//  2. Device discovery is deterministic. The NAA the middleware returns from
//     iscsi.extent.create maps straight onto /dev/disk/by-id/scsi-3<naa>, so the
//     node constructs that path and waits for it to appear. It never enumerates
//     /dev — a scan races with Longhorn's own attach/detach on the same node.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/obs"
	"time"
)

// ErrDeviceNotFound is returned when the by-id link for a block volume — an
// iSCSI NAA or an NVMe subsystem serial — has not appeared within the resolve
// bound after a successful login or connect.
var ErrDeviceNotFound = errors.New("block device did not appear after attach")

// byIDDir is the host-absolute directory holding the stable device links.
const byIDDir = "/dev/disk/by-id"

// stageISCSI logs in to the target, resolves the device deterministically, and —
// for a filesystem volume — formats it if and only if it is blank, then mounts it.
// A raw block volume stops after the device is resolved: no format, no mount.
func (n *Node) stageISCSI(ctx context.Context, req StageRequest) error {
	if err := n.pre.Require(CapISCSI); err != nil {
		return err
	}
	fsType := fsTypeOf(req.VolumeCapability, req.PublishContext)
	if !req.VolumeCapability.Block {
		// Fail here, before anything touches the host, so the operator reads
		// "needs xfsprogs" rather than a mount(8) exit code.
		if err := n.requireFS(fsType); err != nil {
			return err
		}
	}

	portal := req.PublishContext[KeyPortal]
	iqn := req.PublishContext[KeyIQN]
	naa := req.PublishContext[KeyNAA]
	if portal == "" || iqn == "" || naa == "" {
		return fmt.Errorf("%w: iscsi volume needs %q, %q and %q in the publish context",
			ErrInvalidRequest, KeyPortal, KeyIQN, KeyNAA)
	}

	user, secret := chapCredentials(req)
	if err := iscsiLogin(ctx, n.exec, portal, iqn, user, secret); err != nil {
		return err
	}

	device, err := n.deviceFor(ctx, naa)
	if err != nil {
		return err
	}

	if req.VolumeCapability.Block {
		// volumeMode: Block. The pod gets the device itself at publish time;
		// there is nothing to stage beyond the login.
		return nil
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
	if err := n.formatIfBlank(ctx, device, fsType); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("create staging path %s: %w", req.StagingPath, err)
	}
	opts := append([]string{}, req.VolumeCapability.MountFlags...)
	if req.VolumeCapability.Readonly {
		opts = append(opts, "ro")
	}
	return n.mount(ctx, fsType, device, req.StagingPath, opts)
}

// unstageISCSI logs the node out of our target only. The publish context is not
// part of NodeUnstageVolume in the CSI spec, so when the CO does not supply it the
// session is left alone rather than guessed at: an unscoped logout is the one
// mistake that would take Longhorn's volumes down with ours.
func (n *Node) unstageISCSI(ctx context.Context, req UnstageRequest) error {
	if protocolOf(req.PublishContext) != ProtocolISCSI {
		return nil
	}
	portal, iqn := req.PublishContext[KeyPortal], req.PublishContext[KeyIQN]
	if portal == "" || iqn == "" {
		return nil
	}
	// The session is SHARED by every volume this driver has staged on the node,
	// because every volume is a LUN on one target. Logging out while a sibling
	// is still mounted takes that sibling's data path down with it.
	inUse, err := n.iscsiTargetInUse(portal, iqn)
	if err != nil {
		// Could not tell. Keeping a session costs one idle TCP connection;
		// ending one that is still carrying a volume costs that volume.
		obs.Logger(ctx).Warn("leaving the iSCSI session up: could not determine "+
			"whether other volumes still use this target",
			"portal", portal, "target", iqn, "error", obs.Redact(err.Error()))
		return nil
	}
	if inUse {
		obs.Logger(ctx).Info("leaving the iSCSI session up: other volumes on this "+
			"node are still mounted through the same shared target",
			"portal", portal, "target", iqn)
		return nil
	}
	return iscsiLogout(ctx, n.exec, portal, iqn)
}

// deviceFor resolves the NAA to the device the volume should be used through: the
// multipath mapper device when this node has multipath and the LUN is multipathed,
// and the single by-id path otherwise. A node without multipath-tools degrades to
// the single path with one warning rather than failing the attach.
func (n *Node) deviceFor(ctx context.Context, naa string) (string, error) {
	device, err := resolveDevice(n.hostRoot(), naa)
	if err != nil {
		return "", err
	}
	if !n.pre.Found[CapMultipath] {
		n.warnNoMultipath(ctx)
		return device, nil
	}
	if mapper, ok, err := multipathDevice(ctx, n.exec, naa); err == nil && ok {
		return mapper, nil
	}
	return device, nil
}

// chapCredentials reads CHAP from the node-stage secrets, falling back to the
// publish context. Secrets are the correct channel; the publish context is accepted
// because the controller may embed credentials there for a static PV.
func chapCredentials(req StageRequest) (user, secret string) {
	user, secret = req.Secrets[KeyCHAPUser], req.Secrets[KeyCHAPSecret]
	if user == "" {
		user = req.PublishContext[KeyCHAPUser]
	}
	if secret == "" {
		secret = req.PublishContext[KeyCHAPSecret]
	}
	return user, secret
}

// iscsiLogin discovers the portal and logs in to one target. CHAP node options are
// written before the login, because iscsid reads them from the node record at
// session setup and a login attempted first simply fails authentication.
//
// A login for a session that already exists is a success: the kubelet retries
// NodeStageVolume, and iscsiadm reports "already exists" rather than failing in a
// way that should fail the volume.
func iscsiLogin(ctx context.Context, e Executor, portal, iqn, chapUser, chapSecret string) error {
	if portal == "" || iqn == "" {
		return fmt.Errorf("%w: iscsi login needs both a portal and a target", ErrInvalidRequest)
	}
	// Discovery is the single mode with no target to name — the target list is
	// what it returns — so it is scoped by portal alone, and it only ever adds
	// node records.
	if _, err := e.Run(ctx, "iscsiadm", "-m", "discovery", "-t", "sendtargets", "-p", portal); err != nil {
		return fmt.Errorf("iscsi discovery on %s: %w", portal, err)
	}

	if chapUser != "" && chapSecret != "" {
		for _, kv := range [][2]string{
			{"node.session.auth.authmethod", "CHAP"},
			{"node.session.auth.username", chapUser},
			{"node.session.auth.password", chapSecret},
		} {
			if _, err := e.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal,
				"-o", "update", "-n", kv[0], "-v", kv[1]); err != nil {
				return fmt.Errorf("set CHAP on %s: %w", iqn, err)
			}
		}
	}

	if _, err := e.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "--login"); err != nil {
		if isAlreadyLoggedIn(err) {
			return nil
		}
		return fmt.Errorf("iscsi login to %s at %s: %w", iqn, portal, err)
	}
	return nil
}

// iscsiLogout ends our session and removes our node record, both scoped to the one
// target and portal. A session or record that is already gone is a success, so a
// retried teardown converges instead of failing.
func iscsiLogout(ctx context.Context, e Executor, portal, iqn string) error {
	if portal == "" || iqn == "" {
		return fmt.Errorf("%w: iscsi logout needs both a portal and a target", ErrInvalidRequest)
	}
	if _, err := e.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "--logout"); err != nil {
		if !isNoSuchSession(err) {
			return fmt.Errorf("iscsi logout from %s at %s: %w", iqn, portal, err)
		}
	}
	if _, err := e.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-o", "delete"); err != nil {
		if !isNoSuchSession(err) {
			return fmt.Errorf("delete iscsi node record %s at %s: %w", iqn, portal, err)
		}
	}
	return nil
}

// iscsiRescan asks the kernel to re-read the size of the LUNs on our session only.
// The unscoped form of this command rescans every session on the node, Longhorn's
// included, so the scoping here is not stylistic.
func iscsiRescan(ctx context.Context, e Executor, portal, iqn string) error {
	if portal == "" || iqn == "" {
		return fmt.Errorf("%w: iscsi rescan needs both a portal and a target", ErrInvalidRequest)
	}
	if _, err := e.Run(ctx, "iscsiadm", "-m", "node", "-T", iqn, "-p", portal, "-R"); err != nil {
		return fmt.Errorf("rescan %s at %s: %w", iqn, portal, err)
	}
	return nil
}

// isAlreadyLoggedIn recognises iscsiadm's report that the session it was asked to
// create is already there.
func isAlreadyLoggedIn(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already exists") || strings.Contains(s, "already present")
}

// isNoSuchSession recognises iscsiadm's report that what it was asked to remove is
// not there.
func isNoSuchSession(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no matching sessions") ||
		strings.Contains(s, "no session found") ||
		strings.Contains(s, "no records found") ||
		strings.Contains(s, "no matching node records")
}

// resolveDevice turns an extent's NAA into the stable device path the kernel
// publishes for it, and waits — up to deviceWaitTimeout — for udev to create the
// link after a login.
//
// It builds exactly one path and stats it. It deliberately does not list
// /dev/disk/by-id, /dev/disk/by-path, /sys/class/scsi_device or anything else:
// Longhorn attaches and detaches devices on these same nodes, so an enumeration
// can observe a device mid-teardown or attribute another driver's device to this
// volume. The NAA is unique and the mapping is deterministic, so there is nothing
// a scan could add.
//
// root is the host filesystem root as this process sees it; the returned path is
// host-absolute, because every command the node runs is executed in the host's
// mount namespace.
func resolveDevice(root, naa string) (string, error) {
	id := normalizeNAA(naa)
	if id == "" {
		return "", fmt.Errorf("%w: empty NAA", ErrInvalidRequest)
	}
	// The by-id link uses the NAA designator type as its prefix: NAA 6589cfc…
	// is published as scsi-36589cfc….
	name := "scsi-3" + id
	hostPath := byIDDir + "/" + name
	localPath := filepath.Join(root, "dev", "disk", "by-id", name)

	deadline := time.Now().Add(deviceWaitTimeout)
	for {
		if _, err := os.Lstat(localPath); err == nil {
			return hostPath, nil
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("%w: %s after %s", ErrDeviceNotFound, hostPath, deviceWaitTimeout)
		}
		time.Sleep(devicePollInterval)
	}
}

// normalizeNAA renders a NAA the way the by-id link spells it: lower case, no 0x.
func normalizeNAA(naa string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(strings.ToLower(naa)), "0x"))
}

// formatIfBlank creates a filesystem on device only when blkid POSITIVELY
// reports that it holds none.
//
// This check is the difference between a retried NodeStageVolume and a
// destroyed volume, so the rule is absolute: never mkfs a device that already
// carries a filesystem, and never mkfs a device whose state could not be
// established. "Could not ask" is not "blank".
func (n *Node) formatIfBlank(ctx context.Context, device, fsType string) error {
	blank, err := n.deviceIsBlank(ctx, device)
	if err != nil {
		return err
	}
	if !blank {
		return nil
	}
	if _, err := n.exec.Run(ctx, "mkfs."+fsType, device); err != nil {
		return fmt.Errorf("mkfs.%s on %s: %w", fsType, device, err)
	}
	return nil
}

// deviceIsBlank reports whether blkid said the device holds no filesystem, and
// errors when blkid could not answer at all.
//
// blkid exits 2 with NO output for a genuinely blank device, and that exit
// status is the only thing separating it from every other failure — a missing
// binary, a permission error, a busy device — which also produce no output.
// Treating an empty answer as "blank" therefore formatted devices nobody had
// established were empty, and blkid is not among the binaries preflight
// requires, so a host without it advertised ext4 and xfs and then reformatted
// every volume staged on it.
//
// Guessing "there is a filesystem" costs a failed stage the CO retries.
// Guessing the other way costs the data, so only a positive exit-2 answer is
// allowed to mean blank.
func (n *Node) deviceIsBlank(ctx context.Context, device string) (bool, error) {
	out, err := n.exec.Run(ctx, "blkid", "-p", "-s", "TYPE", "-o", "value", device)
	if err == nil {
		return strings.TrimSpace(string(out)) == "", nil
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) && coded.ExitCode() == blkidNothingFound &&
		strings.TrimSpace(string(out)) == "" {
		return true, nil
	}
	return false, fmt.Errorf(
		"refusing to stage %s: blkid could not establish whether it already holds a "+
			"filesystem, and formatting a device that does would destroy it. Ensure blkid "+
			"(util-linux) is installed on this node: %w", device, err)
}

// blkidNothingFound is blkid's exit status for "the device holds nothing I
// recognise", which is the only answer that licenses mkfs.
const blkidNothingFound = 2
