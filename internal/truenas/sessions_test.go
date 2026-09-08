package truenas

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// TestISCSISessionsKeepsInitiatorIdentity is the difference from
// ISCSISessionCount that the fence depends on: WHICH node is attached, not how
// many. Both the IQN and the address must survive, because a node is identified
// by whichever of the two the controller happens to know.
func TestISCSISessionsKeepsInitiatorIdentity(t *testing.T) {
	tests := []struct {
		name     string
		sessions []fake.ISCSISession
		want     []string
	}{
		{
			name: "no sessions",
			want: []string{},
		},
		{
			name: "one initiator",
			sessions: []fake.ISCSISession{{
				Initiator:     "iqn.2004-10.com.ubuntu:01:4f9d1b17f9aa",
				InitiatorAddr: "192.168.10.21",
				Target:        "iqn.2005-10.org.freenas.ctl:k8s",
				TargetAlias:   "k8s",
			}},
			want: []string{"iqn.2004-10.com.ubuntu:01:4f9d1b17f9aa@192.168.10.21 -> iqn.2005-10.org.freenas.ctl:k8s (k8s)"},
		},
		{
			name: "two nodes on the one shared target",
			sessions: []fake.ISCSISession{
				{Initiator: "iqn.a", InitiatorAddr: "192.168.10.21", Target: "iqn.t", TargetAlias: "k8s"},
				{Initiator: "iqn.b", InitiatorAddr: "192.168.10.22", Target: "iqn.t", TargetAlias: "k8s"},
			},
			want: []string{
				"iqn.a@192.168.10.21 -> iqn.t (k8s)",
				"iqn.b@192.168.10.22 -> iqn.t (k8s)",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.SeedISCSISessions(tc.sessions...)
			c := dialFake(t, s)

			got, err := c.ISCSISessions(context.Background())
			if err != nil {
				t.Fatalf("ISCSISessions: %v", err)
			}
			rendered := make([]string, 0, len(got))
			for _, sess := range got {
				rendered = append(rendered, sess.Initiator+"@"+sess.InitiatorAddr+
					" -> "+sess.Target+" ("+sess.TargetName+")")
			}
			if strings.Join(rendered, "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("want\n%s\ngot\n%s", strings.Join(tc.want, "\n"), strings.Join(rendered, "\n"))
			}
		})
	}
}

// TestISCSISessionsPropagatesFailure. An unreadable answer must not look like an
// empty one: "no sessions" is the conclusion that permits a fence.
func TestISCSISessionsPropagatesFailure(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.Handle("iscsi.global.sessions", func([]json.RawMessage) (any, error) {
		return nil, &fake.RPCError{Code: -32001, ErrName: "EACCES", Reason: "not authorised"}
	})
	c := dialFake(t, s)

	if _, err := c.ISCSISessions(context.Background()); err == nil {
		t.Fatal("want an error, got nil")
	}
}

// TestNFSClientsMergesBothProtocols pins the merge, the port stripping, the
// de-duplication and the lease age. A node that mounts over both v3 and v4, or
// that appears twice in rmtab, is still one node — and its liveness is what a
// fence reads, so it must survive the merge.
//
// Each want entry is "address@renewAge", with -1 for an unknown age.
func TestNFSClientsMergesBothProtocols(t *testing.T) {
	tests := []struct {
		name string
		v3   []string
		v4   []fake.NFSv4Client
		want []string
	}{
		{
			name: "nothing mounted",
			want: []string{},
		},
		{
			// rmtab records a mount, not a heartbeat: there is no lease age to
			// report and the driver must not invent one.
			name: "v3 only, with no liveness signal",
			v3:   []string{"192.168.10.21", "192.168.10.22"},
			want: []string{"192.168.10.21@-1", "192.168.10.22@-1"},
		},
		{
			name: "v4 only, port stripped and lease age kept",
			v4:   []fake.NFSv4Client{{Address: "192.168.10.102:666", RenewAgeSeconds: 14}},
			want: []string{"192.168.10.102@14"},
		},
		{
			name: "the same node over both protocols appears once, keeping its lease age",
			v3:   []string{"192.168.10.21"},
			v4:   []fake.NFSv4Client{{Address: "192.168.10.21:790", RenewAgeSeconds: 3}},
			want: []string{"192.168.10.21@3"},
		},
		{
			name: "a duplicate keeps the freshest lease age",
			v4: []fake.NFSv4Client{
				{Address: "192.168.10.21:790", RenewAgeSeconds: 40},
				{Address: "192.168.10.21:791", RenewAgeSeconds: 2},
			},
			want: []string{"192.168.10.21@2"},
		},
		{
			// A courtesy or expirable client has stopped talking but still holds
			// state on the server. Reporting it is the safe error for a fence;
			// the lease age is what tells the caller how stale it is.
			name: "a stalled v4 client is still reported",
			v4: []fake.NFSv4Client{
				{Address: "192.168.10.21:790", Status: "courtesy", RenewAgeSeconds: 120},
				{Address: "192.168.10.22:791", Status: "expirable", RenewAgeSeconds: 90000},
			},
			want: []string{"192.168.10.21@120", "192.168.10.22@90000"},
		},
		{
			name: "an IPv6 v4 client keeps its address, not its port",
			v4:   []fake.NFSv4Client{{Address: "[fd00::21]:790", RenewAgeSeconds: 5}},
			want: []string{"fd00::21@5"},
		},
		{
			name: "an address with no port is left alone",
			v4:   []fake.NFSv4Client{{Address: "192.168.10.21", RenewAgeSeconds: 5}},
			want: []string{"192.168.10.21@5"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.SeedNFSClients(tc.v3, tc.v4)
			c := dialFake(t, s)

			got, err := c.NFSClients(context.Background())
			if err != nil {
				t.Fatalf("NFSClients: %v", err)
			}
			rendered := make([]string, 0, len(got))
			for _, client := range got {
				rendered = append(rendered, client.Address+"@"+strconv.Itoa(client.RenewAgeSeconds))
			}
			sort.Strings(rendered)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(rendered, ",") != strings.Join(want, ",") {
				t.Fatalf("want %v, got %v", want, rendered)
			}
		})
	}
}

// TestClientCountsDecodeBareIntegers. Both count methods answer with a naked
// integer rather than an object, which is the only thing to get wrong here.
func TestClientCountsDecodeBareIntegers(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.SeedClientCounts(0, 1)
	c := dialFake(t, s)

	iscsi, err := c.ISCSIClientCount(context.Background())
	if err != nil || iscsi != 0 {
		t.Fatalf("ISCSIClientCount: want (0,nil), got (%d,%v)", iscsi, err)
	}
	nfs, err := c.NFSClientCount(context.Background())
	if err != nil || nfs != 1 {
		t.Fatalf("NFSClientCount: want (1,nil), got (%d,%v)", nfs, err)
	}
}

// TestNFSClientsFailsWhenEitherSourceFails. Half an answer reads as "that node
// has let go", so a partial listing is refused rather than returned.
func TestNFSClientsFailsWhenEitherSourceFails(t *testing.T) {
	for _, broken := range []string{"nfs.get_nfs3_clients", "nfs.get_nfs4_clients"} {
		t.Run(broken, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.SeedNFSClients([]string{"192.168.10.21"}, []fake.NFSv4Client{{Address: "192.168.10.22:790"}})
			s.Handle(broken, func([]json.RawMessage) (any, error) {
				return nil, &fake.RPCError{Code: -32001, ErrName: "EACCES", Reason: "not authorised"}
			})
			c := dialFake(t, s)

			if _, err := c.NFSClients(context.Background()); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}
