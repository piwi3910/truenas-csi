package podmon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// fakeSessions stands in for the appliance. It answers the two "who is
// attached?" calls and nothing else, which is the whole point of the narrow
// Sessions interface.
type fakeSessions struct {
	iscsi    []truenas.ISCSISession
	nfs      []truenas.NFSClient
	iscsiErr error
	nfsErr   error
}

func (f fakeSessions) ISCSISessions(context.Context) ([]truenas.ISCSISession, error) {
	return f.iscsi, f.iscsiErr
}

func (f fakeSessions) NFSClients(context.Context) ([]truenas.NFSClient, error) {
	return f.nfs, f.nfsErr
}

type fakeAppliances struct {
	sessions Sessions
	err      error
}

func (f fakeAppliances) Sessions(context.Context, string) (Sessions, error) {
	return f.sessions, f.err
}

type fakeNodes struct {
	ref backend.NodeRef
	err error
}

func (f fakeNodes) Resolve(context.Context, string) (backend.NodeRef, error) {
	return f.ref, f.err
}

func mustID(t *testing.T, s string) volume.ID {
	t.Helper()
	id, err := volume.ParseID(s)
	if err != nil {
		t.Fatalf("ParseID(%q): %v", s, err)
	}
	return id
}

// worker21 is the node under test throughout: one address, one IQN.
var worker21 = backend.NodeRef{
	ID:    "worker-21",
	Addrs: []string{"192.168.10.21"},
	IQN:   "iqn.2005-10.org.freenas.ctl:worker-21",
}

// TestVerdictNeverTreatsAnUnknownAsSafeToFence is the load-bearing test of this
// package.
//
// Fencing revokes a live node's access to its data. The failure that destroys
// data is fencing a node that is still writing; the failure that costs a pod
// some downtime is refusing to fence a node that is already dead. So every
// answer that is not a positive "the appliance sees nothing from this node"
// must refuse. If this test ever has to be relaxed, the change is wrong.
func TestVerdictNeverTreatsAnUnknownAsSafeToFence(t *testing.T) {
	io := checkVolumeIO()
	disconnected := Signal{Kind: SignalISCSISession, State: StateDisconnected, Reason: "no session"}

	cases := []struct {
		name      string
		report    Report
		wantSafe  bool
		wantInMsg string
	}{
		{
			name:     "the zero report: no signals, no errors, nothing observed",
			report:   Report{},
			wantSafe: false,
			// A struct nobody filled in must not read as a clean bill of health.
			wantInMsg: "not evidence",
		},
		{
			name: "an error while asking the appliance",
			report: Report{NodeID: "worker-21", Signals: []Signal{disconnected, io},
				Errors: []string{"appliance is unreachable"}},
			wantSafe:  false,
			wantInMsg: "unreachable",
		},
		{
			name:      "a signal the appliance could not determine",
			report:    Report{Signals: []Signal{{Kind: SignalNFSLease, State: StateUnknown, Reason: "no answer"}, io}},
			wantSafe:  false,
			wantInMsg: "unknown",
		},
		{
			name:      "a signal defaulted to the zero State",
			report:    Report{Signals: []Signal{{Kind: SignalNFSLease}, io}},
			wantSafe:  false,
			wantInMsg: "unknown",
		},
		{
			name:      "the node is still connected",
			report:    Report{Signals: []Signal{{Kind: SignalISCSISession, State: StateConnected, Reason: "live session"}, io}},
			wantSafe:  false,
			wantInMsg: "connected",
		},
		{
			name: "an unobservable signal that nothing else subsumes",
			report: Report{Signals: []Signal{disconnected,
				{Kind: SignalSMBSession, State: StateUnobservable, Reason: "no smb status call"}, io}},
			wantSafe:  false,
			wantInMsg: "unobservable",
		},
		{
			name:      "only unobservable signals: absence of evidence is not evidence of absence",
			report:    Report{Signals: []Signal{io}},
			wantSafe:  false,
			wantInMsg: "not evidence",
		},
		{
			name:     "a positive disconnection, with only the subsumed I/O gap",
			report:   Report{NodeID: "worker-21", Signals: []Signal{disconnected, io}},
			wantSafe: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.report.Verdict()
			if v.SafeToFence != tc.wantSafe {
				t.Fatalf("SafeToFence = %v, want %v (reason: %s)", v.SafeToFence, tc.wantSafe, v.Reason)
			}
			if v.Reason == "" {
				t.Error("a verdict must always say why; an event with no reason is unactionable")
			}
			if tc.wantInMsg != "" && !strings.Contains(v.Reason, tc.wantInMsg) {
				t.Errorf("reason %q does not mention %q", v.Reason, tc.wantInMsg)
			}
			if !v.SafeToFence && len(v.Blocking) == 0 && len(tc.report.Errors) == 0 &&
				!strings.Contains(v.Reason, "not evidence") {
				t.Error("an unsafe verdict must name what blocked it")
			}
		})
	}
}

// TestVerdictAlwaysReportsTheUnobservableIOGap: a safe verdict must still carry
// "I could not see per-volume I/O" so it is stated in the event, not implied by
// its absence.
func TestVerdictAlwaysReportsTheUnobservableIOGap(t *testing.T) {
	r := Report{Signals: []Signal{
		{Kind: SignalISCSISession, State: StateDisconnected, Reason: "no session"},
		checkVolumeIO(),
	}}
	v := r.Verdict()
	if !v.SafeToFence {
		t.Fatalf("want a safe verdict, got %s", v.Reason)
	}
	if len(v.Unobservable) != 1 || v.Unobservable[0].Kind != SignalVolumeIO {
		t.Fatalf("the I/O gap is missing from the verdict: %+v", v.Unobservable)
	}
}

// TestSafeToFenceRefusesAnEmptySetOfReports: a caller that resolved no volumes
// checked nothing, and "nothing was checked" is not "nothing is attached".
func TestSafeToFenceRefusesAnEmptySetOfReports(t *testing.T) {
	if safe, reason := SafeToFence(nil); safe {
		t.Fatalf("an empty report set was called safe: %s", reason)
	}
}

// TestCheckReadsTheApplianceAndAttributesSessionsToTheRightNode covers each
// protocol's answer, including the RWX case where another node is using the
// same export.
func TestCheckReadsTheApplianceAndAttributesSessionsToTheRightNode(t *testing.T) {
	cases := []struct {
		name       string
		volumeID   string
		sessions   fakeSessions
		wantKind   SignalKind
		wantState  State
		wantSafe   bool
		wantErrors bool
	}{
		{
			name:      "iscsi: this node still holds a session, matched by IQN",
			volumeID:  "nas1/iscsi/tank/k8s/pvc-a",
			sessions:  fakeSessions{iscsi: []truenas.ISCSISession{{Initiator: worker21.IQN, Target: "iqn:shared"}}},
			wantKind:  SignalISCSISession,
			wantState: StateConnected,
		},
		{
			name:      "iscsi: this node still holds a session, matched by address",
			volumeID:  "nas1/iscsi/tank/k8s/pvc-a",
			sessions:  fakeSessions{iscsi: []truenas.ISCSISession{{InitiatorAddr: "192.168.10.21", Target: "iqn:shared"}}},
			wantKind:  SignalISCSISession,
			wantState: StateConnected,
		},
		{
			name:     "iscsi: another node holds a session, this one does not",
			volumeID: "nas1/iscsi/tank/k8s/pvc-a",
			sessions: fakeSessions{iscsi: []truenas.ISCSISession{
				{Initiator: "iqn.2005-10.org.freenas.ctl:worker-99", InitiatorAddr: "192.168.10.99"}}},
			wantKind:  SignalISCSISession,
			wantState: StateDisconnected,
			wantSafe:  true,
		},
		{
			name:     "nfs: a lease renewed seconds ago is a live client",
			volumeID: "nas1/nfs/tank/k8s/pvc-a",
			sessions: fakeSessions{nfs: []truenas.NFSClient{
				{Address: "192.168.10.21", RenewAgeSeconds: 3}}},
			wantKind:  SignalNFSLease,
			wantState: StateConnected,
		},
		{
			name:     "nfs: a lease older than the liveness threshold",
			volumeID: "nas1/nfs/tank/k8s/pvc-a",
			sessions: fakeSessions{nfs: []truenas.NFSClient{
				{Address: "192.168.10.21", RenewAgeSeconds: 3600}}},
			wantKind:  SignalNFSLease,
			wantState: StateDisconnected,
			wantSafe:  true,
		},
		{
			name:     "nfs: an NFSv3 rmtab entry carries no liveness, so it is unknown",
			volumeID: "nas1/nfs/tank/k8s/pvc-a",
			sessions: fakeSessions{nfs: []truenas.NFSClient{
				{Address: "192.168.10.21", RenewAgeSeconds: truenas.RenewAgeUnknown}}},
			wantKind:  SignalNFSLease,
			wantState: StateUnknown,
		},
		{
			// RWX: a healthy peer legitimately mounting the same ReadWriteMany
			// export must not veto fencing the dead node, and must not be
			// mistaken for it either.
			name:     "nfs: a healthy peer on the same RWX export does not veto",
			volumeID: "nas1/nfs/tank/k8s/pvc-a",
			sessions: fakeSessions{nfs: []truenas.NFSClient{
				{Address: "192.168.10.22", RenewAgeSeconds: 2}}},
			wantKind:  SignalNFSLease,
			wantState: StateDisconnected,
			wantSafe:  true,
		},
		{
			name:      "smb: the appliance publishes no session call at all",
			volumeID:  "nas1/smb/tank/k8s/pvc-a",
			wantKind:  SignalSMBSession,
			wantState: StateUnknown,
		},
		{
			name:      "nvme: the session call exists on the appliance but not on this driver's API",
			volumeID:  "nas1/nvme/tank/k8s/pvc-a",
			wantKind:  SignalNVMeSession,
			wantState: StateUnknown,
		},
		{
			name:       "iscsi: the appliance refused to answer",
			volumeID:   "nas1/iscsi/tank/k8s/pvc-a",
			sessions:   fakeSessions{iscsiErr: errors.New("websocket is closed")},
			wantKind:   SignalISCSISession,
			wantState:  StateUnknown,
			wantErrors: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Connectivity{
				Nodes:      fakeNodes{ref: worker21},
				Appliances: fakeAppliances{sessions: tc.sessions},
			}
			reports := c.Check(context.Background(), worker21.ID, []volume.ID{mustID(t, tc.volumeID)})
			if len(reports) != 1 {
				t.Fatalf("want one report, got %d", len(reports))
			}
			r := reports[0]
			if (len(r.Errors) > 0) != tc.wantErrors {
				t.Fatalf("errors = %v, wantErrors = %v", r.Errors, tc.wantErrors)
			}
			var got *Signal
			for i := range r.Signals {
				if r.Signals[i].Kind == tc.wantKind {
					got = &r.Signals[i]
				}
			}
			if got == nil {
				t.Fatalf("no %s signal in %+v", tc.wantKind, r.Signals)
			}
			if got.State != tc.wantState {
				t.Fatalf("%s = %s, want %s (%s)", tc.wantKind, got.State, tc.wantState, got.Reason)
			}
			if v := r.Verdict(); v.SafeToFence != tc.wantSafe {
				t.Fatalf("SafeToFence = %v, want %v (%s)", v.SafeToFence, tc.wantSafe, v.Reason)
			}
			// Every report must state the I/O gap, whatever the protocol.
			if !hasSignal(r.Signals, SignalVolumeIO, StateUnobservable) {
				t.Error("the report does not state that per-volume I/O is unobservable")
			}
		})
	}
}

// stripComments removes // comments so a source guard reads code, not prose.
// It is line-based and therefore approximate; it errs towards keeping text,
// which for a guard means erring towards failing loudly.
func stripComments(body string) string {
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

func hasSignal(sigs []Signal, kind SignalKind, state State) bool {
	for _, s := range sigs {
		if s.Kind == kind && s.State == state {
			return true
		}
	}
	return false
}

// TestCheckRefusesWhenTheNodeCannotBeIdentified: without addresses or an IQN no
// session can be attributed to the node, so no session can be ruled out either.
func TestCheckRefusesWhenTheNodeCannotBeIdentified(t *testing.T) {
	for name, nodes := range map[string]backend.NodeResolver{
		"the resolver failed":      fakeNodes{err: errors.New("no such node")},
		"the node has no identity": fakeNodes{ref: backend.NodeRef{ID: "worker-21"}},
		"there is no resolver":     nil,
	} {
		t.Run(name, func(t *testing.T) {
			c := &Connectivity{Nodes: nodes, Appliances: fakeAppliances{sessions: fakeSessions{}}}
			reports := c.Check(context.Background(), "worker-21",
				[]volume.ID{mustID(t, "nas1/nfs/tank/k8s/pvc-a")})
			if safe, reason := SafeToFence(reports); safe {
				t.Fatalf("an unidentifiable node was called safe to fence: %s", reason)
			}
		})
	}
}

// TestUnreachableApplianceIsNeverSafeToFence: the appliance being down is the
// worst possible moment to guess. It is also exactly when a fencing controller
// is most likely to be running.
func TestUnreachableApplianceIsNeverSafeToFence(t *testing.T) {
	c := &Connectivity{
		Nodes:      fakeNodes{ref: worker21},
		Appliances: fakeAppliances{err: errors.New("dial wss://nas1: connection refused")},
	}
	reports := c.Check(context.Background(), worker21.ID, []volume.ID{mustID(t, "nas1/nfs/tank/k8s/pvc-a")})
	if safe, reason := SafeToFence(reports); safe {
		t.Fatalf("an unreachable appliance was called safe to fence: %s", reason)
	}
}

// TestConnectivityIsSourcedFromTheApplianceNotTheNode reads this file's source,
// in the style of TestLastIOProbeDoesNotUseModTime, because the regression it
// guards against is silent.
//
// The whole design is that the fencing answer comes from the appliance. A future
// change that "improves" it by consulting the node's statfs, its kernel I/O
// counters or its self-check would produce a service that works perfectly in
// every test and answers nothing at all in the one case it exists for: the node
// is gone. The compiler cannot catch that; this can.
func TestConnectivityIsSourcedFromTheApplianceNotTheNode(t *testing.T) {
	src, err := os.ReadFile("connectivity.go")
	if err != nil {
		t.Fatal(err)
	}
	// Comments are stripped first: this file's own prose explains at length why
	// the node is the wrong witness, and naming the thing you refuse to use is
	// not using it. Only executable code is scanned.
	body := stripComments(string(src))

	for _, banned := range []string{
		"statfsProbe", "dialProbe", "lastIOProbe", "defaultIOCounters",
		"NodeSelfCheck", "syscall.Statfs", "/proc/",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("connectivity.go references %q. The connectivity answer must come from the "+
				"APPLIANCE: a node that has stopped answering cannot report that it has stopped "+
				"answering, and that is the only situation fencing exists for.", banned)
		}
	}
	for _, required := range []string{"ISCSISessions(ctx)", "NFSClients(ctx)"} {
		if !strings.Contains(body, required) {
			t.Errorf("connectivity.go no longer calls %s: it is not asking the appliance anything", required)
		}
	}
}
