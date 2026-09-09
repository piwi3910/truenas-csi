package truenas

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// TestTargetExtentDeleteForces pins the argument without which iSCSI volumes
// can never be unpublished on a busy cluster.
//
// This driver puts every volume on a backend on ONE shared target, and the
// appliance refuses to unmap a LUN while that target has any session:
// "[EFAULT] Associated target iqn...:csi-pool0-k8s is in use." So the unmap
// succeeded only when the whole backend was idle, which on a live cluster is
// never. ControllerUnpublishVolume then failed and the attacher retried it for
// ever -- VolumeAttachments were never removed, PersistentVolumes could not be
// deleted, claims sat in Terminating, and the per-publish LUN mapping, which is
// the appliance-side fence for iSCSI, was never revoked.
//
// Found by the upstream conformance suite ("PersistentVolume ... still exists
// within 20m0s") and then reproduced directly on 25.10.6: with a live session
// on the shared target, delete without force returns EFAULT and delete with
// force succeeds.
func TestTargetExtentDeleteForces(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	var params []string
	s.Handle("iscsi.targetextent.delete", func(p []json.RawMessage) (any, error) {
		for _, one := range p {
			params = append(params, string(one))
		}
		return nil, nil
	})
	c := dialFake(t, s)

	if err := (&Ops{Transport: c}).TargetExtentDelete(context.Background(), 7); err != nil {
		t.Fatalf("TargetExtentDelete: %v", err)
	}

	if len(params) == 0 {
		t.Fatal("iscsi.targetextent.delete was never called")
	}
	if !slices.Contains(params, "true") {
		t.Errorf("iscsi.targetextent.delete was called with %v and no force; on a shared "+
			"target that fails whenever any volume on the backend is attached", params)
	}
}
