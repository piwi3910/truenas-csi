package truenas

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"

	"github.com/piwi3910/truenas-csi/internal/config"
)

// Transport is the wire-level half of a TrueNAS connection: how a middleware
// method and its parameters reach the appliance and how the answer comes back.
//
// SCALE implements it as JSON-RPC 2.0 over a persistent websocket; CORE
// implements it as one HTTPS request per call against the legacy REST v2 API.
// Everything above this line is identical for both, which is the point.
type Transport interface {
	// Call invokes a method and returns its raw result.
	Call(ctx context.Context, method string, params ...any) (json.RawMessage, error)
	// CallJSON invokes a method and decodes its result into out.
	CallJSON(ctx context.Context, out any, method string, params ...any) error
	// Host is the appliance's hostname or address, taken from the endpoint. It
	// is the default data address for NFS exports and iSCSI portals.
	Host() string
	// Close releases whatever the transport holds.
	Close() error
}

// API is the whole appliance surface the driver uses.
//
// Backends depend on this, never on a concrete client, so that a CORE appliance
// and a SCALE appliance are indistinguishable above this line. Every method here
// is implemented once, by Ops, on top of a Transport — so the two flavours
// cannot drift in behaviour, only in how bytes reach the box.
type API interface {
	Transport

	DatasetCreate(ctx context.Context, spec DatasetSpec) (*Dataset, error)
	DatasetQuery(ctx context.Context, id string) (*Dataset, error)
	DatasetList(ctx context.Context, prefix string) ([]Dataset, error)
	DatasetUpdate(ctx context.Context, id string, patch map[string]any) (*Dataset, error)
	DatasetDelete(ctx context.Context, id string, recursive, force bool) error
	SetUserProperty(ctx context.Context, id, key, value string) error
	RecommendedZvolBlocksize(ctx context.Context, pool string) (string, error)
	PoolQuery(ctx context.Context, name string) (*Pool, error)

	SnapshotCreate(ctx context.Context, dataset, name string) (*Snapshot, error)
	// SnapshotCreateRecursive snapshots a dataset and every child in ONE
	// operation. It is the only atomic multi-dataset primitive the appliance
	// offers, and therefore the only way a volume group snapshot can be
	// crash-consistent.
	SnapshotCreateRecursive(ctx context.Context, dataset, name string) (*Snapshot, error)
	// ISCSISessionCount reports how many initiators are attached, for metrics.
	ISCSISessionCount(ctx context.Context) (int, error)

	// SMB datasets carry an NFSv4 ACL rather than a mode, and NVMe-oF volumes
	// are served through nvmet objects. Both are transport-agnostic middleware
	// calls, so they belong to every flavour rather than to one client.
	SetACL(ctx context.Context, path string, dacl []ACLEntry, uid, gid int) error
	NVMeGlobalConfig(ctx context.Context) (*NVMeGlobal, error)
	NVMeSubsysByName(ctx context.Context, name string) (*NVMeSubsystem, error)
	NVMeSubsysCreate(ctx context.Context, name string, allowAnyHost bool) (*NVMeSubsystem, error)
	NVMeSubsysDelete(ctx context.Context, id int) error
	NVMeNamespaceByDevice(ctx context.Context, devicePath string) (*NVMeNamespace, error)
	NVMeNamespaceCreate(ctx context.Context, subsysID int, devicePath string) (*NVMeNamespace, error)
	NVMeNamespaceDelete(ctx context.Context, id int) error
	NVMePortFind(ctx context.Context, trtype, addr string, port int) (*NVMePort, error)
	NVMePortList(ctx context.Context) ([]NVMePort, error)
	NVMePortCreate(ctx context.Context, trtype, addr string, port int) (*NVMePort, error)
	NVMePortDelete(ctx context.Context, id int) error
	NVMePortSubsysList(ctx context.Context, subsysID int) ([]NVMePortSubsys, error)
	NVMePortSubsysCreate(ctx context.Context, portID, subsysID int) (*NVMePortSubsys, error)
	NVMePortSubsysDelete(ctx context.Context, id int) error
	NVMeHostByNQN(ctx context.Context, nqn string) (*NVMeHost, error)
	NVMeHostCreate(ctx context.Context, nqn string) (*NVMeHost, error)
	NVMeHostSubsysList(ctx context.Context, subsysID int) ([]NVMeHostSubsys, error)
	NVMeHostSubsysCreate(ctx context.Context, hostID, subsysID int) (*NVMeHostSubsys, error)
	NVMeHostSubsysDelete(ctx context.Context, id int) error
	SnapshotQuery(ctx context.Context, id string) (*Snapshot, error)
	SnapshotList(ctx context.Context, datasetPrefix string) ([]Snapshot, error)
	SnapshotDelete(ctx context.Context, id string) error
	SnapshotClone(ctx context.Context, snapshot, dst string) error

	NFSShareCreate(ctx context.Context, spec NFSShareSpec) (*NFSShare, error)
	NFSShareByPath(ctx context.Context, path string) (*NFSShare, error)
	NFSShareDelete(ctx context.Context, id int) error

	ISCSIGlobalConfig(ctx context.Context) (*ISCSIGlobal, error)
	PortalList(ctx context.Context) ([]ISCSIPortal, error)
	PortalCreate(ctx context.Context, comment string, ips []string) (*ISCSIPortal, error)
	PortalDelete(ctx context.Context, id int) error
	TargetByName(ctx context.Context, name string) (*ISCSITarget, error)
	TargetCreate(ctx context.Context, name string, portalID, initiatorID int) (*ISCSITarget, error)
	TargetDelete(ctx context.Context, id int) error
	ExtentByName(ctx context.Context, name string) (*ISCSIExtent, error)
	ExtentCreate(ctx context.Context, name, zvolPath string) (*ISCSIExtent, error)
	ExtentDelete(ctx context.Context, id int) error
	TargetExtentList(ctx context.Context, targetID int) ([]ISCSITargetExtent, error)
	TargetExtentCreate(ctx context.Context, targetID, extentID, lun int) (*ISCSITargetExtent, error)
	TargetExtentDelete(ctx context.Context, id int) error

	SetPerm(ctx context.Context, path, mode string, uid, gid int) error
}

// Ops implements every typed method of API in terms of a Transport.
//
// It is embedded by both clients rather than reimplemented per flavour: a
// second copy of "a missing dataset is (nil, nil)" is exactly the kind of
// divergence that turns a CSI idempotency guarantee into a flavour-specific
// bug.
type Ops struct{ Transport }

// Compile-time proof that the SCALE client is a complete API implementation.
var _ API = (*Client)(nil)

// ValidateBackend runs the full configuration validation for a single backend,
// before anything opens a connection.
//
// Both clients call this first. A plaintext endpoint does not merely fail to
// connect: TrueNAS REVOKES the API key presented over it, so this check has to
// happen before the first byte is sent, not after the first error.
func ValidateBackend(b config.Backend) error {
	cfg := &config.Config{Backends: map[string]config.Backend{b.Name: b}, NodeID: "x"}
	return cfg.Validate()
}

// TLSConfig builds the TLS settings for a backend.
//
// A stock TrueNAS certificate is self-signed with SAN=DNS:localhost and cannot
// be verified against any real address, so an explicit trust decision — a CA
// certificate, or InsecureSkipVerify — is unavoidable rather than sloppy.
func TLSConfig(b config.Backend) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if b.InsecureSkipVerify {
		tc.InsecureSkipVerify = true
		return tc, nil
	}
	if len(b.CACert) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b.CACert) {
			return nil, errors.New("caCert is not a valid PEM certificate")
		}
		tc.RootCAs = pool
	}
	return tc, nil
}
