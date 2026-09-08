package podmon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// ConnectivityPath is the HTTP path of the appliance-backed connectivity
// report. It is served by the CONTROLLER, never by the node plugin.
const ConnectivityPath = "/podmon/v1/connectivity"

// DefaultConnectivityTimeout bounds the whole appliance conversation behind one
// report. A fencing decision that waits indefinitely for the appliance is a
// fencing decision that never happens; and since a timeout produces an error —
// which is never safe to fence on — being impatient here is safe.
const DefaultConnectivityTimeout = 10 * time.Second

// DefaultStaleLeaseAfter is how long an NFSv4 client may go without renewing
// its lease before this service stops calling it live.
//
// It is twice the NFSv4 lease period a Linux server defaults to (90s), and a
// client that holds state renews at roughly half a lease. So a client silent
// for longer than this has already missed several renewals and the server's own
// state would be expiring. Below that threshold "renewed N seconds ago" is a
// live, active client and a hard veto on fencing.
//
// UNVERIFIED: that this appliance runs the 90s default. nfs.config exposes no
// lease-time field (checked against
// https://192.168.10.253/api/docs/current/api_methods_nfs.config.html), so the
// hardware check that would settle it is reading /proc/fs/nfsd/nfsv4leasetime
// on the appliance. If it is longer, this threshold must grow with it: too
// short a threshold calls a live client dead, which is the dangerous direction.
const DefaultStaleLeaseAfter = 180 * time.Second

// State is what the appliance was able to say about one signal.
//
// StateUnknown is the ZERO VALUE deliberately. Every path that fails to fill a
// signal in — an unset field, a protocol nobody taught this service about, a
// half-built struct in a test — therefore reads as "I do not know", and a
// verdict built from it refuses to fence. There is no way to accidentally
// produce "safe to fence".
type State int

const (
	// StateUnknown: the answer could not be determined. Never safe to fence.
	StateUnknown State = iota
	// StateConnected: the appliance can still see this node. Never safe to fence.
	StateConnected
	// StateDisconnected: the appliance positively reports no such session or
	// lease. This is the ONLY state that permits fencing.
	StateDisconnected
	// StateUnobservable: this appliance cannot answer this question AT ALL —
	// not now, not with a retry, not with better credentials. It is distinct
	// from StateUnknown so that a permanent gap in the appliance's API is never
	// confused with a transient failure to ask, and so that it can be reported
	// honestly instead of being silently rendered as "not connected".
	StateUnobservable
)

// String renders a State for humans, logs and JSON.
func (s State) String() string {
	switch s {
	case StateConnected:
		return "connected"
	case StateDisconnected:
		return "disconnected"
	case StateUnobservable:
		return "unobservable"
	default:
		return "unknown"
	}
}

// MarshalJSON renders the state as its name, so a caller reading the JSON can
// never mistake an integer 0 for "false, not connected".
func (s State) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// SignalKind names one question asked of the appliance.
type SignalKind string

const (
	// SignalISCSISession: does the appliance list an iSCSI session from this node?
	SignalISCSISession SignalKind = "iscsi-session"
	// SignalNFSLease: does the appliance list this node as an NFS client, and
	// how long ago did it renew its lease?
	SignalNFSLease SignalKind = "nfs-lease"
	// SignalSMBSession: does the appliance list an SMB session from this node?
	// Always StateUnknown; see checkSMB.
	SignalSMBSession SignalKind = "smb-session"
	// SignalNVMeSession: does the appliance list an NVMe-oF session from this
	// node? Always StateUnknown; see checkNVMe.
	SignalNVMeSession SignalKind = "nvme-session"
	// SignalVolumeIO: is I/O in progress on this volume? Always
	// StateUnobservable; see checkVolumeIO.
	SignalVolumeIO SignalKind = "volume-io"
)

// Signal is one appliance-side observation about one node and one volume.
type Signal struct {
	Kind  SignalKind `json:"kind"`
	State State      `json:"state"`
	// Reason is always populated, including for StateDisconnected: an operator
	// reading an event about a node that was fenced needs the evidence, not
	// just the conclusion.
	Reason string `json:"reason"`
	// LeaseAgeSeconds is how long ago the node last renewed its NFSv4 lease. It
	// is truenas.RenewAgeUnknown for every other signal and for an NFSv3 client,
	// whose rmtab entry records a mount rather than a heartbeat.
	LeaseAgeSeconds int `json:"leaseAgeSeconds"`
}

// Report is the appliance's answer about one node's hold on one volume.
type Report struct {
	NodeID   string   `json:"nodeId"`
	VolumeID string   `json:"volumeId"`
	Signals  []Signal `json:"signals"`
	// Errors are everything that went wrong while asking. A non-empty Errors
	// makes the verdict unsafe on its own, whatever the signals say.
	Errors []string `json:"errors,omitempty"`
}

// Verdict is the machine-readable fencing answer derived from a Report.
type Verdict struct {
	// SafeToFence is true only when the appliance positively reported that this
	// node holds nothing. See Report.Verdict for the full rule.
	SafeToFence bool `json:"safeToFence"`
	// Reason states why, in one line, for a Kubernetes event.
	Reason string `json:"reason"`
	// Blocking lists the signals that vetoed the fence.
	Blocking []Signal `json:"blocking,omitempty"`
	// Unobservable lists the questions this appliance cannot answer at all.
	// They are reported on every verdict, including a safe one, so that "we
	// could not see I/O" is stated rather than implied.
	Unobservable []Signal `json:"unobservable,omitempty"`
}

// subsumedUnobservable lists the unobservable signals that do NOT block a
// fence, and each entry needs the argument that justifies it.
//
// SignalVolumeIO is the only member. The appliance cannot report per-volume
// I/O: reporting.get_data exposes 40 graphs and not one is per-dataset or
// per-zvol (verified on hardware, see .procoder/notes/truenas-api-findings.md).
// It is nevertheless safe to proceed without it, because I/O IMPLIES A SESSION:
// a node cannot be moving bytes to a zvol without an iSCSI session, or to a
// dataset without an NFS lease it is renewing. The session and lease signals
// are therefore a strict superset of the I/O veto — anything the I/O signal
// would have vetoed, they veto first. This is the one and only place that
// argument is allowed to be made; nothing else may be added here without an
// equivalent proof that some other observed signal subsumes it.
var subsumedUnobservable = []SignalKind{SignalVolumeIO}

// Verdict decides whether this report permits fencing.
//
// The rule, and the direction it fails in, is the whole safety property of this
// package. Fencing a live node corrupts data; failing to fence a dead one costs
// a pod some downtime. So a fence is permitted ONLY when all of:
//
//   - nothing went wrong while asking (any error means abort);
//   - at least one signal positively reports StateDisconnected — a report made
//     entirely of things we could not see is not evidence of absence;
//   - no signal is StateConnected;
//   - no signal is StateUnknown — an unknown is treated exactly like a
//     connected node, never like a disconnected one;
//   - every StateUnobservable signal is in subsumedUnobservable, i.e. some other
//     signal in this same report is known to veto everything it would have.
func (r Report) Verdict() Verdict {
	v := Verdict{}
	if len(r.Errors) > 0 {
		v.Reason = fmt.Sprintf("the appliance could not be asked about node %s: %s",
			r.NodeID, r.Errors[0])
		return v
	}
	disconnected := 0
	for _, s := range r.Signals {
		switch s.State {
		case StateDisconnected:
			disconnected++
		case StateConnected, StateUnknown:
			v.Blocking = append(v.Blocking, s)
		case StateUnobservable:
			v.Unobservable = append(v.Unobservable, s)
			if !slices.Contains(subsumedUnobservable, s.Kind) {
				v.Blocking = append(v.Blocking, s)
			}
		}
	}
	if len(v.Blocking) > 0 {
		b := v.Blocking[0]
		v.Reason = fmt.Sprintf("node %s: %s is %s (%s)", r.NodeID, b.Kind, b.State, b.Reason)
		return v
	}
	if disconnected == 0 {
		v.Reason = fmt.Sprintf(
			"node %s: nothing about volume %s could be positively observed, so its absence is not evidence",
			r.NodeID, r.VolumeID)
		return v
	}
	v.SafeToFence = true
	v.Reason = fmt.Sprintf("node %s holds no session or lease on volume %s according to the appliance",
		r.NodeID, r.VolumeID)
	return v
}

// SafeToFence reports whether EVERY report permits fencing, and why not when
// one does not. An empty slice is not safe: it is the answer of a caller that
// resolved no volumes, which is a bug rather than a clean bill of health.
func SafeToFence(reports []Report) (bool, string) {
	if len(reports) == 0 {
		return false, "no connectivity reports were produced, so nothing was actually checked"
	}
	for _, r := range reports {
		if v := r.Verdict(); !v.SafeToFence {
			return false, v.Reason
		}
	}
	return true, "the appliance reports no session or lease held by this node on any of its volumes"
}

// Sessions is the appliance-side query surface a connectivity report needs.
//
// It is deliberately narrow: this service must be able to answer with nothing
// but the two "who is attached?" calls, and a fake in a test must not have to
// implement the whole provisioning API to stand in for an appliance.
type Sessions interface {
	ISCSISessions(ctx context.Context) ([]truenas.ISCSISession, error)
	NFSClients(ctx context.Context) ([]truenas.NFSClient, error)
}

// Appliances resolves a backend name to its appliance client. *backend.Registry
// satisfies it.
type Appliances interface {
	Sessions(ctx context.Context, backendName string) (Sessions, error)
}

// RegistryAppliances adapts the driver's backend registry to Appliances.
type RegistryAppliances struct{ Registry *backend.Registry }

// Sessions returns the appliance client for a backend name.
func (a RegistryAppliances) Sessions(ctx context.Context, backendName string) (Sessions, error) {
	if a.Registry == nil {
		return nil, fmt.Errorf("no backend registry")
	}
	return a.Registry.Client(ctx, backendName)
}

// Connectivity answers, FROM THE APPLIANCE, whether a node still holds a
// volume.
//
// This is the controller-side service, and the vantage point is the point. The
// node-side NodeSelfCheck cannot answer this question in the situation it
// matters: a node that has fallen off the network cannot tell anyone that it
// has fallen off the network, and a node whose kernel is wedged mid-write is
// the least trustworthy possible witness to its own writes. The appliance, on
// the other side of the wire, is holding the session — so it is asked instead.
//
// What it can observe:
//
//   - iSCSI: iscsi.global.sessions, a live session per initiator.
//   - NFS: nfs.get_nfs4_clients, each client's address and the age of its lease.
//
// What it CANNOT observe, and says so rather than guessing:
//
//   - per-volume I/O — the appliance has no per-dataset or per-zvol counter;
//   - SMB sessions — this appliance's API publishes no session or status call
//     for SMB at all;
//   - NVMe-oF sessions — nvmet.global.sessions exists on the appliance but is
//     not yet on this driver's truenas.API surface.
//
// Every one of those is reported as an explicit state, never as a false.
type Connectivity struct {
	// Nodes turns a CSI node id into the addresses and initiator names a
	// session is attributed by. backend.NodeResolver satisfies it.
	Nodes backend.NodeResolver
	// Appliances opens the appliance a volume lives on.
	Appliances Appliances
	// Timeout bounds the whole report.
	Timeout time.Duration
	// StaleLeaseAfter is the NFSv4 lease age past which a client stops counting
	// as live. Zero uses DefaultStaleLeaseAfter.
	StaleLeaseAfter time.Duration
}

// NewConnectivity builds the controller-side service over the driver's own
// registry and node resolver.
func NewConnectivity(reg *backend.Registry, nodes backend.NodeResolver) *Connectivity {
	return &Connectivity{
		Nodes:           nodes,
		Appliances:      RegistryAppliances{Registry: reg},
		Timeout:         DefaultConnectivityTimeout,
		StaleLeaseAfter: DefaultStaleLeaseAfter,
	}
}

// Check produces one report per volume.
//
// Errors are recorded IN the reports rather than returned, because a caller
// that got half an answer and an error would have to remember not to act on the
// half; a report carrying an error already refuses to fence.
func (c *Connectivity) Check(ctx context.Context, nodeID string, ids []volume.ID) []Report {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()

	reports := make([]Report, 0, len(ids))
	node, nodeErr := c.resolve(ctx, nodeID)
	for _, id := range ids {
		r := Report{NodeID: nodeID, VolumeID: id.String()}
		if nodeErr != nil {
			// Without the node's addresses nothing can be attributed to it, and
			// an unattributable session must never be read as "not ours".
			r.Errors = append(r.Errors, nodeErr.Error())
			reports = append(reports, r)
			continue
		}
		sess, err := c.Appliances.Sessions(ctx, id.Backend)
		if err != nil {
			r.Errors = append(r.Errors, fmt.Sprintf("appliance %q is unreachable: %v", id.Backend, err))
			reports = append(reports, r)
			continue
		}
		r.Signals = append(r.Signals, c.protocolSignal(ctx, id, node, sess, &r))
		r.Signals = append(r.Signals, checkVolumeIO())
		reports = append(reports, r)
	}
	return reports
}

// resolve looks the node up, converting "I could not ask" into an error rather
// than an empty NodeRef — an empty NodeRef would match no session and read as a
// disconnected node.
func (c *Connectivity) resolve(ctx context.Context, nodeID string) (backend.NodeRef, error) {
	if c.Nodes == nil {
		return backend.NodeRef{}, fmt.Errorf("no node resolver: this node's addresses are unknown")
	}
	node, err := c.Nodes.Resolve(ctx, nodeID)
	if err != nil {
		return backend.NodeRef{}, fmt.Errorf("resolving node %s: %w", nodeID, err)
	}
	if len(node.Addrs) == 0 && node.IQN == "" {
		return backend.NodeRef{}, fmt.Errorf(
			"node %s has neither an address nor an initiator name, so no appliance session can be attributed to it",
			nodeID)
	}
	return node, nil
}

// protocolSignal asks the one question the volume's protocol allows.
func (c *Connectivity) protocolSignal(ctx context.Context, id volume.ID, node backend.NodeRef,
	sess Sessions, r *Report) Signal {
	switch id.Protocol {
	case "iscsi":
		return c.checkISCSI(ctx, node, sess, r)
	case "nfs":
		return c.checkNFS(ctx, node, sess, r)
	case "smb":
		return checkSMB()
	case "nvme":
		return checkNVMe()
	default:
		return Signal{
			Kind:            SignalKind(id.Protocol + "-session"),
			State:           StateUnknown,
			Reason:          fmt.Sprintf("no appliance-side liveness query is implemented for protocol %q", id.Protocol),
			LeaseAgeSeconds: truenas.RenewAgeUnknown,
		}
	}
}

// checkISCSI attributes live iSCSI sessions to the node.
//
// The match is by initiator IQN when the node advertises one, and by initiator
// address otherwise. It is deliberately node-level rather than volume-level:
// this driver serves every iSCSI volume through ONE shared target, so a session
// names the target, not the LUN. Answering "this node is attached to the
// appliance's target" is stricter than "this node is attached to this volume" —
// it vetoes fences it need not veto, and never permits one it should not.
//
// UNVERIFIED: the shape of a POPULATED element. iscsi.global.sessions was
// confirmed to exist and its schema (initiator, initiator_addr, target,
// target_alias) is published at
// https://192.168.10.253/api/docs/current/api_methods_iscsi.global.sessions.html,
// but the appliance had nothing attached when it was run. The hardware check:
// attach a LUN from a node and confirm initiator_addr carries the node's IP in
// the form matched here (a bare address, not host:port).
func (c *Connectivity) checkISCSI(ctx context.Context, node backend.NodeRef, sess Sessions, r *Report) Signal {
	sig := Signal{Kind: SignalISCSISession, LeaseAgeSeconds: truenas.RenewAgeUnknown}
	sessions, err := sess.ISCSISessions(ctx)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("listing iSCSI sessions: %v", err))
		sig.State = StateUnknown
		sig.Reason = "the appliance did not answer iscsi.global.sessions"
		return sig
	}
	for _, s := range sessions {
		switch {
		case node.IQN != "" && s.Initiator == node.IQN:
			sig.State = StateConnected
			sig.Reason = fmt.Sprintf("initiator %s still holds a session on target %s", s.Initiator, s.Target)
			return sig
		case s.InitiatorAddr != "" && slices.Contains(node.Addrs, s.InitiatorAddr):
			sig.State = StateConnected
			sig.Reason = fmt.Sprintf("address %s still holds a session on target %s", s.InitiatorAddr, s.Target)
			return sig
		}
	}
	sig.State = StateDisconnected
	sig.Reason = fmt.Sprintf("the appliance lists %d iSCSI sessions and none is from node %s", len(sessions), node.ID)
	return sig
}

// checkNFS attributes NFS clients to the node and reads their lease age.
//
// The three answers, and why each is what it is:
//
//   - the node's address is listed and its lease was renewed recently: CONNECTED.
//     A renewal seconds ago is a live, active client and the strongest veto this
//     service has.
//   - the node's address is listed with no lease age at all (an NFSv3 rmtab
//     entry, which records a mount rather than a heartbeat): UNKNOWN. rmtab is
//     documented as possibly stale in both directions, so it can neither prove
//     nor disprove liveness.
//   - the node's address is not listed, or is listed with a lease older than
//     StaleLeaseAfter: DISCONNECTED.
//
// RWX: the match is against THIS node's addresses only, never against the
// presence of clients in general. Another healthy node mounting the same
// ReadWriteMany export appears in this list and must not veto anything — it is
// legitimately using a volume it is entitled to, and fencing is per-node
// (Unpublish removes only this node's addresses from the share's host list), so
// its access survives.
func (c *Connectivity) checkNFS(ctx context.Context, node backend.NodeRef, sess Sessions, r *Report) Signal {
	sig := Signal{Kind: SignalNFSLease, LeaseAgeSeconds: truenas.RenewAgeUnknown}
	clients, err := sess.NFSClients(ctx)
	if err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("listing NFS clients: %v", err))
		sig.State = StateUnknown
		sig.Reason = "the appliance did not answer its NFS client listing"
		return sig
	}
	stale := int(c.staleLeaseAfter().Seconds())
	for _, cl := range clients {
		if !slices.Contains(node.Addrs, cl.Address) {
			continue
		}
		sig.LeaseAgeSeconds = cl.RenewAgeSeconds
		switch {
		case cl.RenewAgeSeconds == truenas.RenewAgeUnknown:
			sig.State = StateUnknown
			sig.Reason = fmt.Sprintf(
				"%s is listed as an NFS client with no lease age (an NFSv3 rmtab entry proves neither presence nor absence)",
				cl.Address)
		case cl.RenewAgeSeconds <= stale:
			sig.State = StateConnected
			sig.Reason = fmt.Sprintf("%s renewed its NFSv4 lease %ds ago (live within %ds)",
				cl.Address, cl.RenewAgeSeconds, stale)
		default:
			sig.State = StateDisconnected
			sig.Reason = fmt.Sprintf("%s last renewed its NFSv4 lease %ds ago, past the %ds liveness threshold",
				cl.Address, cl.RenewAgeSeconds, stale)
		}
		return sig
	}
	sig.State = StateDisconnected
	sig.Reason = fmt.Sprintf("the appliance lists %d NFS clients and none is node %s", len(clients), node.ID)
	return sig
}

// checkSMB reports SMB connectivity as unknown, because it is.
//
// The appliance's published API has no SMB session or status method: its whole
// SMB surface is smb.config, smb.update, sharing.smb.* and the ACL calls
// (enumerated from https://192.168.10.253/api/docs/current/ on 25.10.5 —
// smb.status, which older TrueNAS releases had, is not there). There is
// therefore no honest appliance-side answer to "is that node still holding an
// SMB session", and this service refuses to invent one. The consequence is
// deliberate and correct: an SMB volume is never fenced automatically.
//
// UNVERIFIED: whether a private (undocumented) SMB status method exists the way
// nfs.get_nfs4_clients does. The hardware check is a call to smb.status against
// the appliance with a live session, which needs a working API key.
func checkSMB() Signal {
	return Signal{
		Kind:            SignalSMBSession,
		State:           StateUnknown,
		Reason:          "this appliance publishes no SMB session or status method, so SMB connectivity cannot be observed",
		LeaseAgeSeconds: truenas.RenewAgeUnknown,
	}
}

// checkNVMe reports NVMe-oF connectivity as unknown.
//
// nvmet.global.sessions DOES exist on the appliance (listed at
// https://192.168.10.253/api/docs/current/api_methods_nvmet.global.sessions.html),
// so unlike SMB this gap is closeable — but the method is not on this driver's
// truenas.API surface yet, and inventing a call here would put a second,
// divergent copy of the appliance client in the fencing path. Until it is
// added, an NVMe-oF volume is never fenced automatically.
func checkNVMe() Signal {
	return Signal{
		Kind:            SignalNVMeSession,
		State:           StateUnknown,
		Reason:          "nvmet.global.sessions is not yet on this driver's appliance API, so NVMe-oF connectivity cannot be observed",
		LeaseAgeSeconds: truenas.RenewAgeUnknown,
	}
}

// checkVolumeIO states, every time, that per-volume I/O is not observable.
//
// Dell's array answers "are I/Os in progress on this volume". This appliance
// cannot: reporting.get_data exposes 40 graphs and none of them is per-dataset
// or per-zvol (verified on hardware 2026-09-08, see
// .procoder/notes/truenas-api-findings.md). The node's own kernel counters are
// NOT a substitute — the node is unreachable in exactly the case this runs, so
// reusing them would answer the question with the witness whose silence started
// the fence.
//
// So it is reported as unobservable, on every report, forever. See
// subsumedUnobservable for why a fence may still proceed without it.
func checkVolumeIO() Signal {
	return Signal{
		Kind:  SignalVolumeIO,
		State: StateUnobservable,
		Reason: "the appliance exposes no per-dataset or per-zvol counter, so whether I/O is in " +
			"progress on this volume cannot be observed from the appliance at all",
		LeaseAgeSeconds: truenas.RenewAgeUnknown,
	}
}

func (c *Connectivity) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultConnectivityTimeout
	}
	return c.Timeout
}

func (c *Connectivity) staleLeaseAfter() time.Duration {
	if c.StaleLeaseAfter <= 0 {
		return DefaultStaleLeaseAfter
	}
	return c.StaleLeaseAfter
}

// ConnectivityRequest asks about one node's volumes.
type ConnectivityRequest struct {
	NodeID string `json:"nodeId"`
	// VolumeIDs are driver volume handles, as they appear in a
	// PersistentVolume's volumeHandle.
	VolumeIDs []string `json:"volumeIds"`
}

// ConnectivityResponse is one report per volume plus the combined verdict.
type ConnectivityResponse struct {
	NodeID      string   `json:"nodeId"`
	Reports     []Report `json:"reports"`
	SafeToFence bool     `json:"safeToFence"`
	Reason      string   `json:"reason"`
}

// Handler serves the connectivity report over HTTP, for an operator or a
// sidecar. It is read-only: nothing here fences anything.
func (c *Connectivity) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ConnectivityPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a connectivity request", http.StatusMethodNotAllowed)
			return
		}
		var req ConnectivityRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		ids := make([]volume.ID, 0, len(req.VolumeIDs))
		for _, raw := range req.VolumeIDs {
			id, err := volume.ParseID(raw)
			if err != nil {
				http.Error(w, "malformed volume id", http.StatusBadRequest)
				return
			}
			ids = append(ids, id)
		}
		reports := c.Check(r.Context(), req.NodeID, ids)
		safe, reason := SafeToFence(reports)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ConnectivityResponse{
			NodeID: req.NodeID, Reports: reports, SafeToFence: safe, Reason: reason,
		})
	})
	return mux
}
