package node

// SMB staging, through the kernel's cifs client.
//
// An SMB volume, like an NFS one, needs nothing but a mount: the appliance owns
// the filesystem, its ACL and its refquota, so the node never formats, never
// grows and never resolves a device. What makes SMB different from every other
// protocol this driver speaks is that the mount AUTHENTICATES, and the
// credential has to reach mount.cifs without becoming visible to anything else
// on the node.
//
// The credential therefore travels in a file, never in an option:
//
//   - `mount -o password=…` puts the password in the argument vector, and
//     /proc/<pid>/cmdline is world-readable on Linux. Any pod with hostPID, any
//     unprivileged process on the node, and every `ps` captured into a log or a
//     support bundle would see it. That is not a theoretical exposure — it is
//     the documented reason mount.cifs offers a credentials file at all.
//   - The file lives under /run, a tmpfs: the password never reaches a disk, and
//     a reboot cannot leave one behind.
//   - It is created 0600, read once by mount.cifs, and removed immediately —
//     including on the failure path, which is why the removal is deferred rather
//     than written after the mount. The kernel keeps the credential in the
//     session it establishes, so nothing rereads the file once the mount
//     returns.
//
// The kernel keyring (cifscreds) is the other channel mount.cifs supports. It is
// not used here because it needs keyutils on the host and applies to multiuser
// mounts only, so it would add a second package to the node's requirements and
// still leave the single-user path to solve.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// smbCredentialsDir is where the short-lived credentials file is written, named
// as the HOST sees it: the node plugin drives the host's mount binaries chrooted
// into the host root, so the path handed to mount(8) must resolve inside that
// namespace, while this process writes it through Root.
//
// /run is a tmpfs on every distribution this driver supports, so the credential
// is never written to a disk.
const smbCredentialsDir = "/run/truenas-csi"

// smbCredentials is one SMB identity, as the node-stage Secret carries it.
type smbCredentials struct {
	username string
	password string
	domain   string
}

// stageSMB mounts //<server>/<share> at the staging path, once. It is safe to
// call repeatedly: when the host already reports the staging path as a mount
// point the call returns success without touching the host.
func (n *Node) stageSMB(ctx context.Context, req StageRequest) error {
	if err := n.pre.Require(CapSMB); err != nil {
		return err
	}
	server := req.PublishContext[KeyServer]
	share := req.PublishContext[KeyShare]
	if server == "" || share == "" {
		return fmt.Errorf("%w: smb volume needs %q and %q in the publish context",
			ErrInvalidRequest, KeyServer, KeyShare)
	}
	if req.StagingPath == "" {
		return fmt.Errorf("%w: no staging path", ErrInvalidRequest)
	}

	// Resolved before the idempotency check does any work, so a volume whose
	// Secret is missing fails with that reason rather than with a mount error.
	creds, err := smbCredentialsOf(req)
	if err != nil {
		return err
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

	credPath, remove, err := n.writeSMBCredentials(req.VolumeID, creds)
	// Deferred before the error is checked: writeSMBCredentials can fail after
	// creating the file, and the password must not survive that either.
	defer remove()
	if err != nil {
		return err
	}

	opts := []string{"credentials=" + credPath}
	opts = append(opts, smbOwnershipOptions(req.PublishContext)...)
	// The StorageClass's own mountOptions come last so an operator can override
	// anything the driver derived, exactly as the NFS path allows.
	opts = append(opts, req.VolumeCapability.MountFlags...)
	if req.VolumeCapability.Readonly {
		opts = append(opts, "ro")
	}
	// The SMB dialect is deliberately not pinned: mount.cifs negotiates the
	// highest one both ends speak and TrueNAS serves 3.x, so pinning here would
	// only ever hold the mount back. An operator who needs an older dialect asks
	// for it through mountOptions.
	return n.mount(ctx, "cifs", "//"+server+"/"+share, req.StagingPath, opts)
}

// unstageSMB is the teardown counterpart of stageSMB. The umount itself belongs
// to Unstage, which every protocol shares; what is left here is removing a
// credentials file that a stage killed between writing it and mounting could
// have stranded. Removing an absent file is a success, so a repeated
// NodeUnstageVolume — which the kubelet issues after a partial failure and after
// a node reboot — cannot fail a volume's teardown.
func (n *Node) unstageSMB(_ context.Context, req UnstageRequest) error {
	if protocolOf(req.PublishContext) != ProtocolSMB {
		return nil
	}
	if err := os.Remove(filepath.Join(n.hostRoot(), n.smbCredentialsPath(req.VolumeID))); err != nil &&
		!os.IsNotExist(err) {
		return fmt.Errorf("remove stale smb credentials for %s: %w", req.VolumeID, err)
	}
	return nil
}

// smbCredentialsOf reads the SMB identity from the node-stage secrets. The
// credential is ONLY ever accepted from there: the publish context is persisted
// verbatim in the PersistentVolume and readable by anyone who can read that
// object, which is why internal/backend/smb publishes a reference to the Secret
// and never its contents.
func smbCredentialsOf(req StageRequest) (smbCredentials, error) {
	c := smbCredentials{
		username: req.Secrets[KeySMBUsername],
		password: req.Secrets[KeySMBPassword],
		domain:   req.Secrets[KeySMBDomain],
	}
	if c.username == "" || c.password == "" {
		return smbCredentials{}, fmt.Errorf(
			"%w: smb volume needs %q and %q in its node-stage secret; the StorageClass names that "+
				"Secret with its secretName and secretNamespace parameters, which reach this node "+
				"as the %q and %q publish-context keys",
			ErrInvalidRequest, KeySMBUsername, KeySMBPassword,
			KeyNodeStageSecretName, KeyNodeStageSecretNamespace)
	}
	return c, nil
}

// smbOwnershipOptions renders the ownership the controller recorded for this
// volume as cifs mount options. For SMB this is genuinely mount-time: the cifs
// client maps uid/gid and the file and directory modes itself, the opposite of
// NFS where they must be set on the appliance.
func smbOwnershipOptions(pc map[string]string) []string {
	var opts []string
	for _, o := range []struct{ option, key string }{
		{"uid", KeyUID},
		{"gid", KeyGID},
		{"file_mode", KeyFileMode},
		{"dir_mode", KeyDirMode},
	} {
		if v := pc[o.key]; v != "" {
			opts = append(opts, o.option+"="+v)
		}
	}
	return opts
}

// smbCredentialsPath is the credentials file for one volume, as the HOST sees
// it. The name is a hash of the volume id rather than the id itself: a volume id
// is a slash-separated path (<backend>/<protocol>/<pool>/…/<name>) and would
// otherwise turn into a directory tree. The hash is stable, so a teardown can
// find and remove a file a crashed stage left behind.
func (n *Node) smbCredentialsPath(volumeID string) string {
	sum := sha256.Sum256([]byte(volumeID))
	return filepath.Join(smbCredentialsDir, hex.EncodeToString(sum[:16])+".cred")
}

// writeSMBCredentials places the credential where mount.cifs will read it and
// returns the path as the host sees it, together with the function that removes
// it. The caller must defer that function: it is the only thing keeping a failed
// mount from leaving a password on the node.
func (n *Node) writeSMBCredentials(volumeID string, c smbCredentials) (string, func(), error) {
	hostPath := n.smbCredentialsPath(volumeID)
	localPath := filepath.Join(n.hostRoot(), hostPath)
	remove := func() { _ = os.Remove(localPath) }

	if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
		return "", remove, fmt.Errorf("create %s on the host: %w", smbCredentialsDir, err)
	}
	// O_EXCL after an explicit remove, rather than O_TRUNC: opening an existing
	// file keeps whatever mode it already has, and a credential must never
	// inherit a permissive mode from a file this driver did not create.
	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return "", remove, fmt.Errorf("replace the smb credentials file: %w", err)
	}
	f, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", remove, fmt.Errorf("create the smb credentials file: %w", err)
	}

	var b strings.Builder
	b.WriteString("username=" + c.username + "\n")
	b.WriteString("password=" + c.password + "\n")
	if c.domain != "" {
		b.WriteString("domain=" + c.domain + "\n")
	}
	// Every error below is reported without the content that failed to be
	// written, so no failure path can put the credential into a log line.
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return "", remove, fmt.Errorf("write the smb credentials file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", remove, fmt.Errorf("close the smb credentials file: %w", err)
	}
	return hostPath, remove, nil
}
