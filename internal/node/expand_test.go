package node

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// growingDevice answers blockdev --getsize64 with the old size until the rescan has
// been issued, and with the new size afterwards — which is exactly how the kernel
// behaves: the LUN's size changes only once the session has been rescanned.
type growingDevice struct {
	rescanned atomic.Bool
	old, new  int64
	fsType    string
}

func (g *growingDevice) run(name string, args []string) ([]byte, error) {
	switch name {
	case "iscsiadm":
		if contains(args, "-R") {
			g.rescanned.Store(true)
		}
		return nil, nil
	case "blockdev":
		if g.rescanned.Load() {
			return []byte(fmt.Sprintf("%d\n", g.new)), nil
		}
		return []byte(fmt.Sprintf("%d\n", g.old)), nil
	case "blkid":
		return []byte(g.fsType + "\n"), nil
	}
	return nil, nil
}

// shortExpandWait shrinks the size-change poll bound for the tests.
func shortExpandWait(t *testing.T) {
	t.Helper()
	oldT, oldI := expandWaitTimeout, expandPollInterval
	expandWaitTimeout, expandPollInterval = 500*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { expandWaitTimeout, expandPollInterval = oldT, oldI })
}

func TestE2EExpandMountedISCSI(t *testing.T) {
	shortExpandWait(t)

	for _, tc := range []struct {
		fsType string
		grow   string
	}{
		{"ext4", "resize2fs"},
		{"xfs", "xfs_growfs"},
	} {
		t.Run(tc.fsType, func(t *testing.T) {
			root := iscsiRoot(t, true)
			g := &growingDevice{old: 1073741824, new: 2147483648, fsType: tc.fsType}
			e := &fakeExec{handler: g.run}
			caps := []Capability{CapISCSI, CapExt4, CapXFS}
			n := newTestNode(t, root, e, caps...)
			volumePath := filepath.Join(t.TempDir(), "globalmount")
			writeMounts(t, root, volumePath)

			resp, err := n.Expand(context.Background(), ExpandRequest{
				VolumeID:         "pvc-abc",
				VolumePath:       volumePath,
				StagingPath:      volumePath,
				PublishContext:   iscsiContext(),
				VolumeCapability: VolumeCapability{FsType: tc.fsType},
				CapacityBytes:    g.new,
			})
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			if resp.CapacityBytes != g.new {
				t.Fatalf("capacity = %d, want %d", resp.CapacityBytes, g.new)
			}

			cmds := e.cmds()
			rescan := "iscsiadm -m node -T " + testIQN + " -p " + testPortal + " -R"
			rescanAt, growAt := -1, -1
			for i, c := range cmds {
				switch {
				case c == rescan:
					rescanAt = i
				case strings.HasPrefix(c, tc.grow+" "):
					growAt = i
				case strings.HasPrefix(c, "umount"):
					t.Fatalf("expansion unmounted the live volume: %q", c)
				}
				if strings.HasPrefix(c, "mkfs") {
					t.Fatalf("expansion reformatted the volume: %q", c)
				}
				if strings.Contains(c, "--login") || strings.Contains(c, "--logout") {
					t.Fatalf("expansion disturbed the session: %q", c)
				}
			}
			if rescanAt < 0 {
				t.Fatalf("no scoped rescan was issued: %q", cmds)
			}
			if growAt < 0 {
				t.Fatalf("no %s was issued: %q", tc.grow, cmds)
			}
			if rescanAt > growAt {
				t.Fatalf("grew the filesystem before rescanning the device: %q", cmds)
			}
			// resize2fs grows a mounted ext4 by device; xfs_growfs takes the
			// mount point. Either way the live mount stays where it is.
			target := cmds[growAt][len(tc.grow)+1:]
			if tc.fsType == "xfs" && target != volumePath {
				t.Fatalf("xfs_growfs target = %q, want the mount point %q", target, volumePath)
			}
			if tc.fsType == "ext4" && target != testByID {
				t.Fatalf("resize2fs target = %q, want the device %q", target, testByID)
			}
		})
	}
}

func TestExpandNFSIsNoOp(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	volumePath := filepath.Join(t.TempDir(), "globalmount")
	writeMounts(t, root, volumePath)

	resp, err := n.Expand(context.Background(), ExpandRequest{
		VolumeID:       "pvc-abc",
		VolumePath:     volumePath,
		PublishContext: nfsContext(),
		CapacityBytes:  10 << 30,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if resp.CapacityBytes != 10<<30 {
		t.Fatalf("capacity = %d", resp.CapacityBytes)
	}
	// The appliance's refquota change is the whole expansion; the client sees it
	// on the next statfs without any node-side action.
	if got := e.cmds(); len(got) != 0 {
		t.Fatalf("nfs expansion touched the host: %q", got)
	}
}

func TestExpandBlockVolumeIsNoOp(t *testing.T) {
	shortExpandWait(t)
	root := iscsiRoot(t, true)
	g := &growingDevice{old: 1073741824, new: 2147483648}
	e := &fakeExec{handler: g.run}
	n := newTestNode(t, root, e, CapISCSI, CapExt4)

	if _, err := n.Expand(context.Background(), ExpandRequest{
		VolumeID:         "pvc-abc",
		VolumePath:       filepath.Join(t.TempDir(), "pvc-abc"),
		PublishContext:   iscsiContext(),
		VolumeCapability: VolumeCapability{Block: true},
		CapacityBytes:    g.new,
	}); err != nil {
		t.Fatalf("Expand: %v", err)
	}

	rescan := "iscsiadm -m node -T " + testIQN + " -p " + testPortal + " -R"
	sawRescan := false
	for _, c := range e.cmds() {
		if c == rescan {
			sawRescan = true
		}
		if strings.HasPrefix(c, "resize2fs") || strings.HasPrefix(c, "xfs_growfs") {
			t.Fatalf("raw block volume had a filesystem grown on it: %q", c)
		}
	}
	if !sawRescan {
		t.Fatalf("raw block expansion did not rescan the device: %q", e.cmds())
	}
}

// countingBlockdev answers blockdev --getsize64 with a fixed size and counts
// how many times it was asked.
type countingBlockdev struct {
	size  int64
	polls int
}

func (c *countingBlockdev) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	if name == "blockdev" {
		c.polls++
		return []byte(strconv.FormatInt(c.size, 10) + "\n"), nil
	}
	return nil, nil
}

// TestWaitForGrowthStopsOnceTheDeviceIsBigEnough.
//
// The wait used to look only for the size to CHANGE, so a device the kernel had
// already grown before the driver first sampled it looked identical to one that
// never grew: NodeExpandVolume sat out the full 30-second timeout on a volume
// that was ready immediately, then logged "device size did not change after
// rescan" — a warning that fires on the healthy path and is therefore no use
// for spotting the unhealthy one. Both were observed on the real cluster.
func TestWaitForGrowthStopsOnceTheDeviceIsBigEnough(t *testing.T) {
	const want = 2 << 30
	ex := &countingBlockdev{size: want}
	n := &Node{exec: ex}

	restore := expandWaitTimeout
	expandWaitTimeout = 5 * time.Second
	t.Cleanup(func() { expandWaitTimeout = restore })

	start := time.Now()
	got := n.waitForGrowth(context.Background(), "/dev/sdc", want, true, want)
	if got != want {
		t.Fatalf("size = %d, want %d", got, want)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for a device that was already at the requested size", elapsed)
	}
	if ex.polls != 1 {
		t.Fatalf("polled %d times; a device already at the requested size needs one look", ex.polls)
	}
}

// TestWaitForGrowthKeepsWaitingUntilTheRequestedSize: a device that grows but
// is still short of the request is not done. Returning at the first change
// reported a partial expansion as the final answer.
func TestWaitForGrowthKeepsWaitingUntilTheRequestedSize(t *testing.T) {
	const want = 2 << 30
	ex := &countingBlockdev{size: 1 << 30} // grew from 512M, still short of 2G
	n := &Node{exec: ex}

	restore, restorePoll := expandWaitTimeout, expandPollInterval
	expandWaitTimeout, expandPollInterval = 300*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { expandWaitTimeout, expandPollInterval = restore, restorePoll })

	got := n.waitForGrowth(context.Background(), "/dev/sdc", 512<<20, true, want)
	if got != 1<<30 {
		t.Fatalf("size = %d, want the observed %d", got, 1<<30)
	}
	if ex.polls < 2 {
		t.Fatalf("polled %d times; a device short of the request must keep being "+
			"watched rather than accepted at the first change", ex.polls)
	}
}
