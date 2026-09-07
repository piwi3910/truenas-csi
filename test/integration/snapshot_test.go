package integration

import (
	"context"
	"strings"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/volume"
)

// TestE2ESnapshotRestoreIntegrity is the spec's byte-for-byte guarantee.
//
// It writes a 4 MiB random payload, records its md5, snapshots, then
// DELIBERATELY CORRUPTS the source — overwriting the marker file and deleting
// the payload — before restoring from the snapshot into a new volume. The
// restored checksum must equal the original. A restore that quietly returned
// post-snapshot content would pass a weaker test; this one cannot.
func TestE2ESnapshotRestoreIntegrity(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)
	ctx := context.Background()

	src := e.createVolume(t, c, uniqueName("pvc-snapsrc"), "nfs", 1<<30, nil)
	srcMP := "/tmp/e2e-src-" + uniqueName("m")

	out, err := r.Run(ctx, nfsMountScript(src, srcMP, `
echo "IMPORTANT DATA v1" > `+srcMP+`/data.txt
dd if=/dev/urandom of=`+srcMP+`/blob.bin bs=1M count=4 2>/dev/null
echo "ORIGINAL_MD5=$(md5sum `+srcMP+`/blob.bin | cut -d' ' -f1)"
`))
	if err != nil {
		t.Fatalf("writing the source payload: %v\n%s", err, out)
	}
	originalMD5 := extractField(t, out, "ORIGINAL_MD5=")
	t.Logf("original payload md5: %s", originalMD5)

	snapResp, err := c.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: src.GetVolumeId(), Name: uniqueName("snap")})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapID := snapResp.GetSnapshot().GetSnapshotId()
	t.Cleanup(func() {
		_, _ = c.DeleteSnapshot(context.Background(), &csipb.DeleteSnapshotRequest{SnapshotId: snapID})
	})

	// Corrupt the source AFTER the snapshot. If the restore reads live data
	// rather than the snapshot, the checksum below cannot match.
	corrupt, err := r.Run(ctx, nfsMountScript(src, srcMP, `
echo "CORRUPTED v2" > `+srcMP+`/data.txt
rm -f `+srcMP+`/blob.bin
echo "SOURCE_NOW=$(cat `+srcMP+`/data.txt)"
`))
	if err != nil {
		t.Fatalf("corrupting the source: %v\n%s", err, corrupt)
	}
	if !strings.Contains(corrupt, "CORRUPTED v2") {
		t.Fatalf("the source was not actually corrupted, so the test proves nothing:\n%s", corrupt)
	}

	restored := e.createVolume(t, c, uniqueName("pvc-restored"), "nfs", 1<<30,
		&csipb.VolumeContentSource{Type: &csipb.VolumeContentSource_Snapshot{
			Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID}}})
	restMP := "/tmp/e2e-rest-" + uniqueName("m")

	check, err := r.Run(ctx, nfsMountScript(restored, restMP, `
echo "RESTORED_DATA=$(cat `+restMP+`/data.txt)"
echo "RESTORED_MD5=$(md5sum `+restMP+`/blob.bin | cut -d' ' -f1)"
echo "RESTORED_SIZE=$(df -k `+restMP+` | tail -1 | awk '{print $2}')"
`))
	if err != nil {
		t.Fatalf("reading the restored volume: %v\n%s", err, check)
	}

	if got := extractField(t, check, "RESTORED_DATA="); got != "IMPORTANT DATA v1" {
		t.Fatalf("restored content is %q, want the pre-corruption %q", got, "IMPORTANT DATA v1")
	}
	restoredMD5 := extractField(t, check, "RESTORED_MD5=")
	if restoredMD5 != originalMD5 {
		t.Fatalf("restored payload md5 %s does not match the original %s — "+
			"the restore did not return the snapshot's bytes", restoredMD5, originalMD5)
	}
	t.Logf("restored payload md5 matches the original: %s", restoredMD5)

	// A restored clone inherits neither quota nor ownership marker; both must
	// have been set explicitly, or the volume misreports its size and leaks.
	sizeKiB := extractInt(t, check, "RESTORED_SIZE=")
	if sizeKiB > 4*1048576 {
		t.Fatalf("the restored volume reports %d KiB: a clone does not inherit refquota "+
			"and it was not set explicitly", sizeKiB)
	}
}

// TestRestoredCloneIsManaged proves a restored volume is stamped as ours and
// can subsequently be deleted. Without the explicit stamp the delete guard
// refuses it and every restored volume leaks permanently.
func TestRestoredCloneIsManaged(t *testing.T) {
	e := requireAppliance(t)
	c := e.controller(t)
	ctx := context.Background()

	srcName := uniqueName("pvc-clonesrc")
	src := e.createVolume(t, c, srcName, "nfs", 1<<30, nil)

	snapResp, err := c.CreateSnapshot(ctx, &csipb.CreateSnapshotRequest{
		SourceVolumeId: src.GetVolumeId(), Name: uniqueName("snap")})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snapID := snapResp.GetSnapshot().GetSnapshotId()

	restoredName := uniqueName("pvc-restored")
	restored, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: restoredName, Parameters: e.params("nfs"), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{SnapshotId: snapID}}},
	})
	if err != nil {
		t.Fatalf("CreateVolume from snapshot: %v", err)
	}

	ds, err := e.client.DatasetQuery(ctx, e.prefix+"/"+restoredName)
	if err != nil || ds == nil {
		t.Fatalf("querying the restored dataset: %v %v", ds, err)
	}
	if !ds.Owned(volume.OwnerProperty, volume.OwnerValue) {
		t.Fatal("the restored clone carries no LOCAL ownership marker; a ZFS clone " +
			"inherits none, so it must be stamped explicitly or it can never be deleted")
	}
	if ds.RefQuota.Parsed != 1<<30 {
		t.Fatalf("the restored clone's refquota is %d, want %d — a clone inherits no quota",
			ds.RefQuota.Parsed, int64(1)<<30)
	}

	// Deleting the snapshot while the clone depends on it must be refused...
	if _, err := c.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID}); err == nil {
		t.Error("DeleteSnapshot must fail while a restored volume depends on the snapshot")
	}
	// ...and the clone itself must be deletable, which is what the stamp buys.
	if _, err := c.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{
		VolumeId: restored.GetVolume().GetVolumeId()}); err != nil {
		t.Fatalf("the restored volume could not be deleted: %v", err)
	}
	if _, err := c.DeleteSnapshot(ctx, &csipb.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("the snapshot could not be deleted once its clone was gone: %v", err)
	}
}

// TestExpandRejectsShrink covers both backends. The appliance refuses a zvol
// shrink itself but silently permits a refquota shrink, so the NFS guard is
// entirely the driver's responsibility.
func TestExpandRejectsShrink(t *testing.T) {
	e := requireAppliance(t)
	c := e.controller(t)
	ctx := context.Background()

	for _, protocol := range []string{"nfs", "iscsi"} {
		t.Run(protocol, func(t *testing.T) {
			vol := e.createVolume(t, c, uniqueName("pvc-shrink-"+protocol), protocol, 1<<30, nil)

			grown, err := c.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
				VolumeId:      vol.GetVolumeId(),
				CapacityRange: &csipb.CapacityRange{RequiredBytes: 2 << 30}})
			if err != nil {
				t.Fatalf("grow: %v", err)
			}
			if grown.GetCapacityBytes() < 2<<30 {
				t.Fatalf("grew to %d, want at least %d", grown.GetCapacityBytes(), int64(2)<<30)
			}
			if _, err := c.ControllerExpandVolume(ctx, &csipb.ControllerExpandVolumeRequest{
				VolumeId:      vol.GetVolumeId(),
				CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30}}); err == nil {
				t.Fatal("shrink must be rejected by the driver")
			}
		})
	}
}

func extractField(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	t.Fatalf("could not find %q in node output:\n%s", prefix, out)
	return ""
}
