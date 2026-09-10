package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// stageRecordHost writes a host whose mount table holds one staged volume and
// one published block volume, each with its record beside it.
func stageRecordHost(t *testing.T, records []stageRecord, paths []string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	table := ""
	for i, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(records[i])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(StageRecordPath(full), b, 0o600); err != nil {
			t.Fatal(err)
		}
		table += "/dev/sdb " + full + " ext4 rw 0 0\n"
	}
	// A mount that is not this driver's. Nothing beside it, so nothing to find.
	table += "/dev/sda / ext4 rw 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestVolumesStagedBeforeTheProcessStartedAreMonitored is the whole point.
//
// The monitor's targets live in memory and are registered by NodeStageVolume
// alone. The kubelet's own state says an already-staged volume is staged, so it
// never calls NodeStageVolume for it again — which means that without recovery,
// every restart of the node plugin stops watching every volume already on the
// node, silently, for as long as those volumes live.
func TestVolumesStagedBeforeTheProcessStartedAreMonitored(t *testing.T) {
	const staged = "nas1/iscsi/Pool0/k8s/pvc-a"
	root := stageRecordHost(t,
		[]stageRecord{{VolumeID: staged, PublishContext: map[string]string{
			KeyProtocol: ProtocolISCSI, KeyPortal: "192.168.10.253:3260",
			KeyIQN: "iqn.x:csi", KeyNAA: "0xabc", KeyLUN: "3"}}},
		[]string{"var/lib/kubelet/plugins/kubernetes.io/csi/hash/globalmount"})

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	if got := n.RecoverStagedVolumes(context.Background()); got != 1 {
		t.Fatalf("recovered %d volumes, want 1", got)
	}
	tgt, ok := n.health.Target(staged)
	if !ok {
		t.Fatal("the volume is mounted on this node and the monitor is not watching it; " +
			"its health metric and its volume condition are both gone until it happens " +
			"to be staged again, which for a running pod is never")
	}
	if tgt.Protocol != ProtocolISCSI || tgt.LUN != "3" || tgt.NAA != "0xabc" {
		t.Errorf("recovered target lost what it needs to check identity: %+v", tgt)
	}
	if filepath.Base(tgt.Path) != "globalmount" {
		t.Errorf("recovered target has no path to stat: %q", tgt.Path)
	}
}

// TestRecoveryIgnoresMountsThatAreNotOurs keeps the scan from claiming another
// driver's mounts. The record file is the proof of ownership, not the path.
func TestRecoveryIgnoresMountsThatAreNotOurs(t *testing.T) {
	root := stageRecordHost(t, nil, nil)
	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}, health: NewHealthMonitor()}
	if got := n.RecoverStagedVolumes(context.Background()); got != 0 {
		t.Fatalf("claimed %d mounts that carry no record of ours", got)
	}
}

// TestAnOldStageRecordStillUnstages is the compatibility this cannot break.
//
// A node upgraded into this version has records on disk in the previous, flat
// shape. They must keep working for the thing they exist for — telling unstage
// which target to log out of — even though they carry no volume id and so
// cannot be recovered.
func TestAnOldStageRecordStillUnstages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalmount")
	old := map[string]string{KeyPortal: "192.168.10.253:3260", KeyIQN: "iqn.x:csi"}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(StageRecordPath(path), b, 0o600); err != nil {
		t.Fatal(err)
	}
	pc := ReadStageRecord(path)
	if pc[KeyIQN] != "iqn.x:csi" || pc[KeyPortal] != "192.168.10.253:3260" {
		t.Fatalf("a record written by the previous version no longer reads back; unstaging "+
			"every volume it staged would leave the iSCSI session behind: %v", pc)
	}
	if r, ok := readStageRecord(path); !ok || r.VolumeID != "" {
		t.Errorf("an old record must read back with no volume id, not a wrong one: %+v", r)
	}
}

// TestARoundTripKeepsBothHalves pins the current shape.
func TestARoundTripKeepsBothHalves(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "globalmount")
	pc := map[string]string{KeyPortal: "p", KeyIQN: "i", KeyNAA: "0xabc"}
	if err := WriteStageRecord(path, "nas1/iscsi/Pool0/k8s/pvc-a", pc); err != nil {
		t.Fatal(err)
	}
	r, ok := readStageRecord(path)
	if !ok || r.VolumeID != "nas1/iscsi/Pool0/k8s/pvc-a" {
		t.Fatalf("volume id did not survive the round trip: %+v", r)
	}
	if r.PublishContext[KeyNAA] != "0xabc" {
		t.Fatalf("publish context did not survive the round trip: %+v", r)
	}
}
