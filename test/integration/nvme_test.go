package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// TestE2ENVMeProvision provisions an NVMe-oF volume through the real controller
// and attaches it from a cluster node over NVMe/TCP.
//
// Device resolution is the part worth proving on real hardware: the test node
// has its own local NVMe disk, so any code that assumed an index — or listed
// /dev — would find the wrong device and, at worst, format the node's own disk.
func TestE2ENVMeProvision(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	vol := e.createVolume(t, c, uniqueName("pvc-e2e-nvme"), "nvme", 1<<30, nil)
	ctx := vol.GetVolumeContext()
	portal, nqn, serial := ctx["portal"], ctx["nqn"], ctx["serial"]
	if portal == "" || nqn == "" || serial == "" {
		t.Fatalf("the volume context is missing NVMe attachment details: %v", redactedKeys(ctx))
	}
	if strings.HasPrefix(portal, "0.0.0.0") || strings.HasPrefix(portal, "[::]") {
		t.Fatalf("portal %q is a listen wildcard, which no initiator can dial", portal)
	}
	host, port := splitHostPort(portal)
	mp := "/tmp/e2e-nvme-" + uniqueName("m")

	script := fmt.Sprintf(`set -e
modprobe nvme_tcp 2>/dev/null || true
BEFORE=$(ls /dev/nvme*n1 2>/dev/null | wc -l)
nvme discover -t tcp -a %[1]s -s %[2]s >/dev/null 2>&1 || true
nvme connect -t tcp -a %[1]s -s %[2]s -n '%[3]s' >/dev/null
sleep 3
DEV=$(readlink -f /dev/disk/by-id/$(ls /dev/disk/by-id 2>/dev/null | grep '%[4]s' | head -1) 2>/dev/null)
echo "DEVICE=$DEV"
[ -b "$DEV" ] || { echo "NO_BLOCK_DEVICE"; nvme disconnect -n '%[3]s' >/dev/null 2>&1; exit 1; }
echo "SIZE=$(blockdev --getsize64 $DEV)"
mkfs.ext4 -F -q "$DEV"
mkdir -p %[5]s && mount "$DEV" %[5]s
echo "nvme payload" > %[5]s/n.txt
cat %[5]s/n.txt
umount %[5]s; rmdir %[5]s
nvme disconnect -n '%[3]s' >/dev/null
echo "LOCAL_DISKS_BEFORE=$BEFORE"
echo "LOCAL_DISKS_AFTER=$(ls /dev/nvme*n1 2>/dev/null | wc -l)"`,
		host, port, nqn, serial, mp)

	out, err := r.Run(context.Background(), script)
	if err != nil {
		t.Fatalf("attaching the NVMe volume failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nvme payload") {
		t.Fatalf("could not read back what was written:\n%s", out)
	}
	if got := extractInt(t, out, "SIZE="); got != 1<<30 {
		t.Errorf("device reports %d bytes, want %d", got, int64(1)<<30)
	}
	before := extractInt(t, out, "LOCAL_DISKS_BEFORE=")
	after := extractInt(t, out, "LOCAL_DISKS_AFTER=")
	if before != after {
		t.Fatalf("the node's NVMe device count changed from %d to %d across the test — "+
			"the disconnect did not release only our subsystem", before, after)
	}
	t.Logf("NVMe/TCP volume attached, formatted and read back; node-local disks untouched")
}

// splitHostPort separates "host:port", defaulting to the NVMe-oF port.
func splitHostPort(addr string) (string, string) {
	if i := strings.LastIndex(addr, ":"); i > 0 && !strings.HasSuffix(addr, "]") {
		return addr[:i], addr[i+1:]
	}
	return addr, "4420"
}
