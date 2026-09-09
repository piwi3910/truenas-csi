package node

// Per-volume I/O limits, applied on the NODE with cgroup v2 io.max.
//
// WHY THIS LIVES ON THE NODE AND NOT ON THE APPLIANCE
//
// TrueNAS/OpenZFS has no per-dataset or per-zvol IOPS or bandwidth limiter.
// There is no ZFS property, no middleware call and no share option that caps a
// single volume's throughput, so there is nothing the controller could ask the
// appliance for. That was verified while comparing this driver against Dell
// CSM, where PowerFlex applies bandwidthLimitInKbps/iopsLimit array-side.
//
// What the appliance cannot do, the node can — but only for the block
// protocols. An iSCSI or NVMe/TCP volume is a real block device on this node,
// and cgroup v2's io.max throttles a cgroup's traffic to one device by its
// major:minor. NFS and SMB volumes are mounts, not devices: the I/O leaves the
// node as network traffic through the kernel's RPC or cifs client and never
// passes through blk-throttle at all. For those protocols this feature is
// refused loudly rather than accepted and silently ignored — see checkIOLimits.
//
// WHAT THIS IS NOT
//
// This is a per-pod, per-node throttle. It is NOT appliance-side QoS:
//
//   - Another pod, on another node, hammering the same appliance is unaffected.
//   - Nothing here protects the appliance from aggregate load. Ten pods each
//     capped at 100 MB/s can still ask the pool for 1 GB/s.
//   - The limit does not travel with the volume. It is written into a cgroup on
//     this node; if the pod reschedules elsewhere, the new node applies it again
//     at its own NodePublishVolume, and until that call the volume is unthrottled
//     on the new node.
//   - A pod restart re-applies the limit, because the kubelet creates a fresh pod
//     cgroup and calls NodePublishVolume again.
//
// FAILURE POLICY
//
// A limit that cannot be applied is logged and the publish continues. A missing
// throttle is a performance problem; a failed NodePublishVolume is an outage.
// The one exception is a limit that can never work at all — a limit on an NFS or
// SMB volume — which is a configuration mistake and is reported as one.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/piwi3910/truenas-csi/internal/obs"
)

// Volume-context keys carrying the per-volume I/O limits.
//
// They are named for what they cap and in what direction, and their values are
// Kubernetes-style quantities. PowerFlex's bandwidthLimitInKbps is the obvious
// precedent and was deliberately not copied: encoding the unit in the NAME
// forces every user to do a conversion in their head and revives the
// kilobit-versus-kilobyte ambiguity, while a Kubernetes user already writes
// "10Gi" for storage a few lines above in the same StorageClass. Bandwidth here
// is therefore BYTES PER SECOND written as a quantity ("100Mi", "1G", "2000000")
// and IOPS is operations per second.
//
// The undirected keys are the common case — most people want "this volume gets
// at most 100 MB/s" — and the directional keys override them per direction,
// because a database that must not swamp the array on flush usually wants a
// tighter write cap than read cap.
//
// An empty value, "0" and "max" all mean "no limit", so a limit can be turned
// off in a StorageClass without deleting the line.
const (
	// KeyBandwidthLimit caps reads and writes alike, in bytes per second.
	KeyBandwidthLimit = "bandwidthLimit"
	// KeyReadBandwidthLimit caps reads only, in bytes per second. It overrides
	// KeyBandwidthLimit.
	KeyReadBandwidthLimit = "readBandwidthLimit"
	// KeyWriteBandwidthLimit caps writes only, in bytes per second. It overrides
	// KeyBandwidthLimit.
	KeyWriteBandwidthLimit = "writeBandwidthLimit"
	// KeyIOPSLimit caps reads and writes alike, in operations per second.
	KeyIOPSLimit = "iopsLimit"
	// KeyReadIOPSLimit caps read operations per second. It overrides KeyIOPSLimit.
	KeyReadIOPSLimit = "readIOPSLimit"
	// KeyWriteIOPSLimit caps write operations per second. It overrides KeyIOPSLimit.
	KeyWriteIOPSLimit = "writeIOPSLimit"
)

// NodeParameterKeys are the StorageClass parameters the CONTROLLER must copy
// into a volume's context so the node can see them.
//
// It is an allowlist rather than a blanket echo of req.GetParameters(): a
// StorageClass carries backend selection, share options and credential
// references, and the node has no business receiving those. Every key here is
// read by this package and by nothing else.
//
// Without the copy these parameters reach the node only on a STATIC
// PersistentVolume, whose spec.csi.volumeAttributes the operator writes by
// hand -- a dynamically provisioned volume would accept the parameter, report
// success, and apply no limit at all.
var NodeParameterKeys = []string{
	KeyBandwidthLimit, KeyReadBandwidthLimit, KeyWriteBandwidthLimit,
	KeyIOPSLimit, KeyReadIOPSLimit, KeyWriteIOPSLimit,
}

// KeyPodUID is the pod's UID, filled in by the kubelet at NodePublishVolume
// because the CSIDriver object sets podInfoOnMount: true. It is the only thing
// that names the cgroup to write the limit into, which is why the limit is
// applied at publish and nowhere else: NodeStageVolume does not know which pod
// — or how many pods — the volume is for.
const KeyPodUID = "csi.storage.k8s.io/pod.uid"

// IOLimits is one volume's throttle, as cgroup v2 expresses it. A zero field is
// "no limit" and renders as "max", which is also how an existing limit is
// cleared.
type IOLimits struct {
	// ReadBPS and WriteBPS are bytes per second.
	ReadBPS, WriteBPS uint64
	// ReadIOPS and WriteIOPS are operations per second.
	ReadIOPS, WriteIOPS uint64
}

// Empty reports whether nothing at all is capped, in which case there is no
// reason to touch a cgroup.
func (l IOLimits) Empty() bool {
	return l == IOLimits{}
}

// MaxLine renders the io.max line for one device.
//
// The kernel accepts a line naming only the keys you care about, but all four
// are always written: a republish with a relaxed limit must CLEAR the value it
// is replacing, and a line that omits a key leaves that key's previous value in
// force. Writing "max" explicitly makes the write idempotent and total.
//
// The exact bytes matter. The kernel parses this line strictly but a value it
// cannot use is not always an error the writer sees, so a malformed line can be
// accepted as a no-op — a throttle that silently does nothing. That is why the
// tests assert the rendered string byte for byte.
func (l IOLimits) MaxLine(major, minor uint32) string {
	return fmt.Sprintf("%d:%d rbps=%s wbps=%s riops=%s wiops=%s\n",
		major, minor,
		limitValue(l.ReadBPS), limitValue(l.WriteBPS),
		limitValue(l.ReadIOPS), limitValue(l.WriteIOPS))
}

// limitValue spells one io.max value: a number, or "max" for no limit.
func limitValue(v uint64) string {
	if v == 0 {
		return "max"
	}
	return strconv.FormatUint(v, 10)
}

// ParseIOLimits reads the limit keys out of a volume context.
//
// It returns the zero IOLimits and no error when the context names no limit at
// all, which is the overwhelmingly common case and must stay free.
func ParseIOLimits(vc map[string]string) (IOLimits, error) {
	both, err := parseQuantity(vc, KeyBandwidthLimit)
	if err != nil {
		return IOLimits{}, err
	}
	bothIOPS, err := parseQuantity(vc, KeyIOPSLimit)
	if err != nil {
		return IOLimits{}, err
	}
	out := IOLimits{ReadBPS: both, WriteBPS: both, ReadIOPS: bothIOPS, WriteIOPS: bothIOPS}

	for _, f := range []struct {
		key string
		dst *uint64
	}{
		{KeyReadBandwidthLimit, &out.ReadBPS},
		{KeyWriteBandwidthLimit, &out.WriteBPS},
		{KeyReadIOPSLimit, &out.ReadIOPS},
		{KeyWriteIOPSLimit, &out.WriteIOPS},
	} {
		// Only a key that is PRESENT overrides the undirected value; an absent
		// key must not reset it to zero.
		if _, ok := vc[f.key]; !ok {
			continue
		}
		v, err := parseQuantity(vc, f.key)
		if err != nil {
			return IOLimits{}, err
		}
		*f.dst = v
	}
	return out, nil
}

// quantitySuffixes are the multipliers a value may carry. They are the ones a
// Kubernetes user already types for resource quantities, so "100Mi" means the
// same here as it does two lines above in the same StorageClass.
var quantitySuffixes = []struct {
	suffix string
	mult   uint64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
	{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
	{"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12},
}

// parseQuantity reads one limit value. Empty, "0" and "max" all mean no limit.
//
// Only whole numbers are accepted. "1.5Gi" is rejected rather than rounded:
// this value becomes a kernel throttle, and a user who wrote a fraction should
// find out at the first pod start, not by measuring the result.
func parseQuantity(vc map[string]string, key string) (uint64, error) {
	raw := strings.TrimSpace(vc[key])
	if raw == "" || raw == "0" || raw == "max" {
		return 0, nil
	}
	digits, mult := raw, uint64(1)
	for _, s := range quantitySuffixes {
		if strings.HasSuffix(raw, s.suffix) {
			digits, mult = strings.TrimSpace(strings.TrimSuffix(raw, s.suffix)), s.mult
			break
		}
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s=%q is not a whole number with an optional "+
			"Ki/Mi/Gi/Ti or K/M/G/T suffix", ErrInvalidRequest, key, raw)
	}
	if n != 0 && mult != 0 && n > ^uint64(0)/mult {
		return 0, fmt.Errorf("%w: %s=%q overflows", ErrInvalidRequest, key, raw)
	}
	return n * mult, nil
}

// checkIOLimits validates the limit keys and refuses the ones that can never be
// enforced.
//
// An NFS or SMB volume is refused, not warned about. The choice is deliberate:
// a warning lands in the node plugin's log, which is exactly where nobody is
// looking, and the user is then running a workload they believe is capped. The
// failure mode of the friendlier option is discovering during an incident that
// the isolation you configured never existed. Refusing costs a pod that will
// not start, with a message naming the parameter and the reason — and it can
// only ever fire for someone who has just added the parameter, since no
// StorageClass that predates this feature carries it.
func checkIOLimits(protocol string, vc map[string]string) error {
	limits, err := ParseIOLimits(vc)
	if err != nil {
		return err
	}
	if limits.Empty() {
		return nil
	}
	switch protocol {
	case ProtocolNFS, ProtocolSMB:
		return fmt.Errorf("%w: per-volume I/O limits (%s, %s and their read/write forms) "+
			"cannot be enforced for the %s protocol: the volume is a network mount, not a "+
			"block device, so there is no device for cgroup v2 io.max to throttle. Remove the "+
			"limit from the StorageClass, or use the iscsi or nvme protocol",
			ErrInvalidRequest, KeyBandwidthLimit, KeyIOPSLimit, protocol)
	}
	return nil
}

// cgroupRoot is the host-absolute cgroup v2 mount point. It is a variable so a
// test can point it at a fixture tree; on a real node it is never anything else,
// because a unified (cgroup v2) hierarchy is mounted exactly here.
var cgroupRoot = "/sys/fs/cgroup"

// io.max is the cgroup v2 blk-throttle knob. It exists only under a unified
// hierarchy and only when the io controller is enabled in the parent's
// cgroup.subtree_control, which the kubelet does for pods that ask for I/O
// isolation and systemd does by default under kubepods.slice.
const ioMaxFile = "io.max"

// applyIOLimits writes one pod's throttle for one volume's device.
//
// Every failure here is a warning and a return, never an error: see the failure
// policy at the top of this file. The caller has already published the volume,
// so the pod is about to start; refusing to start it because a throttle could
// not be written would turn a performance control into an availability risk.
//
// There is deliberately no matching teardown in Unpublish. A cgroup file has no
// life of its own: io.max lives inside the pod's cgroup directory, and the
// kubelet removes that entire directory when the pod's sandbox is torn down.
// Clearing the value first would be writing to something that is about to be
// unlinked — and worse, it would be wrong while the pod is merely restarting a
// container inside a cgroup that stays put. Nothing to undo, so nothing is done.
func (n *Node) applyIOLimits(ctx context.Context, req PublishRequest, protocol string) {
	limits, err := ParseIOLimits(req.PublishContext)
	if err != nil || limits.Empty() {
		return // already reported by checkIOLimits; an invalid value never reaches here
	}
	lg := obs.Logger(ctx)

	podUID := strings.TrimSpace(req.PublishContext[KeyPodUID])
	if podUID == "" {
		lg.Warn("per-volume I/O limit not applied: the volume context carries no pod UID",
			"hint", "the CSIDriver object must set podInfoOnMount: true")
		return
	}

	device, err := n.limitDevice(ctx, req, protocol)
	if err != nil {
		lg.Warn("per-volume I/O limit not applied: the volume's block device could not be resolved",
			"error", obs.Redact(err.Error()))
		return
	}
	// Every path the node hands to a host binary is host-absolute; to STAT it
	// from inside this container it has to be re-rooted at the mounted host root.
	major, minor, err := deviceNumbers(filepath.Join(n.hostRoot(), device))
	if err != nil {
		lg.Warn("per-volume I/O limit not applied: the device's major:minor could not be read",
			"device", device, "error", obs.Redact(err.Error()))
		return
	}

	dir, err := resolvePodCgroup(filepath.Join(n.hostRoot(), cgroupRoot), podUID)
	if err != nil {
		lg.Warn("per-volume I/O limit not applied: the pod's cgroup could not be found",
			"podUID", podUID, "error", obs.Redact(err.Error()))
		return
	}

	line := limits.MaxLine(major, minor)
	if err := writeCgroupFile(filepath.Join(dir, ioMaxFile), line); err != nil {
		lg.Warn("per-volume I/O limit not applied: io.max could not be written",
			"cgroup", dir, "error", obs.Redact(err.Error()))
		return
	}
	lg.Info("per-volume I/O limit applied",
		"cgroup", dir, "device", device,
		"limit", strings.TrimSpace(line),
		"scope", "this pod on this node only; the appliance enforces nothing")
}

// limitDevice names the block device a volume's traffic goes through, which is
// the only thing io.max can be keyed on.
//
// It reuses the very same resolution the data path uses, multipath included: a
// throttle written against the single path of a device the pod reaches through
// the mapper would simply never fire.
func (n *Node) limitDevice(ctx context.Context, req PublishRequest, protocol string) (string, error) {
	return n.resolveBlockDevice(ctx, req.PublishContext, protocol)
}

// resolveBlockDevice resolves the host device backing a volume, by whatever
// identifier its protocol uses: an iSCSI LUN is found by its NAA, an NVMe
// namespace by its subsystem serial.
//
// Every caller that needs the device goes through here. publishBlock used to
// carry its own copy of the iSCSI half and demand a NAA unconditionally, so a
// raw-block NVMe volume failed every NodePublishVolume with `block volume needs
// "naa" in the publish context` -- filesystem NVMe worked, and volumeMode:
// Block did not.
func (n *Node) resolveBlockDevice(ctx context.Context, pc map[string]string, protocol string) (string, error) {
	switch protocol {
	case ProtocolISCSI:
		naa := pc[KeyNAA]
		if naa == "" {
			return "", fmt.Errorf("%w: no %s in the volume context", ErrInvalidRequest, KeyNAA)
		}
		return n.deviceFor(ctx, naa)
	case ProtocolNVMe:
		serial := pc[KeySerial]
		if serial == "" {
			return "", fmt.Errorf("%w: no %s in the volume context", ErrInvalidRequest, KeySerial)
		}
		return resolveNVMeDevice(ctx, n.exec, n.hostRoot(), serial)
	}
	return "", fmt.Errorf("%w: protocol %q has no block device", ErrInvalidRequest, protocol)
}

// writeCgroupFile writes one line to a cgroup control file.
//
// O_CREATE is deliberately absent. A cgroup control file always already exists;
// if io.max does not, the io controller is not enabled for that cgroup and the
// honest outcome is "could not write", not a stray regular file named io.max
// sitting in a cgroup directory pretending a limit is in force.
func writeCgroupFile(path, line string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// resolvePodCgroup finds the cgroup v2 directory of one pod beneath root.
//
// This is the part most likely to silently stop working, because the layout is
// not a kernel interface — it is the product of the kubelet's cgroup DRIVER,
// the pod's QoS CLASS, and whatever the distribution decided to nest kubepods
// under. Three shapes are in the wild for the same pod:
//
//	systemd, burstable:   kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<uid>.slice
//	systemd, guaranteed:  kubepods.slice/kubepods-pod<uid>.slice
//	cgroupfs, besteffort: kubepods/besteffort/pod<uid>
//
// and with the systemd driver the UID's dashes become underscores, because
// systemd unit names cannot contain a dash in that position without meaning
// something else. Guaranteed pods have NO QoS segment at all; a resolver that
// assumed one would work in testing (where everything is burstable) and fail
// exactly for the pods someone cared enough to give guaranteed QoS.
//
// So nothing is hardcoded to one shape. The known layouts are tried as direct
// paths first — that is one stat on a real node — and anything else is found by
// a bounded search for a directory whose name identifies the pod, which is what
// catches a distribution that nests the whole tree somewhere else (k3s under
// system.slice/k3s.service, for instance).
func resolvePodCgroup(root, podUID string) (string, error) {
	podUID = strings.TrimSpace(podUID)
	if podUID == "" {
		return "", fmt.Errorf("%w: empty pod UID", ErrInvalidRequest)
	}
	dashed := podUID
	under := strings.ReplaceAll(podUID, "-", "_")

	for _, rel := range []string{
		// systemd driver. Guaranteed pods sit directly under kubepods.slice.
		filepath.Join("kubepods.slice", "kubepods-pod"+under+".slice"),
		filepath.Join("kubepods.slice", "kubepods-burstable.slice", "kubepods-burstable-pod"+under+".slice"),
		filepath.Join("kubepods.slice", "kubepods-besteffort.slice", "kubepods-besteffort-pod"+under+".slice"),
		// cgroupfs driver, which keeps the UID verbatim.
		filepath.Join("kubepods", "pod"+dashed),
		filepath.Join("kubepods", "burstable", "pod"+dashed),
		filepath.Join("kubepods", "besteffort", "pod"+dashed),
	} {
		p := filepath.Join(root, rel)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			return p, nil
		}
	}

	if p, ok := searchPodCgroup(root, dashed, under, 0); ok {
		return p, nil
	}
	return "", fmt.Errorf("no cgroup directory for pod %s under %s", podUID, root)
}

// cgroupSearchDepth bounds the fallback walk. The deepest layout seen in the
// wild is five levels — /sys/fs/cgroup/system.slice/<kubelet unit>/kubepods.slice/
// kubepods-<qos>.slice/kubepods-<qos>-pod<uid>.slice — and a bound keeps the
// walk from descending into the per-container cgroups below every pod on a busy
// node, which is where the directory count actually lives.
const cgroupSearchDepth = 6

// searchPodCgroup walks for a directory naming the pod. Entries are visited in
// sorted order so that a tree with more than one match resolves the same way
// every time rather than depending on directory order.
func searchPodCgroup(dir, dashed, under string, depth int) (string, bool) {
	if depth > cgroupSearchDepth {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if isCgroupDir(e) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if matchesPodCgroup(name, dashed, under) {
			return filepath.Join(dir, name), true
		}
	}
	for _, name := range names {
		if p, ok := searchPodCgroup(filepath.Join(dir, name), dashed, under, depth+1); ok {
			return p, true
		}
	}
	return "", false
}

// isCgroupDir reports whether a directory entry is a directory to descend into.
// A symlink is not followed: the cgroup filesystem has none, so one that appears
// in a fixture — or in something bind-mounted over the tree — is not part of the
// hierarchy and following it risks an unbounded walk.
func isCgroupDir(e fs.DirEntry) bool {
	return e.IsDir() && e.Type()&fs.ModeSymlink == 0
}

// matchesPodCgroup reports whether a cgroup directory name is the pod-level
// cgroup for this UID, in any of the spellings the drivers produce:
//
//	pod<uid>                              cgroupfs
//	kubepods-pod<uid>.slice               systemd, guaranteed
//	kubepods-<qos>-pod<uid>.slice         systemd, burstable or besteffort
//
// Matching on the "pod<uid>" tail rather than on a full expected name is what
// makes an unfamiliar QoS or prefix segment harmless instead of fatal.
func matchesPodCgroup(name, dashed, under string) bool {
	base := strings.TrimSuffix(name, ".slice")
	for _, uid := range []string{dashed, under} {
		if base == "pod"+uid || strings.HasSuffix(base, "-pod"+uid) {
			return true
		}
	}
	return false
}
