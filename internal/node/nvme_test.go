package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// testSerial is the serial nvmet.subsys.create returns; it is the ONLY
	// stable key the node has for our namespace.
	testSerial = "adfa04662ffcf780febc"
	testModel  = "TrueNAS_TVS-1688"
	testSubNQN = "nqn.2011-06.com.truenas:uuid:6ab80cc6-1111-2222-3333-444444444444:csi-pvc-abc"
	nvmePortal = "192.168.10.253:4420"
	nvmeByID   = "/dev/disk/by-id/nvme-TrueNAS_TVS-1688_adfa04662ffcf780febc"

	// localNVMe is the node's OWN disk. worker-21 carries a Lexar NM620 at
	// /dev/nvme0n1, so an implementation that assumed an index — or that
	// matched loosely — would hand a pod the node's root device.
	localSerial = "PL0000000000000000"
	localModel  = "Lexar_SSD_NM620_2TB"
	localByID   = "/dev/disk/by-id/nvme-Lexar_SSD_NM620_2TB_PL0000000000000000"
)

// nvmeRoot builds a fake host root whose by-id directory holds both our volume's
// link and the node's own NVMe disk. The directory is made execute-only: an exact
// path lookup through it still works, but LISTING it does not.
//
// That is the entire point of this fixture. The node must construct one exact
// by-id path from the subsystem serial and stat it; anything that enumerates the
// directory (a glob, a ReadDir, a walk of /dev) fails here, and would on a real
// node race with the local disk and with other drivers' devices.
func nvmeRoot(t *testing.T, withDevice bool) string {
	t.Helper()
	root := hostRoot(t)
	byID := filepath.Join(root, "dev", "disk", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatalf("mkdir by-id: %v", err)
	}
	// The node's own disk is always there, device or not.
	if err := os.Symlink("../../nvme0n1", filepath.Join(byID, filepath.Base(localByID))); err != nil {
		t.Fatalf("symlink local disk: %v", err)
	}
	if withDevice {
		if err := os.Symlink("../../nvme1n1", filepath.Join(byID, filepath.Base(nvmeByID))); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	if os.Geteuid() == 0 {
		t.Log("running as root: the unreadable-directory guard against /dev enumeration cannot be enforced")
		return root
	}
	if err := os.Chmod(byID, 0o111); err != nil {
		t.Fatalf("chmod by-id: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(byID, 0o755) })
	return root
}

// nvmeHandler answers the nvme-cli commands a stage issues. `nvme list` reports
// both the node's own disk and ours, keyed by serial — which is how the node
// learns the model half of the by-id name without listing the directory.
type nvmeHandler struct {
	connected map[string]bool
	attached  bool
	hasFS     bool
}

func newNVMeHandler(attached bool) *nvmeHandler {
	return &nvmeHandler{connected: map[string]bool{}, attached: attached}
}

func (h *nvmeHandler) run(name string, args []string) ([]byte, error) {
	switch name {
	case "blkid":
		if h.hasFS {
			return []byte("ext4\n"), nil
		}
		// The EXIT STATUS is what says "no filesystem here"; a bare error would
		// mean blkid could not answer, which must never license mkfs.
		return nil, exitErr{blkidNothingFound}
	case "nvme":
		if len(args) == 0 {
			return nil, nil
		}
		switch args[0] {
		case "discover":
			return []byte("discovery: " + testSubNQN + "\n"), nil
		case "connect":
			h.connected[argValue(args, "-n")] = true
			h.attached = true
			return nil, nil
		case "disconnect":
			delete(h.connected, argValue(args, "-n"))
			h.attached = false
			return []byte("NQN:" + argValue(args, "-n") + " disconnected 1 controller(s)\n"), nil
		case "disconnect-all":
			return nil, errors.New("disconnect-all must never be called")
		case "list":
			devices := `{"NameSpace":1,"DevicePath":"/dev/nvme0n1","ModelNumber":"Lexar SSD NM620 2TB","SerialNumber":"` + localSerial + `"}`
			if h.attached {
				devices += `,{"NameSpace":1,"DevicePath":"/dev/nvme1n1","ModelNumber":"TrueNAS TVS-1688","SerialNumber":"` + testSerial + `"}`
			}
			return []byte(`{"Devices":[` + devices + `]}`), nil
		}
	}
	return nil, nil
}

func nvmeContext() map[string]string {
	return map[string]string{
		KeyProtocol:  ProtocolNVMe,
		KeyPortal:    nvmePortal,
		KeyNQN:       testSubNQN,
		KeySerial:    testSerial,
		KeyTransport: "tcp",
	}
}

// TestNVMeDeviceResolutionUsesSerial pins the finding that cost the most to
// learn: the node has its own NVMe disk, so an index is never a device
// identity. Resolution keys on the subsystem serial from the API, builds one
// exact /dev/disk/by-id path and stats it — the by-id directory here is
// execute-only, so any enumeration fails outright.
func TestNVMeDeviceResolutionUsesSerial(t *testing.T) {
	root := nvmeRoot(t, true)
	h := newNVMeHandler(true)
	e := &fakeExec{handler: h.run}
	ctx := context.Background()

	got, err := resolveNVMeDevice(ctx, e, root, testSerial)
	if err != nil {
		t.Fatalf("resolveNVMeDevice: %v", err)
	}
	if got != nvmeByID {
		t.Fatalf("resolveNVMeDevice = %q, want %q", got, nvmeByID)
	}
	if got == localByID || strings.Contains(got, "nvme0n1") {
		t.Fatalf("resolution landed on the node's own disk: %q", got)
	}

	// No command may enumerate a directory or /dev either.
	for _, c := range e.cmds() {
		for _, banned := range []string{"ls ", "find ", "udevadm trigger", "disconnect-all"} {
			if strings.Contains(c, banned) {
				t.Fatalf("device resolution enumerated the host: %q", c)
			}
		}
	}

	// A serial that is not attached times out with ErrDeviceNotFound rather
	// than hanging or returning some other node's device.
	restore := shortDeviceWait(t)
	defer restore()
	if _, err := resolveNVMeDevice(ctx, e, root, "0000000000000000dead"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("absent device error = %v, want ErrDeviceNotFound", err)
	}
	if _, err := resolveNVMeDevice(ctx, e, root, ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty serial error = %v, want ErrInvalidRequest", err)
	}
}

// TestNVMeDisconnectIsScoped: `nvme disconnect-all` would tear down every fabric
// connection on the node, including any other driver's. Teardown names our
// subsystem NQN and nothing else.
func TestNVMeDisconnectIsScoped(t *testing.T) {
	root := nvmeRoot(t, true)
	h := newNVMeHandler(false)
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapNVMe, CapExt4)
	staging := filepath.Join(t.TempDir(), "globalmount")
	ctx := context.Background()

	if err := n.Stage(ctx, StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: nvmeContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !h.connected[testSubNQN] {
		t.Fatal("our subsystem was never connected")
	}

	writeMounts(t, root, staging)
	if err := n.Unstage(ctx, UnstageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: nvmeContext(),
	}); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if h.connected[testSubNQN] {
		t.Fatal("our own connection survived unstage")
	}

	sawDisconnect := false
	for _, c := range e.only("nvme") {
		args := strings.Fields(c)[1:]
		if strings.Contains(c, "disconnect-all") {
			t.Fatalf("blanket disconnect would kill every fabric connection on the node: %q", c)
		}
		if args[0] == "disconnect" {
			sawDisconnect = true
			if argValue(args, "-n") != testSubNQN {
				t.Fatalf("disconnect not scoped to our subsystem: %q", c)
			}
			if contains(args, "-d") || contains(args, "--device") {
				t.Fatalf("disconnect must name the subsystem, not a device: %q", c)
			}
		}
		if args[0] == "connect" && argValue(args, "-n") != testSubNQN {
			t.Fatalf("connect not scoped to our subsystem: %q", c)
		}
	}
	if !sawDisconnect {
		t.Fatal("no nvme disconnect was issued at all")
	}
}

// TestNVMeStageMountsTheResolvedDevice: the verified sequence is discover,
// connect, resolve, format-if-blank, mount — and the mount must use the by-id
// path resolved from the serial, never a /dev/nvmeXnY guess.
func TestNVMeStageMountsTheResolvedDevice(t *testing.T) {
	root := nvmeRoot(t, true)
	h := newNVMeHandler(false)
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapNVMe, CapXFS)
	staging := filepath.Join(t.TempDir(), "globalmount")

	if err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: nvmeContext(),
		VolumeCapability: VolumeCapability{FsType: "xfs"},
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	cmds := e.cmds()
	joined := strings.Join(cmds, "\n")
	if !strings.Contains(joined, "nvme discover -t tcp -a 192.168.10.253 -s 4420") {
		t.Fatalf("discovery must precede connect and carry the portal:\n%s", joined)
	}
	if !strings.Contains(joined, "nvme connect -t tcp -a 192.168.10.253 -s 4420 -n "+testSubNQN) {
		t.Fatalf("connect must name transport, portal and subsystem:\n%s", joined)
	}
	if got := e.only("mkfs.xfs"); len(got) != 1 || got[0] != "mkfs.xfs "+nvmeByID {
		t.Fatalf("mkfs: got %q, want mkfs.xfs %s", got, nvmeByID)
	}
	for _, c := range e.only("mount") {
		if !strings.Contains(c, nvmeByID) {
			t.Fatalf("mount used a device other than the resolved by-id path: %q", c)
		}
	}

	// A staged path that is already mounted is not formatted again: a second
	// mkfs on a retried NodeStageVolume destroys the volume.
	writeMounts(t, root, staging)
	h.hasFS = true
	if err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: nvmeContext(),
		VolumeCapability: VolumeCapability{FsType: "xfs"},
	}); err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if got := e.only("mkfs.xfs"); len(got) != 1 {
		t.Fatalf("an already-mounted staging path was formatted again: %q", got)
	}
}

// TestNVMeRequiresNVMeCLI: a node without nvme-cli must fail the stage with a
// message naming the package, not with a cryptic exec error.
func TestNVMeRequiresNVMeCLI(t *testing.T) {
	root := nvmeRoot(t, true)
	n := newTestNode(t, root, &fakeExec{}, CapExt4) // no CapNVMe
	err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: filepath.Join(t.TempDir(), "m"),
		PublishContext:   nvmeContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
	})
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("want ErrCapabilityUnavailable, got %v", err)
	}
	if !strings.Contains(err.Error(), "nvme-cli") {
		t.Fatalf("the error must name the package to install, got %v", err)
	}

	// And the preflight itself must know what NVMe-oF costs on a host.
	bare := fakeRoot(t, nil, nil, []string{"kernel/drivers/nvme/host/nvme-tcp.ko"})
	var rec recorder
	p, err := Detect(context.Background(), bare, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if p.Found[CapNVMe] {
		t.Fatal("CapNVMe reported available on a root without the nvme binary")
	}
	if !strings.Contains(strings.Join(p.Missing[CapNVMe], ","), "nvme-cli") {
		t.Fatalf("missing must name nvme-cli, got %v", p.Missing[CapNVMe])
	}

	ready := fakeRoot(t, []string{"sbin/nvme"}, []string{"nvme_tcp"}, nil)
	p2, err := Detect(context.Background(), ready, rec.modprobe)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !p2.Found[CapNVMe] {
		t.Fatalf("CapNVMe must be available with nvme and nvme_tcp present, missing %v", p2.Missing[CapNVMe])
	}
	if _, ok := p2.TopologyLabels()[TopologyKey(CapNVMe)]; !ok {
		t.Fatal("CapNVMe must be advertised as a topology segment")
	}
}

// TestNVMeControllerFor pins the mapping that NVMe expansion depends on.
//
// `nvme ns-rescan` takes the controller. Given a namespace it prints its usage
// line and exits 1 — measured on a node, where
// `nvme ns-rescan /dev/disk/by-id/nvme-TrueNAS_...` exited 1 while
// `nvme ns-rescan /dev/nvme1` exited 0 and the namespace reported its new size
// at once. The caller only ever has the by-id link, which points at the
// namespace, so every NVMe expansion failed: the controller grew the zvol, the
// node was asked to finish, and the claim stayed at its old size for ever.
func TestNVMeControllerFor(t *testing.T) {
	root := t.TempDir()
	byID := filepath.Join(root, "dev", "disk", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev", "nvme1n1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(byID, "nvme-TrueNAS_TVS-1688_2c63f0d7bbe7362fb311")
	if err := os.Symlink("../../nvme1n1", link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		device string
		want   string
	}{
		{"a by-id link, which is all the caller has", "/dev/disk/by-id/nvme-TrueNAS_TVS-1688_2c63f0d7bbe7362fb311", "/dev/nvme1"},
		{"a namespace path passed directly", "/dev/nvme1n1", "/dev/nvme1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := nvmeControllerFor(root, tc.device)
			if err != nil {
				t.Fatalf("nvmeControllerFor: %v", err)
			}
			if got != tc.want {
				t.Errorf("nvmeControllerFor(%q) = %q, want %q", tc.device, got, tc.want)
			}
		})
	}

	if _, err := nvmeControllerFor(root, "/dev/sda"); err == nil {
		t.Error("a device that is not an NVMe namespace was accepted, so the rescan " +
			"would run against something else entirely")
	}
}

// TestNVMeRescanTargetsTheController checks the command actually issued.
func TestNVMeRescanTargetsTheController(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dev", "nvme1n1"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recordingExec{}
	if err := nvmeRescan(context.Background(), rec, root, "/dev/nvme1n1"); err != nil {
		t.Fatalf("nvmeRescan: %v", err)
	}
	want := "nvme ns-rescan /dev/nvme1"
	if got := strings.Join(rec.calls, " | "); !strings.Contains(got, want) {
		t.Errorf("ran %q, want it to contain %q", got, want)
	}
}

// recordingExec captures the commands a node action runs.
type recordingExec struct{ calls []string }

func (r *recordingExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, strings.Join(append([]string{name}, args...), " "))
	return nil, nil
}
