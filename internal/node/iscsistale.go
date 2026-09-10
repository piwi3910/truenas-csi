package node

// Stale iSCSI devices, and why they are the driver's problem.
//
// Every volume this driver provisions is a LUN on ONE SHARED target, and the
// appliance hands out the lowest free LUN id. So LUN ids are RECYCLED: delete a
// volume and the next volume created takes its number.
//
// A node that logged in to the target keeps a SCSI device per LUN for as long as
// the session lives, and the session outlives any single volume — it is shared.
// Nothing in the iSCSI protocol tells a live session that LUN 0 now means a
// different disk, so unless the node deletes the device when it finishes with
// the volume, it keeps a device that names a LUN whose contents have changed
// underneath it.
//
// Measured on the cluster: worker-23 held sdb for LUN 0 and sdd for LUN 2, both
// left over from volumes deleted earlier, while the appliance was serving two
// different, live volumes at those LUNs. The next volume to land on LUN 0 could
// never be staged there — its by-id link never appeared, so every attempt ended
// in "block device did not appear after attach" and the pod stayed Pending for
// ever, on that node only, with nothing in the message hinting at a stale
// device.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// dropStaleTargetDevices deletes the kernel's SCSI devices for LUNs of one
// target that are neither mounted nor the device wanted, then asks the session
// to rediscover them.
//
// It is deliberately narrow. A device is removed only when all three hold:
//
//   - it sits at the LUN this volume was mapped to on OUR portal and target.
//     That is the only device that can be in our way, so it is the only one
//     considered: another initiator's session, the host's own disks, and every
//     other LUN of our own target are all out of scope;
//   - its wwid is not the NAA being resolved, so the volume being staged is
//     never removed;
//   - nothing on the host has it mounted, directly or as a multipath member,
//     which is what makes the removal safe — a stale device by definition
//     carries nothing.
//
// The LUN restriction is what keeps this from being destructive. Removing every
// unmounted device of the target would also take devices that are perfectly
// valid and merely idle — one staged but not yet mounted, or a raw block volume
// whose consumer is starting — and racing a sibling's stage is not a price
// worth paying to fix our own.
//
// It returns the number of devices removed, so the caller can skip a rescan it
// does not need.
func (n *Node) dropStaleTargetDevices(ctx context.Context, portal, iqn, wantNAA, lun string) int {
	if lun == "" {
		// Without a LUN there is no way to name the one device in our way, and
		// guessing wider is how a fix becomes an outage.
		return 0
	}
	devices, err := n.devicesAtLUN(portal, iqn, lun)
	if err != nil || len(devices) == 0 {
		return 0
	}
	entries, err := n.mounts()
	if err != nil {
		// Cannot prove a device is unused, so remove nothing.
		return 0
	}
	inUse := map[string]bool{}
	for _, e := range entries {
		for _, name := range n.deviceNamesOf(e.source) {
			inUse[name] = true
		}
	}

	want := normalizeNAA(wantNAA)
	removed := 0
	for name := range devices {
		if inUse[name] {
			continue
		}
		if id := n.deviceWWID(name); id == "" || id == want {
			// Unknown identity is left alone: "could not ask" is not "stale".
			continue
		}
		if err := n.deleteSCSIDevice(name); err != nil {
			obs.Logger(ctx).Warn("could not remove a stale iSCSI device",
				"device", name, "target", iqn, "error", obs.Redact(err.Error()))
			continue
		}
		obs.Logger(ctx).Info("removed a stale iSCSI device: it holds a LUN of this target "+
			"whose contents the appliance has since reassigned, and nothing on this node "+
			"has it mounted", "device", name, "target", iqn)
		removed++
	}
	return removed
}

// dropDeviceForNAA removes the kernel's SCSI devices for one volume, so the LUN
// id it occupied can be recycled without leaving a device that lies about what
// it holds.
//
// This runs at unstage, when the staging mount is already gone. Multipath maps
// are flushed first: deleting the paths under a live map leaves a map with no
// usable path, which is worse than either.
func (n *Node) dropDeviceForNAA(ctx context.Context, naa string) {
	id := normalizeNAA(naa)
	if id == "" {
		return
	}
	link := filepath.Join(n.hostRoot(), "dev", "disk", "by-id", "scsi-3"+id)
	dest, err := os.Readlink(link)
	if err != nil {
		// No link: nothing of ours is left for the kernel to hold.
		return
	}
	name := filepath.Base(dest)

	if n.pre.Found[CapMultipath] {
		if mapper, ok, mErr := multipathDevice(ctx, n.exec, naa); mErr == nil && ok {
			if _, fErr := n.exec.Run(ctx, "multipath", "-f", filepath.Base(mapper)); fErr != nil {
				obs.Logger(ctx).Warn("could not flush the multipath map before removing its paths",
					"map", mapper, "error", obs.Redact(fErr.Error()))
			}
		}
	}

	for _, dev := range n.deviceNamesOf("/dev/" + name) {
		if strings.HasPrefix(dev, "dm-") {
			// A device-mapper node is removed by flushing the map, not by the
			// SCSI delete attribute, which it does not have.
			continue
		}
		if err := n.deleteSCSIDevice(dev); err != nil {
			obs.Logger(ctx).Warn("could not remove this volume's iSCSI device; its LUN id may "+
				"be reused by a later volume that then cannot be staged on this node",
				"device", dev, "error", obs.Redact(err.Error()))
			continue
		}
		obs.Logger(ctx).Info("removed this volume's iSCSI device so its LUN id can be recycled",
			"device", dev)
	}
}

// deleteSCSIDevice asks the kernel to forget one SCSI disk.
//
// The write goes to sysfs directly rather than through a helper binary because
// there is no helper to depend on: this attribute IS the interface, present on
// every kernel that can speak iSCSI at all.
func (n *Node) deleteSCSIDevice(name string) error {
	path := filepath.Join(n.hostRoot(), "sys", "block", name, "device", "delete")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("1"); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// devicesAtLUN returns the kernel names of the devices the host has for ONE LUN
// of one portal and target, read from the by-path link the kernel maintains for
// exactly that triple.
func (n *Node) devicesAtLUN(portal, iqn, lun string) (map[string]bool, error) {
	dir := filepath.Join(n.hostRoot(), "dev", "disk", "by-path")
	link := filepath.Join(dir, "ip-"+portal+"-iscsi-"+iqn+"-lun-"+lun)
	dest, err := os.Readlink(link)
	if err != nil {
		return nil, err
	}
	return map[string]bool{filepath.Base(dest): true}, nil
}

// deviceWWID reads a disk's identity as the kernel reports it, normalised to the
// spelling the publish context uses. "" when it cannot be read.
func (n *Node) deviceWWID(name string) string {
	b, err := os.ReadFile(filepath.Join(n.hostRoot(), "sys", "block", name, "device", "wwid"))
	if err != nil {
		return ""
	}
	// The kernel spells it "naa.6589cfc…"; the publish context spells the same
	// value "0x6589cfc…".
	return normalizeNAA(strings.TrimPrefix(strings.TrimSpace(string(b)), "naa."))
}

// stagedDeviceIdentity reports the identity the appliance currently serves at
// one LUN of one target, and whether it could be established.
//
// The device is located by its LUN — the SLOT — and not by any name derived
// from its identity, because the identity is precisely what has changed. The
// by-id link and the wwid cached in sysfs both go on naming the previous volume
// for as long as nothing makes the kernel ask again: measured on the cluster,
// after the appliance moved a LUN to a different extent, sysfs reported the old
// wwid indefinitely and the by-id link still pointed at that disk.
//
// So the kernel is made to ask. Writing to the device's rescan attribute is one
// INQUIRY, the same mechanism online expansion already uses to see a new size,
// and afterwards sysfs holds what the appliance is actually serving.
func (n *Node) stagedDeviceIdentity(portal, iqn, lun string) (string, bool) {
	devices, err := n.devicesAtLUN(portal, iqn, lun)
	if err != nil || len(devices) != 1 {
		return "", false
	}
	var name string
	for d := range devices {
		name = d
	}
	if err := n.rescanSCSIDevice(name); err != nil {
		// Without a fresh read there is nothing to compare: the cached value
		// would agree with itself no matter what the appliance is serving.
		return "", false
	}
	got := n.deviceWWID(name)
	if got == "" {
		return "", false
	}
	return got, true
}

// rescanSCSIDevice makes the kernel re-read one disk's capacity and identity.
func (n *Node) rescanSCSIDevice(name string) error {
	path := filepath.Join(n.hostRoot(), "sys", "block", name, "device", "rescan")
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString("1"); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
