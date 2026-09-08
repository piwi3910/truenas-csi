package nfs

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/piwi3910/truenas-csi/internal/backend"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// policyRequest is testRequest with an operator network policy attached.
func policyRequest(name, networks string) backend.CreateRequest {
	r := testRequest(name, gib)
	r.Params[ParamNetworks] = networks
	return r
}

const exportPath = "/mnt/Pool0/k8s/pvc-fence"

func fenceID() volume.ID { return testID("pvc-fence") }

// TestUnpublishNeverLeavesTheExportOpen is the regression test for the single
// most dangerous mistake this package can make.
//
// On TrueNAS an NFS share is exported to EVERYONE when its `hosts` and
// `networks` lists are both empty — confirmed against live hardware, where two
// production shares in exactly that state are world-readable. So the obvious
// implementation of "revoke the last node" — remove its addresses from `hosts`
// and write the result back — does not fence the volume, it publishes it to the
// internet. Both fields are asserted here, not just `hosts`, because emptying
// either one alone is harmless and emptying both is the hole.
func TestUnpublishNeverLeavesTheExportOpen(t *testing.T) {
	cases := []struct {
		name  string
		nodes []backend.NodeRef
	}{
		{"one node", []backend.NodeRef{
			{ID: "worker-1", Addrs: []string{"10.0.0.1"}},
		}},
		{"two nodes, revoked in turn", []backend.NodeRef{
			{ID: "worker-1", Addrs: []string{"10.0.0.1"}},
			{ID: "worker-2", Addrs: []string{"10.0.0.2", "10.0.0.3"}},
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
				if _, err := b.Publish(ctx, fenceID(), node); err != nil {
					t.Fatalf("Publish %s: %v", node.ID, err)
				}
			}
			for _, node := range tc.nodes {
				if err := b.Unpublish(ctx, fenceID(), node); err != nil {
					t.Fatalf("Unpublish %s: %v", node.ID, err)
				}
			}

			sh := n.export(exportPath)
			if sh == nil {
				t.Fatal("the export vanished; unpublish must fence it, not delete it")
			}
			if len(sh.hosts) == 0 && len(sh.networks) == 0 {
				t.Fatal("hosts AND networks are both empty: the export is now open to " +
					"everyone, which is the opposite of a fence")
			}
			for _, node := range tc.nodes {
				for _, addr := range node.Addrs {
					if slices.Contains(sh.hosts, addr) {
						t.Errorf("%s is still allowed after being unpublished", addr)
					}
				}
			}
			if !slices.Contains(sh.hosts, DenyHost) {
				t.Errorf("the sentinel host must survive every revoke, got hosts=%v", sh.hosts)
			}
		})
	}
}

// TestCreateExportsNothingUntilPublished: a volume is provisioned long before
// anything attaches it, and between the two it must be reachable by nobody. An
// export created with empty lists would be world-readable for that entire
// window.
func TestCreateExportsNothingUntilPublished(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n)
	ctx := context.Background()

	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sh := n.export(exportPath)
	if sh == nil {
		t.Fatal("Create must publish an export")
	}
	if len(sh.hosts) != 1 || sh.hosts[0] != DenyHost || len(sh.networks) != 0 {
		t.Fatalf("a freshly provisioned export must grant nobody, got hosts=%v networks=%v",
			sh.hosts, sh.networks)
	}
}

// TestPublishGrantsAndIsIdempotent: the CO retries ControllerPublishVolume
// freely, and a retry must converge on the same access list rather than stack
// duplicates or widen it.
func TestPublishGrantsAndIsIdempotent(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()
	node := backend.NodeRef{ID: "worker-1", Addrs: []string{"10.0.0.1", "10.0.0.1"}}

	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pc, err := b.Publish(ctx, fenceID(), node)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pc["share"] != exportPath {
		t.Errorf("publish context must carry the export path, got %v", pc)
	}
	first := n.export(exportPath).hosts

	if _, err := b.Publish(ctx, fenceID(), node); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if got := n.export(exportPath).hosts; len(got) != len(first) {
		t.Fatalf("a repeated publish changed the access list: %v then %v", first, got)
	}
	if !slices.Contains(first, "10.0.0.1") {
		t.Fatalf("the node's address must be granted, got %v", first)
	}
}

// TestUnpublishIsSuccessForWhatIsNotThere: CSI requires unpublishing a volume
// that was never published — or one that has since been deleted — to succeed,
// because the CO has no other way to retire a stale attachment.
func TestUnpublishIsSuccessForWhatIsNotThere(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()
	node := backend.NodeRef{ID: "worker-9", Addrs: []string{"10.9.9.9"}}

	if err := b.Unpublish(ctx, fenceID(), node); err != nil {
		t.Fatalf("unpublishing an absent volume must succeed, got %v", err)
	}
	if _, err := b.Create(ctx, testRequest("pvc-fence", gib)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Unpublish(ctx, fenceID(), node); err != nil {
		t.Fatalf("unpublishing a never-published node must succeed, got %v", err)
	}
	if sh := n.export(exportPath); len(sh.hosts) == 0 && len(sh.networks) == 0 {
		t.Fatal("a no-op unpublish opened the export to everyone")
	}
}

// TestNetworkPolicyFiltersGrantsRatherThanWideningThem: the `networks`
// StorageClass parameter used to be written onto the export, where the
// appliance ORs it with the host list — so every address inside it could mount
// regardless of which node was attached, and no per-node revoke could take that
// away. It is a filter on what may be granted instead.
func TestNetworkPolicyFiltersGrantsRatherThanWideningThem(t *testing.T) {
	n := newNAS(t)
	b := newBackend(t, n).(*Backend)
	ctx := context.Background()

	if _, err := b.Create(ctx, policyRequest("pvc-fence", "10.0.0.0/24")); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sh := n.export(exportPath); len(sh.networks) != 0 {
		t.Fatalf("the operator's networks must not reach the export, got %v", sh.networks)
	}

	// An address inside the policy is granted; one outside it is dropped.
	if _, err := b.Publish(ctx, fenceID(), backend.NodeRef{
		ID: "worker-1", Addrs: []string{"10.0.0.5", "192.168.1.5"}}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	hosts := n.export(exportPath).hosts
	if !slices.Contains(hosts, "10.0.0.5") {
		t.Errorf("an address inside the policy must be granted, got %v", hosts)
	}
	if slices.Contains(hosts, "192.168.1.5") {
		t.Errorf("an address outside the policy must not be granted, got %v", hosts)
	}

	// A node with nothing inside the policy is refused outright rather than
	// published with an empty grant, which would look attached and never mount.
	_, err := b.Publish(ctx, fenceID(), backend.NodeRef{
		ID: "worker-2", Addrs: []string{"172.16.0.1"}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition for a node outside the policy, got %v", err)
	}
}

// TestWithDenyHostIsTheOnlyWayToWriteAnAccessList guards the choke point
// itself: every path that writes an export's host list goes through
// withDenyHost, so no caller can produce the unrestricted state by passing an
// empty slice.
func TestWithDenyHostIsTheOnlyWayToWriteAnAccessList(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{DenyHost}},
		{"blank entries", []string{"", "  "}, []string{DenyHost}},
		{"deduplicated and ordered", []string{"10.0.0.2", "10.0.0.1", "10.0.0.2"},
			[]string{"10.0.0.1", "10.0.0.2", DenyHost}},
		{"sentinel is not duplicated", []string{DenyHost}, []string{DenyHost}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withDenyHost(tc.in); !slices.Equal(got, tc.want) {
				t.Fatalf("withDenyHost(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := withoutHosts([]string{"10.0.0.1", DenyHost}, []string{"10.0.0.1"}); !slices.Equal(got, []string{DenyHost}) {
		t.Fatalf("withoutHosts must keep the sentinel, got %v", got)
	}
}
