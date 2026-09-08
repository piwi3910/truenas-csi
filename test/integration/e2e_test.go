package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
)

// requireNode gives the tests a real Linux host to mount on.
func requireNode(t *testing.T) *NodeRunner {
	t.Helper()
	r, why := NewNodeRunner()
	if r == nil {
		t.Skip(why)
	}
	return r
}

// createVolume provisions through the real controller and registers cleanup.
func (e *env) createVolume(t *testing.T, c csipb.ControllerServer, name, protocol string,
	bytes int64, source *csipb.VolumeContentSource) *csipb.Volume {
	t.Helper()
	req := &csipb.CreateVolumeRequest{
		Name: name, Parameters: e.params(protocol), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: bytes}, VolumeContentSource: source,
	}
	resp, err := c.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolume(%s/%s): %v", protocol, name, err)
	}
	id := resp.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		if _, err := c.DeleteVolume(context.Background(),
			&csipb.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Errorf("cleanup DeleteVolume(%s): %v", id, err)
		}
	})
	return e.publish(t, c, resp.GetVolume())
}

// publish grants the node appliance-side access and revokes it on cleanup.
//
// Every volume is created FENCED -- an NFS export carries only the unroutable
// deny host, an iSCSI extent is mapped to no LUN, an SMB share denies everyone
// -- so nothing can reach a volume until ControllerPublishVolume grants the
// node. A test that mounted without publishing would be exercising the fence
// rather than the data path, and would fail in a thoroughly misleading way: an
// NFS server answers a host outside its access list with a bare
// "No such file or directory", which reads like a missing dataset.
func (e *env) publish(t *testing.T, c csipb.ControllerServer, vol *csipb.Volume) *csipb.Volume {
	t.Helper()
	if e.nodeID == "" {
		return vol
	}
	id := vol.GetVolumeId()
	resp, err := c.ControllerPublishVolume(context.Background(), &csipb.ControllerPublishVolumeRequest{
		VolumeId: id, NodeId: e.nodeID, VolumeCapability: caps()[0],
	})
	if err != nil {
		t.Fatalf("ControllerPublishVolume(%s -> %s): %v", id, e.nodeID, err)
	}
	t.Cleanup(func() {
		if _, err := c.ControllerUnpublishVolume(context.Background(),
			&csipb.ControllerUnpublishVolumeRequest{VolumeId: id, NodeId: e.nodeID}); err != nil {
			t.Errorf("cleanup ControllerUnpublishVolume(%s): %v", id, err)
		}
	})
	merged := map[string]string{}
	for k, v := range vol.GetVolumeContext() {
		merged[k] = v
	}
	for k, v := range resp.GetPublishContext() {
		merged[k] = v
	}
	return &csipb.Volume{
		VolumeId: id, CapacityBytes: vol.GetCapacityBytes(), VolumeContext: merged,
		ContentSource: vol.GetContentSource(), AccessibleTopology: vol.GetAccessibleTopology(),
	}
}

// nfsMountScript builds a script that mounts an NFS volume on the node.
func nfsMountScript(vol *csipb.Volume, mountpoint, body string) string {
	ctx := vol.GetVolumeContext()
	return fmt.Sprintf(`
set -e
mkdir -p %[3]s
mount -t nfs -o vers=%[4]s %[1]s:%[2]s %[3]s
%[5]s
umount %[3]s
rmdir %[3]s
`, ctx["server"], ctx["share"], mountpoint, orDefault(ctx["nfsVersion"], "4"), body)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// TestE2ENFSProvision provisions an NFS volume and proves a pod-equivalent can
// read back exactly what it wrote, and that the volume reports its own size
// rather than the whole pool.
func TestE2ENFSProvision(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	vol := e.createVolume(t, c, uniqueName("pvc-e2e-nfs"), "nfs", 1<<30, nil)
	mp := "/tmp/e2e-nfs-" + uniqueName("m")

	out, err := r.Run(context.Background(), nfsMountScript(vol, mp, `
echo "hello from the csi e2e test" > `+mp+`/data.txt
cat `+mp+`/data.txt
echo "REPORTED_SIZE=$(df -k `+mp+` | tail -1 | awk '{print $2}')"
`))
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "hello from the csi e2e test") {
		t.Fatalf("did not read back what was written:\n%s", out)
	}
	// 1 GiB is 1048576 KiB. The pool is ~44 TB, so a missing refquota shows up
	// as a size three orders of magnitude too large.
	sizeKiB := extractInt(t, out, "REPORTED_SIZE=")
	if sizeKiB > 4*1048576 {
		t.Fatalf("volume reports %d KiB, far larger than the 1 GiB requested — "+
			"refquota is not being applied and the pod can see the whole pool", sizeKiB)
	}
	t.Logf("NFS volume reported %d KiB for a 1 GiB request", sizeKiB)
}

// TestE2ENFSNonRootWrite is the check that catches the most common NFS-CSI
// failure: a fresh dataset is root:root 0755 and a non-root pod cannot write.
func TestE2ENFSNonRootWrite(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	vol := e.createVolume(t, c, uniqueName("pvc-e2e-nonroot"), "nfs", 1<<30, nil)
	mp := "/tmp/e2e-nonroot-" + uniqueName("m")

	out, err := r.Run(context.Background(), nfsMountScript(vol, mp, `
if setpriv --reuid=1000 --regid=1000 --clear-groups sh -c 'echo written-as-uid-1000 > `+mp+`/user.txt'; then
  echo NONROOT_WRITE=ok
  cat `+mp+`/user.txt
else
  echo NONROOT_WRITE=denied
fi
ls -ld `+mp+`
`))
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NONROOT_WRITE=ok") {
		t.Fatalf("a non-root user could not write to a freshly provisioned NFS volume. "+
			"Permissions must be set on the appliance at provisioning time; fsGroup does "+
			"not apply to NFS.\n%s", out)
	}
}

// TestE2EISCSIProvision attaches a real iSCSI volume, formats and mounts it,
// and proves the device is found deterministically from the NAA.
func TestE2EISCSIProvision(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	vol := e.createVolume(t, c, uniqueName("pvc-e2e-iscsi"), "iscsi", 1<<30, nil)
	ctx := vol.GetVolumeContext()
	mp := "/tmp/e2e-iscsi-" + uniqueName("m")

	out, err := r.Run(context.Background(), iscsiScript(ctx, mp, `
echo "iscsi payload" > `+mp+`/data.txt
cat `+mp+`/data.txt
`))
	if err != nil {
		t.Fatalf("node script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "iscsi payload") {
		t.Fatalf("could not read back what was written to the iSCSI volume:\n%s", out)
	}
	if !strings.Contains(out, "DEVICE=/dev/") {
		t.Fatalf("device was not resolved from the NAA:\n%s", out)
	}
	// Longhorn shares this node's iSCSI stack and must be untouched.
	if strings.Contains(out, "LONGHORN_SESSIONS=0") {
		t.Error("the node had Longhorn sessions before and none after — " +
			"our iscsiadm calls are not properly scoped")
	}
}

// iscsiScript logs in by IQN and portal only, resolves the device strictly from
// the NAA, and logs out again — never touching another target's sessions.
func iscsiScript(ctx map[string]string, mountpoint, body string) string {
	naa := strings.TrimPrefix(ctx["naa"], "0x")
	login := ""
	if ctx["chapUser"] != "" && ctx["chapSecret"] != "" {
		login = fmt.Sprintf(`
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.authmethod -v CHAP
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.username -v '%[3]s'
iscsiadm -m node -T %[1]s -p %[2]s --op update -n node.session.auth.password -v '%[4]s'
`, ctx["iqn"], ctx["portal"], ctx["chapUser"], ctx["chapSecret"])
	}
	return fmt.Sprintf(`
set -e
BEFORE=$(iscsiadm -m session 2>/dev/null | grep -c longhorn || true)
iscsiadm -m discovery -t sendtargets -p %[1]s >/dev/null
%[5]s
iscsiadm -m node -T %[2]s -p %[1]s --login >/dev/null
sleep 3
DEV=$(readlink -f /dev/disk/by-id/scsi-3%[3]s)
echo "DEVICE=$DEV"
mkfs.ext4 -F -q "$DEV"
mkdir -p %[4]s
mount "$DEV" %[4]s
%[6]s
umount %[4]s
rmdir %[4]s
iscsiadm -m node -T %[2]s -p %[1]s --logout >/dev/null
iscsiadm -m node -o delete -T %[2]s -p %[1]s >/dev/null 2>&1 || true
AFTER=$(iscsiadm -m session 2>/dev/null | grep -c longhorn || true)
echo "LONGHORN_SESSIONS=$AFTER"
[ "$BEFORE" = "$AFTER" ] || echo "LONGHORN_SESSIONS_CHANGED=$BEFORE->$AFTER"
`, ctx["portal"], ctx["iqn"], naa, mountpoint, login, body)
}

// TestE2ECHAPAttach proves a CHAP-protected target attaches. CHAP is on by
// default, so the ordinary iSCSI path already exercises it; this asserts the
// credentials really are present and used.
func TestE2ECHAPAttach(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)

	vol := e.createVolume(t, c, uniqueName("pvc-e2e-chap"), "iscsi", 1<<30, nil)
	ctx := vol.GetVolumeContext()
	if ctx["chapUser"] == "" || ctx["chapSecret"] == "" {
		t.Fatalf("CHAP is on by default but the volume context carries no credentials: %v",
			redactedKeys(ctx))
	}
	if len(ctx["chapSecret"]) < 12 {
		t.Fatalf("CHAP secret is only %d characters", len(ctx["chapSecret"]))
	}
	mp := "/tmp/e2e-chap-" + uniqueName("m")
	out, err := r.Run(context.Background(), iscsiScript(ctx, mp, `echo chap-ok > `+mp+`/c.txt; cat `+mp+`/c.txt`))
	if err != nil {
		t.Fatalf("attaching a CHAP-protected target failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "chap-ok") {
		t.Fatalf("CHAP attach did not produce a usable filesystem:\n%s", out)
	}
}

// redactedKeys lists a context's keys without their values, so a failure
// message can never print a credential.
func redactedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func extractInt(t *testing.T, out, prefix string) int64 {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		var v int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, prefix), "%d", &v); err == nil {
			return v
		}
	}
	t.Fatalf("could not find %q in node output:\n%s", prefix, out)
	return 0
}
