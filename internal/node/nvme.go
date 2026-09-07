package node

// NVMe-oF attach.
//
// The sequence here is the one proven end to end against the appliance and
// worker-21 (nvme-cli 2.x, nvme_tcp loaded):
//
//	nvme discover -t tcp -a <ip> -s <port>
//	nvme connect  -t tcp -a <ip> -s <port> -n <subnqn>
//	resolve /dev/disk/by-id/nvme-<model>_<serial>  -> mkfs -> mount
//	nvme disconnect -n <subnqn>
//
// Two rules govern every line, and both come from what is actually on the nodes:
//
//  1. DEVICE RESOLUTION KEYS ON THE SUBSYSTEM SERIAL, NEVER ON AN INDEX.
//     worker-21 has its OWN NVMe SSD (a Lexar NM620) at /dev/nvme0n1; our volume
//     landed on /dev/nvme1n1. An index is not a device identity, and handing a
//     pod the node's own disk is unrecoverable. The serial comes from
//     nvmet.subsys.create and appears verbatim in the by-id link name.
//  2. TEARDOWN IS SCOPED TO OUR SUBSYSTEM. `nvme disconnect-all` would drop
//     every fabric connection on the node, including any other driver's, so it
//     is never issued — only `nvme disconnect -n <subnqn>`.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultNVMePort is the IANA-assigned NVMe-oF port, used when the publish
// context carries a bare address.
const defaultNVMePort = "4420"

// stageNVMe connects the subsystem, resolves the device deterministically, and —
// for a filesystem volume — formats it if and only if it is blank, then mounts
// it. A raw block volume stops after the device is resolved.
func (n *Node) stageNVMe(ctx context.Context, req StageRequest) error {
	if err := n.pre.Require(CapNVMe); err != nil {
		return err
	}
	fsType := fsTypeOf(req.VolumeCapability, req.PublishContext)
	if !req.VolumeCapability.Block {
		// Fail here, before anything touches the host, so the operator reads
		// "needs xfsprogs" rather than a mount(8) exit code.
		if err := n.requireFS(fsType); err != nil {
			return err
		}
	}

	portal := req.PublishContext[KeyPortal]
	nqn := req.PublishContext[KeyNQN]
	serial := req.PublishContext[KeySerial]
	if portal == "" || nqn == "" || serial == "" {
		return fmt.Errorf("%w: nvme volume needs %q, %q and %q in the publish context",
			ErrInvalidRequest, KeyPortal, KeyNQN, KeySerial)
	}
	transport := nvmeTransport(req.PublishContext)

	if err := nvmeConnect(ctx, n.exec, transport, portal, nqn); err != nil {
		return err
	}

	device, err := resolveNVMeDevice(ctx, n.exec, n.hostRoot(), serial)
	if err != nil {
		return err
	}

	if req.VolumeCapability.Block {
		// volumeMode: Block. The pod gets the device itself at publish time;
		// there is nothing to stage beyond the connection.
		return nil
	}
	if req.StagingPath == "" {
		return fmt.Errorf("%w: no staging path", ErrInvalidRequest)
	}

	mounted, err := n.isMounted(req.StagingPath)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := n.formatIfBlank(ctx, device, fsType); err != nil {
		return err
	}
	if err := os.MkdirAll(req.StagingPath, 0o750); err != nil {
		return fmt.Errorf("create staging path %s: %w", req.StagingPath, err)
	}
	opts := append([]string{}, req.VolumeCapability.MountFlags...)
	if req.VolumeCapability.Readonly {
		opts = append(opts, "ro")
	}
	return n.mount(ctx, fsType, device, req.StagingPath, opts)
}

// unstageNVMe disconnects our subsystem and nothing else. The publish context is
// not part of NodeUnstageVolume in the CSI spec, so when the CO does not supply
// it the connection is left alone rather than guessed at — guessing here means
// disconnect-all, which takes every other fabric volume on the node down with
// ours.
func (n *Node) unstageNVMe(ctx context.Context, req UnstageRequest) error {
	nqn := req.PublishContext[KeyNQN]
	if nqn == "" {
		return nil
	}
	return nvmeDisconnect(ctx, n.exec, nqn)
}

// nvmeTransport reads the transport out of the publish context.
//
// RDMA is IMPLEMENTED BUT UNVALIDATED: the appliance reports
// nvmet.global.rdma=false and the RK3588 nodes have no RDMA NICs, so this
// branch has never carried a byte. The controller refuses to provision an RDMA
// volume against an appliance that cannot serve one, which is what keeps an
// untested transport from reaching a node by accident.
func nvmeTransport(pc map[string]string) string {
	switch t := strings.ToLower(strings.TrimSpace(pc[KeyTransport])); t {
	case "rdma":
		return "rdma"
	default:
		return "tcp"
	}
}

// splitPortal splits "host:port" into its parts, defaulting the port. A bare
// IPv6 address without brackets is accepted as an address, not as host:port.
func splitPortal(portal string) (host, port string) {
	portal = strings.TrimSpace(portal)
	h, p, err := net.SplitHostPort(portal)
	if err != nil {
		return portal, defaultNVMePort
	}
	if p == "" {
		p = defaultNVMePort
	}
	return h, p
}

// nvmeConnect discovers the portal and connects to one subsystem.
//
// A connection that already exists is a success: the kubelet retries
// NodeStageVolume, and nvme-cli reports the existing controller rather than
// failing in a way that should fail the volume.
func nvmeConnect(ctx context.Context, e Executor, transport, portal, nqn string) error {
	if portal == "" || nqn == "" {
		return fmt.Errorf("%w: nvme connect needs both a portal and a subsystem", ErrInvalidRequest)
	}
	host, port := splitPortal(portal)

	// Discovery is the one call with no subsystem to name — the subsystem list
	// is what it returns — so it is scoped by portal alone and changes nothing.
	if _, err := e.Run(ctx, "nvme", "discover", "-t", transport, "-a", host, "-s", port); err != nil {
		return fmt.Errorf("nvme discovery on %s: %w", portal, err)
	}
	if _, err := e.Run(ctx, "nvme", "connect", "-t", transport, "-a", host, "-s", port, "-n", nqn); err != nil {
		if isAlreadyConnected(err) {
			return nil
		}
		return fmt.Errorf("nvme connect to %s at %s: %w", nqn, portal, err)
	}
	return nil
}

// nvmeDisconnect drops the controllers for one subsystem. A subsystem that is
// already gone is a success, so a retried teardown converges instead of failing.
func nvmeDisconnect(ctx context.Context, e Executor, nqn string) error {
	if nqn == "" {
		return fmt.Errorf("%w: nvme disconnect needs a subsystem", ErrInvalidRequest)
	}
	if _, err := e.Run(ctx, "nvme", "disconnect", "-n", nqn); err != nil {
		if isNoSuchSubsystem(err) {
			return nil
		}
		return fmt.Errorf("nvme disconnect from %s: %w", nqn, err)
	}
	return nil
}

// isAlreadyConnected recognises nvme-cli's report that the controller it was
// asked to create is already there.
func isAlreadyConnected(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already connected") ||
		strings.Contains(s, "operation already in progress") ||
		strings.Contains(s, "file exists")
}

// isNoSuchSubsystem recognises nvme-cli's report that what it was asked to
// remove is not there.
func isNoSuchSubsystem(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no controllers found") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "no such")
}

// nvmeDevice is one entry of `nvme list -o json`, in whichever of nvme-cli's
// schemas the host's version emits. Only the model and serial are read.
type nvmeDevice struct {
	Model  string
	Serial string
}

// resolveNVMeDevice turns the subsystem's serial into the stable device path the
// kernel publishes for it, and waits — up to deviceWaitTimeout — for the
// namespace and its udev link to appear after a connect.
//
// It builds exactly ONE path and stats it. It deliberately does not list
// /dev/disk/by-id, /dev or /sys: the node has its own NVMe disks and other
// drivers attach and detach devices on the same node, so an enumeration can
// observe a device mid-teardown or attribute another controller's namespace to
// this volume. The serial is unique and the mapping is deterministic.
//
// The by-id name is nvme-<model>_<serial>, and the model varies per appliance,
// so the model half is read back from `nvme list` — keyed on OUR serial, which
// is a lookup, not a scan of the device tree.
//
// root is the host filesystem root as this process sees it; the returned path is
// host-absolute, because every command the node runs is executed in the host's
// mount namespace.
func resolveNVMeDevice(ctx context.Context, e Executor, root, serial string) (string, error) {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return "", fmt.Errorf("%w: empty NVMe serial", ErrInvalidRequest)
	}

	deadline := time.Now().Add(deviceWaitTimeout)
	for {
		model, ok, err := nvmeModelForSerial(ctx, e, serial)
		if err == nil && ok {
			name := "nvme-" + udevEncode(model) + "_" + udevEncode(serial)
			hostPath := byIDDir + "/" + name
			localPath := filepath.Join(root, "dev", "disk", "by-id", name)
			if _, err := os.Lstat(localPath); err == nil {
				return hostPath, nil
			}
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("%w: no NVMe namespace with serial %s after %s",
				ErrDeviceNotFound, serial, deviceWaitTimeout)
		}
		time.Sleep(devicePollInterval)
	}
}

// nvmeModelForSerial asks nvme-cli which controller carries our serial and
// returns its model number.
func nvmeModelForSerial(ctx context.Context, e Executor, serial string) (string, bool, error) {
	out, err := e.Run(ctx, "nvme", "list", "-o", "json")
	if err != nil {
		return "", false, fmt.Errorf("nvme list: %w", err)
	}
	for _, d := range parseNVMeList(out) {
		if strings.EqualFold(strings.TrimSpace(d.Serial), serial) {
			return d.Model, true, nil
		}
	}
	return "", false, nil
}

// parseNVMeList extracts every (model, serial) pair from `nvme list -o json`.
//
// nvme-cli has changed this document's shape between major versions — 1.x puts
// ModelNumber and SerialNumber on a flat Devices array, 2.x nests them under
// Subsystems/Controllers — so the walk is generic over the JSON rather than
// bound to one schema. A parse that only understood one version would silently
// find no device and time out on the other.
func parseNVMeList(out []byte) []nvmeDevice {
	var doc any
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil
	}
	var devices []nvmeDevice
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			var d nvmeDevice
			for k, raw := range t {
				s, _ := raw.(string)
				switch strings.ToLower(k) {
				case "serialnumber", "serial":
					d.Serial = s
				case "modelnumber", "model":
					d.Model = s
				}
			}
			if d.Serial != "" {
				devices = append(devices, d)
			}
			for _, raw := range t {
				walk(raw)
			}
		case []any:
			for _, raw := range t {
				walk(raw)
			}
		}
	}
	walk(doc)
	return devices
}

// udevEncode renders a model or serial the way udev spells it in a by-id link:
// surrounding whitespace trimmed, and every character outside the safe set —
// spaces included — replaced by an underscore.
func udevEncode(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '+':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// nvmeRescan asks the kernel to re-read the namespaces of one controller after
// the controller side grew the zvol. It is scoped to the device we resolved, so
// no other controller on the node is touched.
func nvmeRescan(ctx context.Context, e Executor, device string) error {
	if device == "" {
		return fmt.Errorf("%w: nvme rescan needs a device", ErrInvalidRequest)
	}
	if _, err := e.Run(ctx, "nvme", "ns-rescan", device); err != nil {
		return fmt.Errorf("nvme ns-rescan %s: %w", device, err)
	}
	return nil
}
