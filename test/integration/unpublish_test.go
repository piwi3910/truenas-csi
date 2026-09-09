package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
)

// TestE2EISCSIUnpublishWhileTargetInUse pins the fix for a failure that made
// every iSCSI volume undeletable on a busy cluster.
//
// This driver puts every volume on a backend on ONE shared target, and the
// appliance refuses to unmap a LUN while that TARGET has any session:
//
//	[EFAULT] Associated target iqn...:csi-pool0-k8s is in use.
//
// The check is on the target, not on the LUN being unmapped, so
// ControllerUnpublishVolume could only succeed when the entire backend was
// idle. Every earlier test missed it by tearing all its volumes down at once,
// leaving no session behind; the upstream conformance suite found it as
// "PersistentVolume ... still exists within 20m0s", because the attacher
// retried the failing unpublish for ever and the PersistentVolume could never
// be deleted.
//
// The volume this test unpublishes is NOT the one holding the session. That is
// the point: unpublishing one volume must not require every other volume on the
// backend to be detached first.
func TestE2EISCSIUnpublishWhileTargetInUse(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)
	ctx := context.Background()

	// Volume 1: attached and logged in, so the shared target has a live session
	// for the whole test.
	holder := e.createVolume(t, c, uniqueName("pvc-e2e-holder"), "iscsi", 1<<30, nil)
	hctx := holder.GetVolumeContext()

	// The shared helper always logs out again, which is exactly what must NOT
	// happen here: the session has to outlive the unpublish below. So the login
	// is done by hand, and torn down by a cleanup that runs whatever happens --
	// a leaked session would sit on somebody's node.
	login := ""
	if hctx["chapUser"] != "" && hctx["chapSecret"] != "" {
		login = fmt.Sprintf(`
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.authmethod -v CHAP
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.username -v '%[3]s'
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.password -v '%[4]s'
`, hctx["iqn"], hctx["portal"], hctx["chapUser"], hctx["chapSecret"])
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(), fmt.Sprintf(`
iscsiadm -m node -T %[1]s -p %[2]s --logout >/dev/null 2>&1 || true
iscsiadm -m node -o delete -T %[1]s -p %[2]s >/dev/null 2>&1 || true
`, hctx["iqn"], hctx["portal"]))
	})
	out, err := r.Run(ctx, fmt.Sprintf(`
set -e
iscsiadm -m discovery -t sendtargets -p %[2]s >/dev/null
%[3]s
iscsiadm -m node -T %[1]s -p %[2]s --login >/dev/null
sleep 3
echo "SESSIONS=$(iscsiadm -m session 2>/dev/null | grep -c '%[1]s' || true)"
`, hctx["iqn"], hctx["portal"], login))
	if err != nil {
		t.Fatalf("logging the node into the shared target: %v\n%s", err, out)
	}
	if strings.Contains(out, "SESSIONS=0") {
		t.Fatalf("no session was established, so the target is not in use:\n%s", out)
	}

	// Volume 2: published to the same node, and then unpublished while the
	// session from volume 1 is still up.
	other := e.createVolume(t, c, uniqueName("pvc-e2e-other"), "iscsi", 1<<30, nil)
	if _, err := c.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: other.GetVolumeId(), NodeId: e.nodeID,
	}); err != nil {
		t.Fatalf("ControllerUnpublishVolume refused while another volume held the "+
			"shared target's session; on a live cluster that is always, and the "+
			"attacher retries it for ever: %v", err)
	}

	// Unpublishing twice is success, per CSI, and must not start failing once
	// the mapping is already gone.
	if _, err := c.ControllerUnpublishVolume(ctx, &csipb.ControllerUnpublishVolumeRequest{
		VolumeId: other.GetVolumeId(), NodeId: e.nodeID,
	}); err != nil {
		t.Errorf("a repeated ControllerUnpublishVolume must succeed: %v", err)
	}
}
