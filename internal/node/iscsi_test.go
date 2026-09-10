package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testNAA    = "0x6589cfc000000a960e31390c2657efa7"
	testByID   = "/dev/disk/by-id/scsi-36589cfc000000a960e31390c2657efa7"
	testIQN    = "iqn.2005-10.org.freenas.ctl:csi-pvc-abc"
	testPortal = "192.168.10.253:3260"
)

// longhornIQNs are the sessions that already exist on the node. Nothing this driver
// does may disturb them.
var longhornIQNs = []string{
	"iqn.2019-10.io.longhorn:pvc-0d0f1e5a-1111-4a1a-9f0e-aaaaaaaaaaaa",
	"iqn.2019-10.io.longhorn:pvc-9b2c3d4e-2222-4b2b-8e1f-bbbbbbbbbbbb",
}

// iscsiRoot builds a fake host root whose by-id directory holds the symlink the
// appliance's NAA maps to. The by-id directory is made execute-only: a path lookup
// through it still works, but listing it does not. That is the whole point — the
// node must construct the by-id path from the NAA and stat it, never enumerate the
// directory, because Longhorn is attaching and detaching devices on the same node
// and a scan races with it.
func iscsiRoot(t *testing.T, withDevice bool) string {
	t.Helper()
	root := hostRoot(t)
	byID := filepath.Join(root, "dev", "disk", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatalf("mkdir by-id: %v", err)
	}
	// by-path exists on any host with iSCSI devices, and unstage reads it to
	// decide whether a sibling volume still holds the SHARED session. Leaving
	// it out made the fake describe a host that cannot exist, on exactly the
	// question these tests are about.
	if err := os.MkdirAll(filepath.Join(root, "dev", "disk", "by-path"), 0o755); err != nil {
		t.Fatalf("mkdir by-path: %v", err)
	}
	if withDevice {
		if err := os.Symlink("../../sdc", filepath.Join(byID, "scsi-36589cfc000000a960e31390c2657efa7")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		// A Longhorn device in the same directory: a scan that matched loosely
		// would find the wrong one.
		if err := os.Symlink("../../sda", filepath.Join(byID, "scsi-3600140512345678900000000000000ff")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	if os.Geteuid() == 0 {
		t.Log("running as root: the unreadable-directory guard against /dev scanning cannot be enforced")
		return root
	}
	if err := os.Chmod(byID, 0o111); err != nil {
		t.Fatalf("chmod by-id: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(byID, 0o755) })
	return root
}

// iscsiHandler answers the commands an iSCSI stage issues against a fake host that
// starts out carrying the Longhorn sessions. hasFS controls what blkid reports.
type iscsiHandler struct {
	sessions map[string]bool
	hasFS    bool
	// deleted records node records removed with -o delete.
	deleted map[string]bool
}

func newISCSIHandler() *iscsiHandler {
	h := &iscsiHandler{sessions: map[string]bool{}, deleted: map[string]bool{}}
	for _, i := range longhornIQNs {
		h.sessions[i] = true
	}
	return h
}

func (h *iscsiHandler) run(name string, args []string) ([]byte, error) {
	line := name + " " + strings.Join(args, " ")
	switch name {
	case "blkid":
		if h.hasFS {
			return []byte(`/dev/sdc: UUID="a1" TYPE="ext4"` + "\n"), nil
		}
		// blkid exits 2 with no output when the device holds no filesystem.
		// The EXIT STATUS is what says so: every other blkid failure is also
		// silent, and only this one licenses mkfs.
		return nil, exitErr{blkidNothingFound}
	case "iscsiadm":
		target := argValue(args, "-T")
		switch {
		case contains(args, "--login"):
			h.sessions[target] = true
		case contains(args, "--logout"):
			delete(h.sessions, target)
		case argValue(args, "-o") == "delete":
			h.deleted[target] = true
		}
		if contains(args, "-m") && argValue(args, "-m") == "discovery" {
			return []byte(testPortal + "," + "1 " + testIQN + "\n"), nil
		}
		return []byte("ok " + line + "\n"), nil
	}
	return nil, nil
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// argValue returns the value following flag in args, or "".
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func iscsiContext() map[string]string {
	return map[string]string{
		KeyProtocol: ProtocolISCSI,
		KeyPortal:   testPortal,
		KeyIQN:      testIQN,
		KeyNAA:      testNAA,
	}
}

func TestISCSIDeviceResolution(t *testing.T) {
	root := iscsiRoot(t, true)

	got, err := resolveDevice(root, testNAA)
	if err != nil {
		t.Fatalf("resolveDevice: %v", err)
	}
	if got != testByID {
		t.Fatalf("resolveDevice = %q, want %q", got, testByID)
	}

	// The NAA is also accepted without its 0x prefix and in upper case, as the
	// middleware has been seen to render it both ways.
	if got, err := resolveDevice(root, "6589CFC000000A960E31390C2657EFA7"); err != nil || got != testByID {
		t.Fatalf("resolveDevice(bare upper) = %q, %v", got, err)
	}

	// An absent device times out with ErrDeviceNotFound rather than hanging or
	// returning a path that is not there.
	restore := shortDeviceWait(t)
	defer restore()
	if _, err := resolveDevice(root, "0xdeadbeef00000000deadbeef00000000"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("absent device error = %v, want ErrDeviceNotFound", err)
	}
}

// shortDeviceWait shrinks the device poll bound so a timeout test does not take
// thirty seconds.
func shortDeviceWait(t *testing.T) func() {
	t.Helper()
	oldT, oldI := deviceWaitTimeout, devicePollInterval
	deviceWaitTimeout, devicePollInterval = 150*time.Millisecond, 10*time.Millisecond
	return func() { deviceWaitTimeout, devicePollInterval = oldT, oldI }
}

func TestISCSICommandsAreScoped(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapISCSI, CapExt4)
	staging := filepath.Join(t.TempDir(), "globalmount")

	req := StageRequest{
		VolumeID:         "pvc-abc",
		StagingPath:      staging,
		PublishContext:   iscsiContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
		Secrets:          map[string]string{KeyCHAPUser: "csi", KeyCHAPSecret: "s3cr3t-passphrase"},
	}
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	writeMounts(t, root, staging)
	if err := n.Unstage(context.Background(), UnstageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(),
	}); err != nil {
		t.Fatalf("Unstage: %v", err)
	}

	saw := 0
	for _, c := range e.only("iscsiadm") {
		args := strings.Fields(c)[1:]
		saw++
		if contains(args, "--logoutall") || strings.Contains(c, "--logoutall=") {
			t.Fatalf("blanket logout would kill Longhorn's sessions: %q", c)
		}
		if argValue(args, "-m") == "session" && (contains(args, "-R") || contains(args, "--rescan")) {
			t.Fatalf("global session rescan touches every target on the node: %q", c)
		}
		if !contains(args, "-p") || argValue(args, "-p") != testPortal {
			t.Fatalf("command not scoped to our portal: %q", c)
		}
		// Discovery is the one mode with no target to name; every other mode
		// must carry ours, or it acts on whatever iscsid has on file.
		if argValue(args, "-m") == "discovery" {
			continue
		}
		if argValue(args, "-T") != testIQN {
			t.Fatalf("command not scoped to our target: %q", c)
		}
	}
	if saw == 0 {
		t.Fatal("no iscsiadm command was issued at all")
	}
}

func TestE2EUnstageLeavesLonghornIntact(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapISCSI, CapExt4)
	staging := filepath.Join(t.TempDir(), "globalmount")

	req := StageRequest{
		VolumeID:         "pvc-abc",
		StagingPath:      staging,
		PublishContext:   iscsiContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
	}
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if !h.sessions[testIQN] {
		t.Fatal("our session was not established")
	}
	writeMounts(t, root, staging)
	if err := n.Unstage(context.Background(), UnstageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(),
	}); err != nil {
		t.Fatalf("Unstage: %v", err)
	}

	for _, i := range longhornIQNs {
		if !h.sessions[i] {
			t.Fatalf("Longhorn session %s was torn down by our unstage", i)
		}
		if h.deleted[i] {
			t.Fatalf("Longhorn node record %s was deleted by our unstage", i)
		}
	}
	if h.sessions[testIQN] {
		t.Fatal("our own session survived unstage")
	}
	if !h.deleted[testIQN] {
		t.Fatal("our own node record was not deleted")
	}
	for _, c := range e.only("iscsiadm") {
		for _, i := range longhornIQNs {
			if strings.Contains(c, i) {
				t.Fatalf("command named a Longhorn target: %q", c)
			}
		}
	}
}

func TestBlockVolumeSkipsMkfs(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapISCSI)
	staging := filepath.Join(t.TempDir(), "globalmount")
	target := filepath.Join(t.TempDir(), "block", "pvc-abc")

	blockCap := VolumeCapability{Block: true}
	if err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(), VolumeCapability: blockCap,
	}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := n.Publish(context.Background(), PublishRequest{
		VolumeID: "pvc-abc", StagingPath: staging, TargetPath: target,
		PublishContext: iscsiContext(), VolumeCapability: blockCap,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	for _, c := range e.cmds() {
		if strings.HasPrefix(c, "mkfs") {
			t.Fatalf("raw block volume was formatted: %q", c)
		}
		if strings.HasPrefix(c, "mount -t ") {
			t.Fatalf("raw block volume was mounted as a filesystem: %q", c)
		}
	}
	want := "mount -o bind " + testByID + " " + target
	if got := e.only("mount"); len(got) != 1 || got[0] != want {
		t.Fatalf("block publish:\n got %q\nwant %q", got, want)
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatalf("block target not created: %v", err)
	}
	if st.IsDir() {
		t.Fatal("block target is a directory; a device bind mount needs a file target")
	}
}

func TestFilesystemVolumeFormatsOnce(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	n := newTestNode(t, root, e, CapISCSI, CapExt4)
	staging := filepath.Join(t.TempDir(), "globalmount")

	req := StageRequest{
		VolumeID: "pvc-abc", StagingPath: staging, PublishContext: iscsiContext(),
		VolumeCapability: VolumeCapability{FsType: "ext4"},
	}
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	mkfs := e.only("mkfs.ext4")
	if len(mkfs) != 1 || !strings.HasSuffix(mkfs[0], testByID) {
		t.Fatalf("first stage mkfs: %q", mkfs)
	}
	if got := e.only("mount"); len(got) != 1 || !strings.Contains(got[0], "-t ext4") {
		t.Fatalf("first stage mount: %q", got)
	}

	// Second stage: the device now carries a filesystem and the staging path is
	// already mounted. Reformatting here destroys the volume.
	h.hasFS = true
	writeMounts(t, root, staging)
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if got := e.only("mkfs.ext4"); len(got) != 1 {
		t.Fatalf("volume was formatted twice: %q", got)
	}
	if got := e.only("mount"); len(got) != 1 {
		t.Fatalf("volume was mounted twice: %q", got)
	}
}

func TestStageFailsWhenFsTypeUnavailable(t *testing.T) {
	root := iscsiRoot(t, true)
	h := newISCSIHandler()
	e := &fakeExec{handler: h.run}
	// The node has iSCSI and ext4 but no xfsprogs, which is worker-21's real state.
	n := newTestNode(t, root, e, CapISCSI, CapExt4)

	err := n.Stage(context.Background(), StageRequest{
		VolumeID: "pvc-abc", StagingPath: filepath.Join(t.TempDir(), "globalmount"),
		PublishContext: iscsiContext(), VolumeCapability: VolumeCapability{FsType: "xfs"},
	})
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("error = %v, want ErrCapabilityUnavailable", err)
	}
	if !strings.Contains(err.Error(), "xfsprogs") {
		t.Fatalf("error %q does not name the missing package", err)
	}
	if got := e.cmds(); len(got) != 0 {
		t.Fatalf("node acted on the host before failing the preflight: %q", got)
	}
}

// exitErr is an error carrying a real process exit status, which is the only
// thing that distinguishes "blkid ran and found no filesystem" from "blkid
// could not answer".
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitErr) ExitCode() int { return e.code }

// blkidExec answers blkid with a fixed result and refuses every other command,
// so a test that formats shows up as a call to mkfs.
type blkidExec struct {
	out []byte
	err error
	ran []string
}

func (b *blkidExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	b.ran = append(b.ran, name+" "+strings.Join(args, " "))
	if name == "blkid" {
		return b.out, b.err
	}
	return nil, nil
}

func (b *blkidExec) formatted() bool {
	for _, c := range b.ran {
		if strings.HasPrefix(c, "mkfs.") {
			return true
		}
	}
	return false
}

// TestNeverFormatsWhenBlkidCannotAnswer is the most destructive bug this driver
// could have.
//
// blkid exits 2 with no output for a genuinely blank device, and that is the
// ONLY answer that licenses mkfs. Every other failure — blkid missing from the
// host, a permission error, a device busy — also produces no output, and
// treating those as "blank" formats a device that may hold somebody's data.
// blkid is not in the preflight requirements either, so a node without it
// advertises ext4 and xfs and then formats every volume it stages.
func TestNeverFormatsWhenBlkidCannotAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    []byte
		err    error
		format bool
	}{
		{name: "blank device: blkid exits 2 with no output", err: exitErr{2}, format: true},
		{name: "device carries a filesystem", out: []byte("ext4\n")},
		{name: "blkid is not installed on the host",
			err: errors.New("blkid: executable file not found in $PATH")},
		{name: "blkid failed for some other reason", err: exitErr{4}},
		{name: "blkid was killed", err: exitErr{-1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex := &blkidExec{out: tc.out, err: tc.err}
			n := &Node{exec: ex}
			err := n.formatIfBlank(context.Background(), "/dev/sdc", "ext4")
			switch {
			case tc.format && err != nil:
				t.Fatalf("a blank device must be formatted, got %v", err)
			case tc.format && !ex.formatted():
				t.Fatal("a blank device must be formatted")
			case !tc.format && ex.formatted():
				t.Fatal("FORMATTED a device blkid did not report as blank — this destroys data")
			}
		})
	}
}
