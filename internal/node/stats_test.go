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
