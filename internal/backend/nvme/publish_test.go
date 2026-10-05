package nvme

import (
	"context"
	"encoding/json"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/backend"
)

func testNode(id, nqn string) backend.NodeRef {
	return backend.NodeRef{ID: id, Addrs: []string{"10.0.0.1"}, NQN: nqn}
}

// TestPublishBindsAndUnpublishFences: a namespace lives inside a subsystem and
// a subsystem is only discoverable through a port it is bound to, so the port
// binding is what makes the volume reachable — and removing it is what makes
// this a fence rather than a request. It works without knowing the node's NQN,
// which matters because a node id is all ControllerPublishVolume receives.
func TestPublishBindsAndUnpublishFences(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Create(ctx, createReq("pvc-f", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, _, _, links := n.counts(); links != 0 {
		t.Fatalf("Create must leave the subsystem unbound, got %d bindings", links)
	}

	pc, err := b.Publish(ctx, volID("pvc-f"), testNode("worker-1", ""))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	for _, k := range []string{"protocol", "portal", "nqn", "serial", "transport"} {
		if pc[k] == "" {
			t.Fatalf("the publish context must carry %q, got %v", k, pc)
		}
	}
	if _, _, _, _, links := n.counts(); links != 1 {
		t.Fatalf("Publish must bind the subsystem exactly once, got %d", links)
	}
	if _, err := b.Publish(ctx, volID("pvc-f"), testNode("worker-1", "")); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if _, _, _, _, links := n.counts(); links != 1 {
		t.Fatalf("a repeated publish stacked bindings, got %d", links)
	}

	if err := b.Unpublish(ctx, volID("pvc-f"), testNode("worker-1", "")); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	_, subs, nss, ports, links := n.counts()
	if links != 0 {
		t.Fatalf("Unpublish must remove every port binding, got %d", links)
	}
	// The fence removes the path, never the data or the shared listener.
	if subs != 1 || nss != 1 || ports != 1 {
		t.Fatalf("a fence destroyed data or the shared port: subsys=%d namespaces=%d ports=%d",
			subs, nss, ports)
	}
	if err := b.Unpublish(ctx, volID("pvc-f"), testNode("worker-1", "")); err != nil {
		t.Fatalf("a repeated Unpublish must succeed, got %v", err)
	}
}

// TestPublishGrantsAKnownHostNQNAndClosesTheSubsystem: adding a host to a
// subsystem that still has allow_any_host set grants nothing and fences
// nothing, because such a subsystem ignores its ACL entirely. The door has to
// be closed in the same breath as the grant.
func TestPublishGrantsAKnownHostNQNAndClosesTheSubsystem(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()
	const nqn = "nqn.2014-08.org.nvmexpress:uuid:worker-1"

	if _, err := b.Create(ctx, createReq("pvc-acl2", 1<<30, nil)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s := n.firstSubsys(); s == nil || s["allow_any_host"] != true {
		t.Fatalf("with no configured NQNs the subsystem starts open, got %v", s)
	}

	if _, err := b.Publish(ctx, volID("pvc-acl2"), testNode("worker-1", nqn)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if s := n.firstSubsys(); s == nil || s["allow_any_host"] != false {
		t.Fatalf("granting a host must close the subsystem to everyone else, got %v", s)
	}
	if got := n.hostGrants(); got != 1 {
		t.Fatalf("want one host grant, got %d", got)
	}
	// Idempotent: a retry must not stack grants.
	if _, err := b.Publish(ctx, volID("pvc-acl2"), testNode("worker-1", nqn)); err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if got := n.hostGrants(); got != 1 {
		t.Fatalf("a repeated publish stacked host grants, got %d", got)
	}

	if err := b.Unpublish(ctx, volID("pvc-acl2"), testNode("worker-1", nqn)); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if got := n.hostGrants(); got != 0 {
		t.Fatalf("the host grant must be revoked, got %d", got)
	}
}

// TestNVMePublishRejectsWhatIsNotThere: a volume that does not exist cannot be
// published, and unpublishing one is success — the CO has no other way to
// retire an attachment whose volume has already been deleted.
func TestNVMePublishRejectsWhatIsNotThere(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()

	if _, err := b.Publish(ctx, volID("pvc-absent"), testNode("worker-1", "")); err == nil {
		t.Fatal("publishing a volume that does not exist must fail")
	}
	if err := b.Unpublish(ctx, volID("pvc-absent"), testNode("worker-1", "")); err != nil {
		t.Fatalf("unpublishing a volume that does not exist must succeed, got %v", err)
	}
}

// TestPublishToleratesARaceOnTheExistingBinding pins idempotency at the point
// where it actually broke.
//
// bindPort and grantHost both list first and create only when the record is
// absent. That is a check-then-act, and ControllerPublishVolume is retried by
// the CO and can run concurrently for the same volume: two callers see nothing,
// both create, and the loser gets
//
//	[EINVAL] nvmet_port_subsys_create.port_id: This record already exists
//
// Observed on a live cluster, where it failed a publish whose work had in fact
// been done. The recovery is the same one ensureSubsystem uses -- establish the
// state by query rather than by classifying an errname the middleware does not
// report reliably.
func TestPublishToleratesARaceOnTheExistingBinding(t *testing.T) {
	for _, method := range []string{
		"nvmet.port_subsys.create",
		"nvmet.host_subsys.create",
	} {
		t.Run(method, func(t *testing.T) {
			n := newNAS(t)
			b := n.backend()
			ctx := context.Background()

			if _, err := b.Create(ctx, createReq("pvc-race", 1<<30, nil)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			// Publish once so the records really exist on the appliance.
			node := testNode("worker-1", "nqn.2014-08.org.nvmexpress:uuid:race")
			if _, err := b.Publish(ctx, volID("pvc-race"), node); err != nil {
				t.Fatalf("first Publish: %v", err)
			}

			// Now model the race rather than the sequential repeat. The FIRST
			// list comes back empty -- the other caller had not committed when
			// we looked -- and the create then fails, because by the time it
			// ran, it had. Every later list tells the truth, which is what the
			// recovery consults. A test that merely repeats a publish never
			// reaches the create at all: the pre-check finds the record and
			// returns early.
			listMethod := strings.Replace(method, ".create", ".query", 1)
			real := n.listHandler(listMethod)
			var first bool
			n.handle(listMethod, func(p []json.RawMessage) (any, error) {
				if !first {
					first = true
					return []any{}, nil
				}
				return real(p)
			})
			n.failOn(method, &fake.RPCError{
				Code: -32602, ErrName: "EINVAL",
				Reason: "[EINVAL] nvmet_port_subsys_create.port_id: This record already exists",
			})
			defer n.clearFail(method)

			if _, err := b.Publish(ctx, volID("pvc-race"), node); err != nil {
				t.Errorf("a publish that lost the race must succeed: the record it "+
					"collided with is its own, and re-querying finds it: %v", err)
			}
		})
	}
}

// TestPublishTwoVolumesOnOneNode pins azrtydxb/kuvryn-ai#122: the first
// NVMe-oF attach on a node worked, and a later attach of ANOTHER volume to the
// same node failed in ControllerPublishVolume with
//
//	binding subsystem 156 to port 3: nvmet.port_subsys.create: ... This record already exists
//
// Every volume has its own subsystem and they all share the one port, so each
// publish binds a different (port, subsys) pair -- and the CO re-issues
// ControllerPublishVolume for an attachment it already holds (retries, attacher
// resyncs), so each publish must also converge when its own pair already
// exists. The fake hands rows back in the appliance's real shape, where
// nvmet.port_subsys / nvmet.host_subsys rows carry "port" / "subsys" / "host"
// objects rather than *_id fields; reading only port_id made every existing
// binding look absent, so every publish after the first one per volume tried to
// create it again and the middleware rejected the duplicate.
func TestPublishTwoVolumesOnOneNode(t *testing.T) {
	n := newNAS(t)
	b := n.backend()
	ctx := context.Background()
	node := testNode("gx10-48f4", "nqn.2014-08.org.nvmexpress:uuid:gx10-48f4")

	for _, name := range []string{"pvc-a", "pvc-b"} {
		if _, err := b.Create(ctx, createReq(name, 1<<30, nil)); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}

	// Volume A attaches, then volume B on the same node, then the CO repeats
	// both (an attacher resync or a retry after a lost reply).
	for _, name := range []string{"pvc-a", "pvc-b", "pvc-a", "pvc-b"} {
		if _, err := b.Publish(ctx, volID(name), node); err != nil {
			t.Fatalf("Publish %s: %v", name, err)
		}
	}
	_, subs, _, ports, links := n.counts()
	if subs != 2 || ports != 1 || links != 2 {
		t.Fatalf("want two subsystems each bound once to the one shared port, got subsys=%d ports=%d port_subsys=%d",
			subs, ports, links)
	}
	if got := n.hostGrants(); got != 2 {
		t.Fatalf("want one host grant per volume, got %d", got)
	}

	// The context a repeated publish returns is the same one the node needs.
	pc, err := b.Publish(ctx, volID("pvc-b"), node)
	if err != nil {
		t.Fatalf("repeated Publish: %v", err)
	}
	if pc["portal"] == "" || pc["nqn"] == "" {
		t.Fatalf("a repeated publish must still return the connection details, got %v", pc)
	}
	if pc2, err := b.PublishContext(ctx, volID("pvc-b")); err != nil || pc2["portal"] != pc["portal"] {
		t.Fatalf("PublishContext must find the port the subsystem is bound to, got %v, %v", pc2, err)
	}

	// Fencing A touches A only: B keeps its binding and its grant.
	if err := b.Unpublish(ctx, volID("pvc-a"), node); err != nil {
		t.Fatalf("Unpublish pvc-a: %v", err)
	}
	if _, _, _, _, links := n.counts(); links != 1 {
		t.Fatalf("unpublishing A must leave exactly B's binding, got %d", links)
	}
	if got := n.hostGrants(); got != 1 {
		t.Fatalf("unpublishing A must revoke A's grant and only A's, got %d grants", got)
	}
	if err := b.Unpublish(ctx, volID("pvc-b"), node); err != nil {
		t.Fatalf("Unpublish pvc-b: %v", err)
	}
	if _, _, _, _, links := n.counts(); links != 0 {
		t.Fatalf("want no bindings left, got %d", links)
	}
	if got := n.hostGrants(); got != 0 {
		t.Fatalf("want no host grants left, got %d", got)
	}
}
