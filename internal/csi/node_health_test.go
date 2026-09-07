package csi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/piwi3910/truenas-csi/internal/node"
)

// TestVolumeConditionReportedInStats checks that a volume the node knows to be
// unreachable is reported as such to the CO rather than looking healthy.
//
// The CSI spec's volume-condition mechanism was reshaped in v1.13, which is the
// version this driver builds against: the alpha `volume_condition` field on
// NodeGetVolumeStatsResponse was replaced by the NodeGetVolumeHealth RPC, the
// VolumeHealth message and the GET_VOLUME_HEALTH node capability. That is the
// same signal under a new name, so this test asserts the stats path still
// answers, that the health RPC carries the abnormal condition and its message,
// and that the capability is advertised — without it kubelet never asks.
func TestVolumeConditionReportedInStats(t *testing.T) {
	const id = "nas1/nfs/tank/k8s/pvc-condition"
	dir := t.TempDir()

	n := node.NewNode("worker-1", &node.Preflight{}, nil)
	n.Health().Timeout = time.Second
	n.Health().Statfs = func(string) error { return errors.New("stale file handle") }
	n.Health().Track(node.HealthTarget{VolumeID: id, Protocol: node.ProtocolNFS, Path: dir})
	t.Cleanup(func() { n.Health().Forget(id) })
	n.Health().CheckOnce(context.Background())

	srv := NewNode(n)
	ctx := context.Background()

	stats, err := srv.NodeGetVolumeStats(ctx, &csipb.NodeGetVolumeStatsRequest{
		VolumeId: id, VolumePath: dir})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats: %v", err)
	}
	if len(stats.GetUsage()) == 0 {
		t.Error("usage must still be reported for an unhealthy volume")
	}

	health, err := srv.NodeGetVolumeHealth(ctx, &csipb.NodeGetVolumeHealthRequest{
		VolumeId: id, VolumePublishPath: dir})
	if err != nil {
		t.Fatalf("NodeGetVolumeHealth: %v", err)
	}
	entries := health.GetVolumeHealth().GetHealthStatuses()
	if len(entries) == 0 {
		t.Fatal("an unreachable volume must report an adverse health condition")
	}
	if got := entries[0].GetStatus(); got != csipb.VolumeHealthErrorType_INACCESSIBLE {
		t.Errorf("health status = %v, want INACCESSIBLE", got)
	}
	if !strings.Contains(entries[0].GetMessage(), "stale file handle") {
		t.Errorf("health message should carry the cause, got %q", entries[0].GetMessage())
	}
	if health.GetVolumeHealth().GetVolumeId() != id {
		t.Errorf("health reported for %q, want %q", health.GetVolumeHealth().GetVolumeId(), id)
	}

	caps, err := srv.NodeGetCapabilities(ctx, &csipb.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities: %v", err)
	}
	var advertised bool
	for _, c := range caps.GetCapabilities() {
		if c.GetRpc().GetType() == csipb.NodeServiceCapability_RPC_GET_VOLUME_HEALTH {
			advertised = true
		}
	}
	if !advertised {
		t.Error("GET_VOLUME_HEALTH must be advertised, or the CO never asks for the condition")
	}
}

// TestHealthyVolumeReportsNoCondition is the other half: a healthy volume must
// report an empty status list, not a fabricated "unknown" entry, or every
// volume looks sick.
func TestHealthyVolumeReportsNoCondition(t *testing.T) {
	const id = "nas1/nfs/tank/k8s/pvc-healthy"
	dir := t.TempDir()

	n := node.NewNode("worker-1", &node.Preflight{}, nil)
	n.Health().Timeout = time.Second
	n.Health().Statfs = func(string) error { return nil }
	n.Health().Track(node.HealthTarget{VolumeID: id, Protocol: node.ProtocolNFS, Path: dir})
	t.Cleanup(func() { n.Health().Forget(id) })
	n.Health().CheckOnce(context.Background())

	health, err := NewNode(n).NodeGetVolumeHealth(context.Background(),
		&csipb.NodeGetVolumeHealthRequest{VolumeId: id, VolumePublishPath: dir})
	if err != nil {
		t.Fatalf("NodeGetVolumeHealth: %v", err)
	}
	if n := len(health.GetVolumeHealth().GetHealthStatuses()); n != 0 {
		t.Errorf("a healthy volume reported %d adverse conditions, want 0", n)
	}
}
