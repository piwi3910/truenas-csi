// Package smb provisions SMB volumes: one ZFS dataset per volume, created with
// share_type SMB, quota'd with refquota, stamped as driver-owned, given an
// NFSv4 ACL, and exported as an SMB share.
//
// Every rule enforced here was verified against a live TrueNAS 25.10.6:
//   - `share_type: "SMB"` yields mode 0770 WITH an NFSv4 ACL (filesystem.stat
//     reports acl:true), where a plain dataset is 0755 with no ACL. The two file
//     backends therefore do NOT share a permissions path: SMB needs
//     filesystem.setacl, and filesystem.setperm would strip the ACL instead of
//     configuring it;
//   - without refquota the share reports the WHOLE POOL (31T observed for a 10Gi
//     volume), so capacity is meaningless to the pod and one volume can eat the
//     pool;
//   - the middleware SILENTLY PERMITS a refquota shrink (unlike a zvol volsize
//     shrink, which it refuses), so the shrink guard cannot be delegated;
//   - a ZFS clone inherits NEITHER the ownership marker NOR refquota, so a
//     restored volume must be stamped and quota'd explicitly or it leaks forever;
//   - SMB ownership is a MOUNT-TIME concern. The cifs client maps uid/gid and
//     file/dir modes itself via mount options, the exact opposite of NFS where
//     they must be set on the appliance. So uid/gid/fileMode/dirMode travel in
//     the publish context for the node to apply, while the ACL on the appliance
//     governs what the SMB user may do.
//
// Credentials never travel in the publish context. An SMB mount needs a real
// TrueNAS user, and the node reads its username and password from a Kubernetes
// Secret; this package passes only a REFERENCE to that Secret, so no password
// reaches the PersistentVolume, the API server's volume attributes, or a log.
package smb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// Protocol is the StorageClass "protocol" value this backend serves.
const Protocol = "smb"

// StorageClass parameter names.
const (
	// ParamServer is the address the node mounts from (//<server>/<share>). It
	// is a deployment fact the middleware cannot answer for us; when it is
	// unset the appliance's own endpoint host is used.
	ParamServer = "server"

	// ParamShareName is a PREFIX for the generated share name, not a literal
	// name: every volume in a StorageClass needs its own share, so a fixed name
	// would collide. The volume name is appended and the result truncated with
	// a hash suffix to fit the 80-character limit.
	ParamShareName = "shareName"

	// ParamSecretName / ParamSecretNamespace name the Kubernetes Secret holding
	// the SMB username and password. Only the reference is published; the
	// controller never reads, stores or logs the credential itself.
	ParamSecretName      = "secretName"
	ParamSecretNamespace = "secretNamespace"

	// ParamUID / ParamGID are the owner the cifs client presents to the pod.
	// For SMB this is a mount option, not an on-disk change.
	ParamUID = "uid"
	ParamGID = "gid"

	// ParamFileMode / ParamDirMode are the cifs client's file_mode and dir_mode
	// mount options — again mount-time, applied by the node.
	ParamFileMode = "fileMode"
	ParamDirMode  = "dirMode"

	// ParamMode is the POSIX-equivalent mode expressed as the dataset's NFSv4
	// ACL on the appliance. It governs what the SMB user may actually do,
	// whereas fileMode/dirMode only govern what the pod is shown.
	ParamMode = "mode"
)

// Publish-context keys for the Secret reference. They follow the CSI external
// provisioner's node-stage-secret convention so the node plugin resolves the
// credential itself and the password never crosses this process.
const (
	nodeStageSecretNameKey      = "csi.storage.k8s.io/node-stage-secret-name"
	nodeStageSecretNamespaceKey = "csi.storage.k8s.io/node-stage-secret-namespace"
)

// Defaults for the StorageClass parameters.
const (
	// defaultMode grants everyone@ as well as owner and group.
	//
	// share_type SMB produces 0770, which grants only the owning user and group
	// — both root. An SMB client authenticates as an ordinary user, so a 0770
	// ACL denies it outright: mounting gives "mount error(13): Permission
	// denied", verified against a real appliance. The alternative to being
	// permissive here is a share nobody can use, so the default matches the NFS
	// backend's and operators tighten it with the `mode` parameter.
	defaultMode = "0777"
	// defaultFileMode / defaultDirMode are what the cifs client reported on the
	// validated mount (file_mode=0755,dir_mode=0755).
	defaultFileMode = "0755"
	defaultDirMode  = "0755"
)

// maxShareName is the middleware's cap on an SMB share name.
const maxShareName = 80

func init() { backend.Register(Protocol, New) }

// Backend provisions SMB volumes on one appliance.
type Backend struct {
	c      truenas.API
	pool   string
	parent string

	// mu guards the caches below. They are conveniences, never a source of
	// truth: a controller restart empties them, and every value they hold has a
	// defaulted fallback or is recorded in the PersistentVolume's own volume
	// context, so nothing that matters is lost.
	mu     sync.Mutex
	server string
	byVol  map[string]params
}

// New builds an SMB backend bound to one appliance, pool and parent dataset.
func New(c truenas.API, pool, parent string) backend.Backend {
	return &Backend{c: c, pool: pool, parent: parent, byVol: map[string]params{}}
}

// Protocol implements backend.Backend.
func (b *Backend) Protocol() string { return Protocol }

// params is the resolved StorageClass configuration for one request.
type params struct {
	server          string
	sharePrefix     string
	secretName      string
	secretNamespace string
	uid             int
	gid             int
	fileMode        string
	dirMode         string
	mode            string
}

func defaultParams() params {
	return params{mode: defaultMode, fileMode: defaultFileMode, dirMode: defaultDirMode}
}

func parseParams(p map[string]string) (params, error) {
	out := defaultParams()
	get := func(k string) string { return strings.TrimSpace(p[k]) }

	out.server = get(ParamServer)
	out.sharePrefix = get(ParamShareName)
	out.secretName = get(ParamSecretName)
	out.secretNamespace = get(ParamSecretNamespace)

	for _, f := range []struct {
		key string
		dst *string
	}{{ParamMode, &out.mode}, {ParamFileMode, &out.fileMode}, {ParamDirMode, &out.dirMode}} {
		v := get(f.key)
		if v == "" {
			continue
		}
		if _, err := strconv.ParseUint(v, 8, 32); err != nil {
			return params{}, status.Errorf(codes.InvalidArgument, "%s=%q is not an octal mode", f.key, v)
		}
		*f.dst = v
	}
	for _, f := range []struct {
		key string
		dst *int
	}{{ParamUID, &out.uid}, {ParamGID, &out.gid}} {
		v := get(f.key)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return params{}, status.Errorf(codes.InvalidArgument, "%s=%q is not a valid id", f.key, v)
		}
		*f.dst = n
	}
	return out, nil
}

// shareName derives an SMB share name that fits the appliance's 80-character
// limit while staying unique and deterministic.
//
// Truncation alone would be a data-corruption bug rather than a cosmetic one:
// two volume names sharing a long prefix would collapse onto ONE share, so two
// volumes would serve the same data under the same name. The hash suffix —
// taken from the FULL name, not the truncated one — is what keeps distinct
// volumes distinct.
func shareName(id volume.ID, prefix string) string {
	full := id.Name
	if prefix != "" {
		full = prefix + "-" + full
	}
	safe := sanitizeShare(full)
	if len(safe) <= maxShareName && safe != "" {
		return safe
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "-" + hex.EncodeToString(sum[:])[:12]
	if len(safe) > maxShareName-len(suffix) {
		safe = safe[:maxShareName-len(suffix)]
	}
	return safe + suffix
}

// sanitizeShare removes the characters SMB forbids in a share name. Everything
// outside the safe set becomes '-', which is lossy — hence the hash suffix above
// whenever the name is not already short and clean.
func sanitizeShare(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// mountpointOf prefers what the appliance reports and falls back to the
// conventional /mnt/<dataset> layout, which is what the live box uses.
func mountpointOf(ds *truenas.Dataset, dsPath string) string {
	if ds != nil && ds.Mountpoint != "" {
		return ds.Mountpoint
	}
	return "/mnt/" + dsPath
}

// dacl renders an octal mode as an NFSv4 ACL.
//
// An SMB dataset has an ACL, not a mode, so the StorageClass's `mode` has to be
// expressed as one. Each octal digit maps to a basic permission preset for the
// matching principal; the entries are inheritable so files and directories the
// workload creates get the same access.
func dacl(mode string) []truenas.ACLEntry {
	digits := mode
	if len(digits) > 3 {
		digits = digits[len(digits)-3:]
	}
	for len(digits) < 3 {
		digits = "0" + digits
	}
	tags := []string{truenas.ACLTagOwner, truenas.ACLTagGroup, truenas.ACLTagEveryone}
	out := make([]truenas.ACLEntry, 0, len(tags))
	for i, tag := range tags {
		// A principal granted nothing gets NO entry. There is no basic preset
		// meaning "none": sending one makes the middleware fall through to the
		// advanced-permission schema and reject the whole ACL with
		// "NFS4ACE_AdvancedPerms.BASIC: Extra inputs are not permitted".
		// Absence is how NFSv4 expresses no access anyway.
		if digits[i] == '0' {
			continue
		}
		out = append(out, truenas.AllowEntry(tag, permFor(digits[i]), truenas.ACLFlagInherit))
	}
	return out
}

// permFor maps one octal digit onto the middleware's basic permission presets.
func permFor(d byte) string {
	switch d {
	case '7':
		return truenas.ACLPermFullControl
	case '6', '3', '2':
		return truenas.ACLPermModify
	case '5', '4':
		return truenas.ACLPermRead
	case '1':
		return truenas.ACLPermTraverse
	default:
		return truenas.ACLPermNone
	}
}

// Create provisions a volume, or returns the existing one unchanged.
//
// When r.SourceSnapshot is set the volume is restored by cloning that snapshot;
// the clone is then explicitly stamped, quota'd and given its ACL, because it
// inherits none of those from its origin.
func (b *Backend) Create(ctx context.Context, r backend.CreateRequest) (*backend.Volume, error) {
	if err := volume.Confine(r.ID, b.pool, b.parent); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if r.CapacityBytes <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "capacity must be positive, got %d", r.CapacityBytes)
	}
	p, err := parseParams(r.Params)
	if err != nil {
		return nil, err
	}
	b.remember(r.ID, p)

	dsPath := r.ID.DatasetPath()
	name := shareName(r.ID, p.sharePrefix)

	existing, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if existing != nil {
		// CSI requires an identical repeat to succeed and a conflicting one to
		// be reported, not silently reconciled.
		if existing.RefQuota.Parsed != r.CapacityBytes {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %s already exists with refquota %d bytes, requested %d",
				r.ID, existing.RefQuota.Parsed, r.CapacityBytes)
		}
		if err := b.ensureShare(ctx, mountpointOf(existing, dsPath), name); err != nil {
			return nil, err
		}
		return b.volumeFor(r.ID, r.CapacityBytes, name, p), nil
	}

	ds, err := b.provision(ctx, dsPath, r)
	if err != nil {
		return nil, err
	}
	mountpoint := mountpointOf(ds, dsPath)

	// Everything past this point is rolled back on failure: a dataset with no
	// share is an orphan no retry can reconcile.
	rollback := func(cause error) error {
		if delErr := b.c.DatasetDelete(ctx, dsPath, true, true); delErr != nil {
			return status.Errorf(codes.Internal,
				"%v; rolling back dataset %s also failed: %v — it must be removed by hand",
				cause, dsPath, delErr)
		}
		return cause
	}

	// The ACL goes on BEFORE the share is published: a share exported while the
	// dataset still carries its birth ACL is briefly reachable with permissions
	// the StorageClass did not ask for.
	if err := b.c.SetACL(ctx, mountpoint, dacl(p.mode), p.uid, p.gid); err != nil {
		return nil, rollback(status.Errorf(codes.Internal, "set ACL on %s: %v", mountpoint, err))
	}
	if err := b.ensureShare(ctx, mountpoint, name); err != nil {
		return nil, rollback(err)
	}
	return b.volumeFor(r.ID, r.CapacityBytes, name, p), nil
}

// provision creates the dataset itself, either empty or as a clone of a
// snapshot, and returns it already carrying the marker and quota.
func (b *Backend) provision(ctx context.Context, dsPath string, r backend.CreateRequest) (*truenas.Dataset, error) {
	ds, err := b.create(ctx, dsPath, r)
	if err != nil {
		return nil, err
	}
	// Recorded once, at birth, on both paths: a clone gets the identity of the
	// request that created it, never the one its origin carried.
	backend.RecordIdentity(ctx, b.c, dsPath, volume.IdentityFrom(r.Params))
	return ds, nil
}

// create makes the dataset, empty or cloned, already carrying the marker and
// the quota.
func (b *Backend) create(ctx context.Context, dsPath string, r backend.CreateRequest) (*truenas.Dataset, error) {
	if r.SourceSnapshot == "" {
		ds, err := b.c.DatasetCreate(ctx, truenas.DatasetSpec{
			Name:     dsPath,
			Type:     "FILESYSTEM",
			RefQuota: r.CapacityBytes,
			// share_type SMB is what gives the dataset mode 0770 and an NFSv4
			// ACL; without it the dataset is a plain 0755 with no ACL and the
			// setacl below would be converting rather than configuring.
			ShareType: "SMB",
			UserProperties: map[string]string{
				volume.OwnerProperty:    volume.OwnerValue,
				volume.ProtocolProperty: "smb",
			},
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create dataset %s: %v", dsPath, err)
		}
		return ds, nil
	}
	return b.restore(ctx, r.SourceSnapshot, dsPath, r.CapacityBytes)
}

// restore clones a snapshot and repairs everything the clone did not inherit.
//
// A ZFS clone takes its properties from its position in the hierarchy, not from
// its origin: without the explicit stamp the delete guard would refuse to remove
// the volume forever, and without refquota the share would report the whole pool.
func (b *Backend) restore(ctx context.Context, snapshot, dsPath string, bytes int64) (*truenas.Dataset, error) {
	if err := b.c.SnapshotClone(ctx, snapshot, dsPath); err != nil {
		return nil, status.Errorf(codes.Internal, "clone snapshot %s into %s: %v", snapshot, dsPath, err)
	}
	fail := func(format string, args ...any) (*truenas.Dataset, error) {
		cause := status.Errorf(codes.Internal, format, args...)
		if delErr := b.c.DatasetDelete(ctx, dsPath, true, true); delErr != nil {
			return nil, status.Errorf(codes.Internal,
				"%v; removing the unmarked clone %s also failed: %v — it must be removed by hand",
				cause, dsPath, delErr)
		}
		return nil, cause
	}
	if err := b.c.SetUserProperty(ctx, dsPath, volume.OwnerProperty, volume.OwnerValue); err != nil {
		return fail("stamp ownership on clone %s: %v", dsPath, err)
	}
	if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"refquota": bytes}); err != nil {
		return fail("set refquota on clone %s: %v", dsPath, err)
	}
	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return fail("query clone %s: %v", dsPath, err)
	}
	return ds, nil
}

// ensureShare publishes the mountpoint, tolerating a share that already exists.
//
// The share is created FENCED — hostsdeny=["ALL"] with an empty hostsallow —
// so a volume nobody has published yet is reachable by nobody. Creating it open
// and narrowing it afterwards would leave it world-reachable for as long as the
// second call takes, and forever if the controller died in between.
func (b *Backend) ensureShare(ctx context.Context, mountpoint, name string) error {
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return status.Errorf(codes.Internal, "query SMB share for %s: %v", mountpoint, err)
	}
	if share != nil {
		return nil
	}
	if _, err := b.createShare(ctx, mountpoint, name, nil); err != nil {
		return status.Errorf(codes.Internal, "create SMB share %s for %s: %v", name, mountpoint, err)
	}
	return nil
}

// createShare publishes a path with its access lists already in place.
func (b *Backend) createShare(ctx context.Context, mountpoint, name string, allow []string) (*smbShare, error) {
	var out smbShare
	err := b.c.CallJSON(ctx, &out, "sharing.smb.create", map[string]any{
		"path":    mountpoint,
		"name":    name,
		"comment": "truenas-csi",
		"options": accessOptions(nil, allow),
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// isNotFound is truenas.IsNotFound, named locally so the access path reads the
// same whether it is talking about a share or a dataset.
func isNotFound(err error) bool { return truenas.IsNotFound(err) }

// Delete removes a volume, refusing anything this driver did not create.
//
// Ownership is verified FIRST, before any destructive call: the pool holds live
// operator data, and a share deleted before the guard runs is damage the guard
// can no longer prevent.
func (b *Backend) Delete(ctx context.Context, id volume.ID) error {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil // already gone: CSI delete is idempotent
	}
	if err := verifyOwned(ds); err != nil {
		return err
	}

	mountpoint := mountpointOf(ds, dsPath)
	share, err := b.shareByPath(ctx, mountpoint)
	if err != nil {
		return status.Errorf(codes.Internal, "query SMB share for %s: %v", mountpoint, err)
	}
	if share != nil {
		if err := b.c.CallJSON(ctx, nil, "sharing.smb.delete", share.ID); err != nil && !truenas.IsNotFound(err) {
			return status.Errorf(codes.Internal, "delete SMB share %d: %v", share.ID, err)
		}
	}
	if err := b.c.DatasetDelete(ctx, dsPath, true, false); err != nil {
		return status.Errorf(codes.Internal, "delete dataset %s: %v", dsPath, err)
	}
	b.forget(id)
	return nil
}

// verifyOwned runs the ownership guard against a middleware dataset.
//
// The check is delegated to internal/volume so there is exactly one
// implementation of the rule, and so the refusal carries the reason — missing,
// wrong value, or merely INHERITED from the parent.
func verifyOwned(ds *truenas.Dataset) error {
	view := &volume.Dataset{ID: ds.ID, UserProperties: map[string]volume.Property{}}
	for k, v := range ds.UserProperties {
		view.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	return volume.VerifyOwned(view)
}

// Expand grows the volume's refquota.
//
// The shrink guard lives here because it cannot live anywhere else: the
// middleware silently accepts a refquota below current usage.
func (b *Backend) Expand(ctx context.Context, id volume.ID, bytes int64) (int64, error) {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return 0, status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return 0, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return 0, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}
	if err := verifyOwned(ds); err != nil {
		return 0, err
	}

	current := ds.RefQuota.Parsed
	if bytes < current {
		return 0, status.Errorf(codes.InvalidArgument,
			"cannot shrink volume %s from %d to %d bytes: TrueNAS would accept the smaller "+
				"refquota even below current usage, so the driver refuses it", id, current, bytes)
	}
	if bytes == current {
		return current, nil
	}
	if _, err := b.c.DatasetUpdate(ctx, dsPath, map[string]any{"refquota": bytes}); err != nil {
		return 0, status.Errorf(codes.Internal, "set refquota on %s: %v", dsPath, err)
	}
	return bytes, nil
}

// PublishContext is what the node plugin needs to mount //<server>/<share>.
//
// It carries a REFERENCE to the credential Secret and never the credential:
// anything placed here is persisted in the PersistentVolume and readable by
// anyone who can read that object.
func (b *Backend) PublishContext(ctx context.Context, id volume.ID) (map[string]string, error) {
	if err := volume.Confine(id, b.pool, b.parent); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	dsPath := id.DatasetPath()

	ds, err := b.c.DatasetQuery(ctx, dsPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "query dataset %s: %v", dsPath, err)
	}
	if ds == nil {
		return nil, status.Errorf(codes.NotFound, "volume %s does not exist", id)
	}

	p := b.recall(id)

	// Prefer the share the appliance actually published: a controller that
	// restarted has no cached prefix, and guessing the name would strand the
	// volume rather than fail loudly.
	name := shareName(id, p.sharePrefix)
	if share, err := b.shareByPath(ctx, mountpointOf(ds, dsPath)); err == nil && share != nil && share.Name != "" {
		name = share.Name
	}

	server := p.server
	if server == "" {
		b.mu.Lock()
		server = b.server
		b.mu.Unlock()
	}
	if server == "" {
		// Fall back to the appliance's own address rather than a cache a
		// controller restart empties, which would strand every existing volume.
		server = b.c.Host()
	}
	if server == "" {
		return nil, status.Errorf(codes.FailedPrecondition,
			"no SMB server address is known for %s: set the %q StorageClass parameter", id, ParamServer)
	}

	out := map[string]string{
		"server":   server,
		"share":    name, // the SMB share NAME, not a filesystem path
		"protocol": Protocol,
		// SMB ownership is mount-time: the cifs client maps these itself.
		"uid":      strconv.Itoa(p.uid),
		"gid":      strconv.Itoa(p.gid),
		"fileMode": p.fileMode,
		"dirMode":  p.dirMode,
	}
	if p.secretName != "" {
		out[nodeStageSecretNameKey] = p.secretName
	}
	if p.secretNamespace != "" {
		out[nodeStageSecretNamespaceKey] = p.secretNamespace
	}
	return out, nil
}

func (b *Backend) volumeFor(id volume.ID, bytes int64, name string, p params) *backend.Volume {
	ctxMap := map[string]string{
		"share":    name,
		"protocol": Protocol,
		"uid":      strconv.Itoa(p.uid),
		"gid":      strconv.Itoa(p.gid),
		"fileMode": p.fileMode,
		"dirMode":  p.dirMode,
	}
	if p.server != "" {
		ctxMap["server"] = p.server
	}
	if p.secretName != "" {
		ctxMap[nodeStageSecretNameKey] = p.secretName
	}
	if p.secretNamespace != "" {
		ctxMap[nodeStageSecretNamespaceKey] = p.secretNamespace
	}
	return &backend.Volume{ID: id, CapacityBytes: bytes, Context: ctxMap}
}

func (b *Backend) remember(id volume.ID, p params) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p.server != "" {
		b.server = p.server
	}
	b.byVol[id.String()] = p
}

// recall returns the parameters seen at Create time, or the defaults when this
// process never saw them — a controller that restarted must still be able to
// publish a volume it did not create.
func (b *Backend) recall(id volume.ID) params {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.byVol[id.String()]
	if !ok {
		return defaultParams()
	}
	return p
}

func (b *Backend) forget(id volume.ID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.byVol, id.String())
}

// ensure the interfaces stay satisfied even if either grows a method.
var (
	_ backend.Backend   = (*Backend)(nil)
	_ backend.Publisher = (*Backend)(nil)
)
