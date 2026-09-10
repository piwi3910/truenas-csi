package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestE2EVolumeStats(t *testing.T) {
	n := newTestNode(t, hostRoot(t), &fakeExec{}, CapNFS)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "payload"), make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	resp, err := n.Stats(context.Background(), StatsRequest{VolumeID: "pvc-abc", VolumePath: dir})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	byUnit := map[UsageUnit]Usage{}
	for _, u := range resp.Usage {
		byUnit[u.Unit] = u
	}
	b, ok := byUnit[UnitBytes]
	if !ok {
		t.Fatalf("no byte usage reported: %+v", resp.Usage)
	}
	if b.Total <= 0 || b.Available <= 0 {
		t.Fatalf("byte usage total=%d available=%d, want both non-zero", b.Total, b.Available)
	}
	if b.Used+b.Available > b.Total+b.Total {
		t.Fatalf("byte usage is incoherent: %+v", b)
	}
	i, ok := byUnit[UnitInodes]
	if !ok {
		t.Fatalf("no inode usage reported: %+v", resp.Usage)
	}
	if i.Total <= 0 {
		t.Fatalf("inode total = %d, want non-zero", i.Total)
	}

	// A path the kubelet asks about but which is not there must be distinguishable
	// from any other failure, because the CO retries on NotFound and gives up on
	// the rest.
	_, err = n.Stats(context.Background(), StatsRequest{
		VolumeID: "pvc-abc", VolumePath: filepath.Join(dir, "gone"),
	})
	if !errors.Is(err, ErrVolumePathNotFound) {
		t.Fatalf("missing path error = %v, want ErrVolumePathNotFound", err)
	}

	if _, err := n.Stats(context.Background(), StatsRequest{VolumeID: "pvc-abc"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty path error = %v, want ErrInvalidRequest", err)
	}
}

// TestBlockVolumeStatsDescribeTheDeviceNotTheHost is what these numbers are for.
//
// A raw block volume has no filesystem, and statfs does not fail on its path —
// it answers about the filesystem containing the device node, which is
// devtmpfs. Measured on a real cluster: a 1 GiB block PVC reported 16,434,946,048
// bytes of capacity, half the node's RAM, and every block volume on that node
// reported the same figure. Those are the numbers behind
// kubelet_volume_stats_capacity_bytes and every usage alert built on it.
func TestBlockVolumeStatsDescribeTheDeviceNotTheHost(t *testing.T) {
	const (
		major, minor = 8, 48
		sectors      = 2097152 // 1 GiB in 512-byte sectors
	)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys", "block", "sdd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys", "dev", "block"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sys", "block", "sdd", "size"),
		[]byte("2097152\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../devices/virtual/block/sdd",
		filepath.Join(root, "sys", "dev", "block", "8:48")); err != nil {
		t.Fatal(err)
	}
	// The published device node. mknod needs root, so the stat is stubbed.
	target := filepath.Join(root, "device")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	restore := blockDeviceNumber
	t.Cleanup(func() { blockDeviceNumber = restore })
	blockDeviceNumber = func(path string) (uint64, bool) {
		if path != target {
			return 0, false
		}
		return mkdev(major, minor), true
	}

	n := &Node{Root: root, pre: &Preflight{Found: map[Capability]bool{}}}
	resp, err := n.Stats(context.Background(), StatsRequest{VolumeID: "v", VolumePath: target})
	if err != nil {
		t.Fatalf("Stats on a block volume: %v", err)
	}
	if len(resp.Usage) != 1 || resp.Usage[0].Unit != UnitBytes {
		t.Fatalf("want exactly one byte usage and no inode usage (a raw block device has no "+
			"inodes to count), got %+v", resp.Usage)
	}
	if got, want := resp.Usage[0].Total, int64(sectors)*512; got != want {
		t.Errorf("reported %d bytes of capacity, want the device's own %d — the host's "+
			"devtmpfs size is not this volume's size", got, want)
	}
	if resp.Usage[0].Used != 0 || resp.Usage[0].Available != 0 {
		t.Errorf("claimed to know how much of a raw block device is used (%+v); only its "+
			"consumer can know that", resp.Usage[0])
	}
}
