package smb

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
)

const smbPath = "/mnt/Pool0/k8s/pvc-fence"

// TestUnpublishDeniesRatherThanAdmits is the SMB half of the empty-list trap.
//
// Samba treats an empty `hostsallow` as "no restriction", so removing the last
// node from it would admit everyone rather than nobody. The driver's answer is
// `hostsdeny: ["ALL"]` on every write — the appliance's own documented way to
// "deny all by default" — which makes the allow list authoritative and an empty
// allow list a real fence.
func TestUnpublishDeniesRatherThanAdmits(t *testing.T) {
	cases := []struct {
		name  string
		nodes []backend.NodeRef
	}{
		{"one node", []backend.NodeRef{{ID: "worker-1", Addrs: []string{"10.0.0.1"}}}},
		{"two nodes", []backend.NodeRef{
			{ID: "worker-1", Addrs: []string{"10.0.0.1"}},
			{ID: "worker-2", Addrs: []string{"10.0.0.2"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNAS(t)
			b := newBackend(t, n).(*Backend)
			ctx := context.Background()

			if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			for _, node := range tc.nodes {
				if _, err := b.Publish(ctx, testID("pvc-fence"), node); err != nil {
					t.Fatalf("Publish %s: %v", node.ID, err)
				}
			}
			for _, node := range tc.nodes {
				if err := b.Unpublish(ctx, testID("pvc-fence"), node); err != nil {
					t.Fatalf("Unpublish %s: %v", node.ID, err)
				}
			}

			sh := n.share(smbPath)
			if sh == nil {
				t.Fatal("the share vanished; unpublish must fence it, not delete it")
			}
			if got := sh.hostList(optHostsAllow); len(got) != 0 {
				t.Errorf("no node may remain allowed after every revoke, got %v", got)
			}
			if got := sh.hostList(optHostsDeny); !slices.Contains(got, DenyAll) {
				t.Fatalf("hostsdeny must keep %q or the empty allow list admits everyone, got %v",
					DenyAll, got)
			}
		})
	}
}

// TestCreateSharesNothingUntilPublished: between provisioning and the first
// attach the share must be reachable by nobody.
func TestCreateSharesNothingUntilPublished(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sh := n.share(smbPath)
	if sh == nil {
		t.Fatal("Create must publish a share")
	}
	if got := sh.hostList(optHostsAllow); len(got) != 0 {
		t.Errorf("a freshly provisioned share must allow nobody, got %v", got)
	}
	if got := sh.hostList(optHostsDeny); !slices.Contains(got, DenyAll) {
		t.Errorf("a freshly provisioned share must deny all by default, got %v", got)
	}
}

// TestPublishPreservesUnrelatedOptions: sharing.smb.update REPLACES the whole
// nested `options` object, and a LEGACY_SHARE carries a dozen keys this driver
// knows nothing about. Writing a fresh object with only the host lists would
// silently reset every one of them.
func TestPublishPreservesUnrelatedOptions(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Stand in for the options an operator or an older TrueNAS put there.
	sh := n.share(smbPath)
	n.mu.Lock()
	sh.options["recyclebin"] = true
	sh.options["path_suffix"] = "%U"
	n.mu.Unlock()

	if _, err := b.Publish(ctx, testID("pvc-fence"),
		backend.NodeRef{ID: "worker-1", Addrs: []string{"10.0.0.1"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	after := n.share(smbPath)
	if after.options["recyclebin"] != true || after.options["path_suffix"] != "%U" {
		t.Fatalf("an access-list update wiped unrelated options: %v", after.options)
	}
	if got := after.hostList(optHostsAllow); !slices.Contains(got, "10.0.0.1") {
		t.Fatalf("the node must be allowed, got %v", got)
	}
}

// TestSMBPublishRefusesANodeWithNoAddress: hostsallow is written in addresses,
// so a node with none would be "published" with an empty grant — attached in
// the CO's eyes, unmountable in reality, and with nothing for the fence to
// revoke.
func TestSMBPublishRefusesANodeWithNoAddress(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := b.Publish(ctx, testID("pvc-fence"), backend.NodeRef{ID: "worker-1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
}

// TestSMBUnpublishIsSuccessForWhatIsNotThere: CSI requires retiring a stale
// attachment to succeed even when the volume or the grant has already gone.
func TestSMBUnpublishIsSuccessForWhatIsNotThere(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()
	node := backend.NodeRef{ID: "worker-9", Addrs: []string{"10.9.9.9"}}

	if err := b.Unpublish(ctx, testID("pvc-fence"), node); err != nil {
		t.Fatalf("unpublishing an absent volume must succeed, got %v", err)
	}
	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Unpublish(ctx, testID("pvc-fence"), node); err != nil {
		t.Fatalf("unpublishing a never-published node must succeed, got %v", err)
	}
	if got := n.share(smbPath).hostList(optHostsDeny); !slices.Contains(got, DenyAll) {
		t.Fatalf("a no-op unpublish dropped the deny-all guard, got %v", got)
	}
}

// TestAccessOptionsAlwaysDeniesAll guards the choke point every write goes
// through, so no future caller can produce an unrestricted share by passing an
// empty allow list.
func TestAccessOptionsAlwaysDeniesAll(t *testing.T) {
	cases := []struct {
		name  string
		allow []string
		want  []string
	}{
		{"empty", nil, []string{}},
		{"blank entries", []string{"", " "}, []string{}},
		{"deduplicated and ordered", []string{"10.0.0.2", "10.0.0.1", "10.0.0.2"},
			[]string{"10.0.0.1", "10.0.0.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := accessOptions(map[string]any{"guestok": false}, tc.allow)
			if got, _ := opts[optHostsAllow].([]string); !slices.Equal(got, tc.want) {
				t.Errorf("hostsallow = %v, want %v", got, tc.want)
			}
			if got, _ := opts[optHostsDeny].([]string); !slices.Equal(got, []string{DenyAll}) {
				t.Errorf("hostsdeny = %v, want [%s]", got, DenyAll)
			}
			if opts["guestok"] != false {
				t.Errorf("unrelated options must be carried through, got %v", opts)
			}
		})
	}
}
