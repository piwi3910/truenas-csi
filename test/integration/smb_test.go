package integration

// SMB, end to end, THROUGH THE DRIVER.
//
// The previous version of this file mounted the share with a `mount -t cifs`
// command the test itself wrote. That proved the appliance side and nothing
// whatsoever about the driver: internal/node contained no cifs path at all, so
// NodeStageVolume rejected every SMB volume as an unsupported protocol, and this
// test still passed. Everything below therefore drives the real node plugin —
// Stage, Publish, Unpublish, Unstage — and only uses the node runner to observe
// what the driver did.
//
// How the node plugin is driven against a REMOTE machine: in production the
// DaemonSet mounts the host's / at /host and runs the host's mount binaries
// chrooted into it, so every path the plugin hands to mount(8) is host-absolute.
// That is exactly the shape the runner already provides (`chroot /host sh -c`),
// so the plugin's Executor is pointed at the runner and its host root at a local
// mirror of the node's /proc/mounts, refreshed after every command. The commands
// that reach the node are the driver's own, verbatim.
//
// One honest limitation of the harness: the runner's only channel to the node is
// the script in a Pod manifest, so the credentials file the driver produced has
// to be replayed onto the node through it, and the password is visible to anyone
// who can read that Pod. That is a property of THIS TEST HARNESS, not of the
// driver — the assertion at the end of the lifecycle test is what proves the
// driver itself never puts the password on a command line.

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"

	"github.com/piwi3910/truenas-csi/internal/node"
)

// smbUser creates a temporary SMB-capable account on the appliance and removes
// it afterwards. SMB, unlike NFS, authenticates, so an end-to-end check needs a
// real user rather than a host allowance.
func (e *env) smbUser(t *testing.T) (string, string) {
	t.Helper()
	name := "csi-e2e-" + uniqueName("u")[len("u")+1:]
	if len(name) > 16 {
		name = name[:16]
	}
	password := "Csi-E2E-" + uniqueName("p")
	var created any
	if err := e.client.CallJSON(context.Background(), &created, "user.create", map[string]any{
		"username": name, "full_name": "truenas-csi e2e", "password": password,
		"group_create": true, "smb": true,
	}); err != nil {
		t.Skipf("cannot create an SMB user on the appliance (needs ACCOUNT_WRITE): %v", err)
	}
	t.Cleanup(func() {
		var users []struct {
			ID int `json:"id"`
		}
		if err := e.client.CallJSON(context.Background(), &users, "user.query",
			[]any{[]any{"username", "=", name}}, map[string]any{}); err == nil && len(users) > 0 {
			_ = e.client.CallJSON(context.Background(), nil, "user.delete", users[0].ID, map[string]any{})
		}
	})
	return name, password
}

// nodeExec is the node plugin's Executor, wired to a real cluster node.
//
// It also keeps the plugin's view of the host honest in two ways the plugin
// cannot do for itself from another machine: it refreshes the mirrored mount
// table after every command, and it replays the credentials file the plugin
// wrote locally onto the node for the duration of the mount. The second is a
// harness concession, not a driver behaviour — see the file comment.
type nodeExec struct {
	t      *testing.T
	runner *NodeRunner
	// root is the local mirror of the node's / that the plugin reads.
	root string

	mu       sync.Mutex
	commands []string
	output   []string
}

// quote renders one argument for the shell that runs on the node.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Run executes one of the plugin's commands on the node.
func (x *nodeExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := append([]string{name}, args...)
	quoted := make([]string, 0, len(cmd))
	for _, a := range cmd {
		quoted = append(quoted, quote(a))
	}
	line := strings.Join(quoted, " ")

	x.mu.Lock()
	x.commands = append(x.commands, strings.Join(cmd, " "))
	x.mu.Unlock()

	script := "set -e\n"
	if cred := credentialsPathIn(args); cred != "" {
		body, err := os.ReadFile(filepath.Join(x.root, cred))
		if err != nil {
			return nil, fmt.Errorf("the driver named a credentials file it did not write (%s): %w", cred, err)
		}
		script += fmt.Sprintf("mkdir -p %s\n", quote(filepath.Dir(cred)))
		script += fmt.Sprintf("printf %%s %s | base64 -d > %s\nchmod 600 %s\n",
			quote(base64.StdEncoding.EncodeToString(body)), quote(cred), quote(cred))
		// Removed whatever the command does, mirroring the driver's own
		// deferred removal inside the plugin container.
		defer func() {
			_, _ = x.runner.Run(ctx, "rm -f "+quote(cred))
		}()
	}
	script += line + "\n"

	out, err := x.runner.Run(ctx, script)
	x.mu.Lock()
	x.output = append(x.output, out)
	x.mu.Unlock()

	// The plugin decides what to do next from the host's mount table, so it has
	// to see the result of what just ran.
	x.sync()
	if err != nil {
		return []byte(out), fmt.Errorf("%s on %s: %w", name, x.runner.Node, err)
	}
	return []byte(out), nil
}

// sync copies the node's mount table into the mirror the plugin reads. It is
// called after every command and before every plugin call, so the plugin's
// idempotency checks run against what the node actually has mounted.
func (x *nodeExec) sync() {
	x.t.Helper()
	out, err := x.runner.Run(context.Background(), "cat /proc/mounts")
	if err != nil {
		x.t.Fatalf("reading the node's mount table: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(x.root, "proc"), 0o755); err != nil {
		x.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(x.root, "proc", "mounts"), []byte(out), 0o644); err != nil {
		x.t.Fatal(err)
	}
}

// credentialsPathIn finds the credentials= option in a mount command.
func credentialsPathIn(args []string) string {
	for _, a := range args {
		for _, opt := range strings.Split(a, ",") {
			if path, ok := strings.CutPrefix(opt, "credentials="); ok {
				return path
			}
		}
	}
	return ""
}

// nodePlugin builds the real node plugin for the cluster node, with its
// capability preflight run against that node's OWN binaries and modules rather
// than against a fixture: the preflight is half of what this test exists to
// check, since a node that does not publish the smb topology label makes every
// SMB PVC unschedulable.
func nodePlugin(t *testing.T, r *NodeRunner) (*node.Node, *nodeExec) {
	t.Helper()
	ctx := context.Background()

	probe, err := r.Run(ctx, `
set -e
echo "MOUNT_CIFS=$(command -v mount.cifs || true)"
echo "OSRELEASE=$(cat /proc/sys/kernel/osrelease)"
echo "CIFS_KO=$(find /lib/modules/$(uname -r) -name 'cifs.ko*' -print -quit 2>/dev/null || true)"
echo MODULES_BEGIN
cat /proc/modules
`)
	if err != nil {
		t.Fatalf("probing the node's capabilities: %v\n%s", err, probe)
	}
	modules := ""
	if _, rest, ok := strings.Cut(probe, "MODULES_BEGIN\n"); ok {
		modules = rest
	}

	root := t.TempDir()
	write := func(rel, content string, mode os.FileMode) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	release := valueOf(probe, "OSRELEASE=")
	write("proc/modules", modules, 0o644)
	write("proc/sys/kernel/osrelease", release+"\n", 0o644)
	if valueOf(probe, "MOUNT_CIFS=") != "" {
		write("sbin/mount.cifs", "#!/bin/sh\n", 0o755)
	}
	if ko := valueOf(probe, "CIFS_KO="); ko != "" {
		write(filepath.Join("lib/modules", release, filepath.Base(ko)), "ELF", 0o644)
	}

	// The module load, if one is needed, happens on the node — the same thing
	// the DaemonSet's modprobe does in production.
	pf, err := node.Detect(ctx, root, func(ctx context.Context, mod string) error {
		out, err := r.Run(ctx, "modprobe "+quote(mod))
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("node capability preflight: %v", err)
	}
	if err := pf.Require(node.CapSMB); err != nil {
		t.Skipf("%s cannot serve SMB volumes: %v", r.Node, err)
	}
	if got := pf.TopologyLabels()[node.TopologyKey(node.CapSMB)]; got != "true" {
		t.Fatalf("the node publishes %s=%q; the controller requires \"true\" for a "+
			"protocol: smb volume, so every SMB PVC would be unschedulable",
			node.TopologyKey(node.CapSMB), got)
	}

	x := &nodeExec{t: t, runner: r, root: root}
	// The plugin decides whether a path is already mounted from the mount table
	// under its host root, so the mirror must hold the node's before the first
	// call. Every later refresh happens inside nodeExec.Run, which is enough
	// because only the plugin's own commands change what is mounted.
	x.sync()
	n := node.NewNode(r.Node, pf, x)
	n.Root = root
	return n, x
}

// valueOf reads a KEY=value line out of a node script's output.
func valueOf(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TestE2ESMBNodeDataPath provisions an SMB volume with the real controller and
// then makes the real node plugin stage, publish, unpublish and unstage it on a
// cluster node. It is the check that could not have passed before this driver
// had a cifs data path at all.
func TestE2ESMBNodeDataPath(t *testing.T) {
	e := requireAppliance(t)
	r := requireNode(t)
	c := e.controller(t)
	ctx := context.Background()

	user, password := e.smbUser(t)
	vol := e.createVolume(t, c, uniqueName("pvc-e2e-smb"), "smb", 1<<30, nil)
	pc := vol.GetVolumeContext()
	if pc["share"] == "" {
		t.Fatalf("the volume context carries no share name: %v", redactedKeys(pc))
	}
	if pc["server"] == "" {
		pc["server"] = e.server
	}
	for k := range pc {
		if k == "password" || k == "username" {
			t.Fatalf("the controller put %q into the volume context, which is persisted "+
				"verbatim in the PersistentVolume", k)
		}
	}

	n, x := nodePlugin(t, r)
	base := "/tmp/csi-e2e-smb-" + uniqueName("m")
	staging := base + "/globalmount"
	target := base + "/mount"
	// The kubelet owns these directories in production and they exist on the
	// node before the plugin is called; here the test creates them, because the
	// plugin's own MkdirAll runs on the machine executing the test.
	if out, err := r.Run(ctx, "mkdir -p "+quote(staging)+" "+quote(target)); err != nil {
		t.Fatalf("preparing the node's mount points: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = r.Run(context.Background(),
			"umount "+quote(target)+" 2>/dev/null; umount "+quote(staging)+" 2>/dev/null; rm -rf "+quote(base))
	})

	stage := node.StageRequest{
		VolumeID:       vol.GetVolumeId(),
		StagingPath:    staging,
		PublishContext: pc,
		// What the kubelet hands the plugin after resolving the node-stage
		// Secret the controller referenced in the publish context.
		Secrets: map[string]string{"username": user, "password": password},
	}

	if err := n.Stage(ctx, stage); err != nil {
		t.Fatalf("NodeStageVolume: %v", err)
	}
	assertMounted(t, r, staging, "cifs")

	// A repeated stage must issue nothing: the kubelet retries it.
	before := len(x.commands)
	if err := n.Stage(ctx, stage); err != nil {
		t.Fatalf("repeated NodeStageVolume: %v", err)
	}
	if len(x.commands) != before {
		t.Fatalf("a repeated stage touched the node again: %v", x.commands[before:])
	}

	if err := n.Publish(ctx, node.PublishRequest{
		VolumeID: vol.GetVolumeId(), StagingPath: staging, TargetPath: target, PublishContext: pc,
	}); err != nil {
		t.Fatalf("NodePublishVolume: %v", err)
	}
	assertMounted(t, r, target, "cifs")

	// The pod's view: write through the target path, read it back, and check the
	// share reports its own refquota rather than the whole pool.
	out, err := r.Run(ctx, fmt.Sprintf(`set -e
echo "smb payload" > %[1]s/s.txt
cat %[1]s/s.txt
echo "REPORTED_SIZE=$(df -k %[1]s | tail -1 | awk '{print $2}')"`, quote(target)))
	if err != nil {
		t.Fatalf("using the published volume: %v\n%s", err, redactOutput(out, password))
	}
	if !strings.Contains(out, "smb payload") {
		t.Fatalf("could not read back what was written:\n%s", redactOutput(out, password))
	}
	if sizeKiB := extractInt(t, out, "REPORTED_SIZE="); sizeKiB > 4*1048576 {
		t.Fatalf("the SMB share reports %d KiB for a 1 GiB volume — refquota is not applied "+
			"and the pod can see the whole pool", sizeKiB)
	}

	// Teardown, twice: the kubelet retries both halves after a partial failure
	// and after a node reboot, and neither may fail the second time.
	unpublish := node.UnpublishRequest{VolumeID: vol.GetVolumeId(), TargetPath: target}
	unstage := node.UnstageRequest{VolumeID: vol.GetVolumeId(), StagingPath: staging, PublishContext: pc}
	for i := 0; i < 2; i++ {
		if err := n.Unpublish(ctx, unpublish); err != nil {
			t.Fatalf("NodeUnpublishVolume #%d: %v", i+1, err)
		}
		if err := n.Unstage(ctx, unstage); err != nil {
			t.Fatalf("NodeUnstageVolume #%d: %v", i+1, err)
		}
	}
	assertNotMounted(t, r, target)
	assertNotMounted(t, r, staging)

	// The credential must be gone from the node, and must never have been on a
	// command line: /proc/<pid>/cmdline is world-readable, so an argument would
	// have exposed it to every process on the node.
	for _, cmd := range x.commands {
		if strings.Contains(cmd, password) {
			t.Fatalf("the driver put the SMB password on a command line: %q",
				redactOutput(cmd, password))
		}
	}
	if leftovers, err := r.Run(ctx, "ls -A /run/truenas-csi 2>/dev/null || true"); err != nil {
		t.Fatalf("checking for stranded credentials: %v", err)
	} else if strings.TrimSpace(leftovers) != "" {
		t.Fatalf("the driver left credential files behind on %s: %q", r.Node, strings.TrimSpace(leftovers))
	}

	// What this test could NOT exercise. SMB genuinely supports RWX — several
	// pods on several nodes may hold the same share — but the controller's
	// supportsAccessMode special-cases only "nfs", so it refuses the mode and no
	// RWX SMB PVC can be bound. That is plan Task 8's to fix, and it is reported
	// here rather than quietly narrowed to the modes that do work.
	resp, err := c.ValidateVolumeCapabilities(ctx, &csipb.ValidateVolumeCapabilitiesRequest{
		VolumeId: vol.GetVolumeId(),
		VolumeCapabilities: []*csipb.VolumeCapability{{
			AccessType: &csipb.VolumeCapability_Mount{Mount: &csipb.VolumeCapability_MountVolume{}},
			AccessMode: &csipb.VolumeCapability_AccessMode{
				Mode: csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities: %v", err)
	}
	if resp.GetConfirmed() == nil {
		t.Errorf("the node data path works, but the controller still refuses "+
			"MULTI_NODE_MULTI_WRITER for SMB (%q), so no RWX SMB PVC can be bound and this "+
			"test cannot exercise a shared mount. supportsAccessMode in "+
			"internal/csi/controller.go special-cases only \"nfs\"; fixing it is plan Task 8.",
			resp.GetMessage())
	}
}

// assertMounted fails unless the node reports path as a mount of the given type.
func assertMounted(t *testing.T, r *NodeRunner, path, fsType string) {
	t.Helper()
	out, err := r.Run(context.Background(), "cat /proc/mounts")
	if err != nil {
		t.Fatalf("reading the node's mount table: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[1] == path {
			if f[2] != fsType {
				t.Fatalf("%s is mounted as %q, want %q", path, f[2], fsType)
			}
			return
		}
	}
	t.Fatalf("the driver reported success but %s is not mounted on %s:\n%s", path, r.Node, out)
}

// assertNotMounted fails when the node still reports path as a mount point.
func assertNotMounted(t *testing.T, r *NodeRunner, path string) {
	t.Helper()
	out, err := r.Run(context.Background(), "cat /proc/mounts")
	if err != nil {
		t.Fatalf("reading the node's mount table: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == path {
			t.Fatalf("%s is still mounted after teardown: %q", path, line)
		}
	}
}

// redactOutput keeps a credential out of a failure message.
func redactOutput(out, secret string) string {
	if secret == "" {
		return out
	}
	return strings.ReplaceAll(out, secret, "[redacted]")
}

// TestE2ESMBProvision provisions and deletes an SMB volume with the controller
// alone, mounting nothing.
//
// It exists because the only other SMB test needs an SMB USER, which it creates
// on the appliance and which needs ACCOUNT_WRITE. The driver never creates a
// user — only shares — so that requirement belongs to the test, not to the
// driver, and it made the test skip under exactly the account it should have
// been validating: a least-privilege run went green while sharing.smb.query
// returned EACCES and no SMB volume could be provisioned at all.
//
// This one exercises what the driver actually needs (sharing.smb.create,
// .query, .update and .delete, which want SHARING_SMB_WRITE) and therefore runs
// under any account the driver is meant to work with.
func TestE2ESMBProvision(t *testing.T) {
	e := requireAppliance(t)
	c := e.controller(t)
	ctx := context.Background()

	name := uniqueName("pvc-e2e-smbprov")
	resp, err := c.CreateVolume(ctx, &csipb.CreateVolumeRequest{
		Name: name, Parameters: e.params("smb"), VolumeCapabilities: caps(),
		CapacityRange: &csipb.CapacityRange{RequiredBytes: 1 << 30},
	})
	if err != nil {
		t.Fatalf("CreateVolume(smb/%s): %v", name, err)
	}
	id := resp.GetVolume().GetVolumeId()
	t.Cleanup(func() {
		if _, err := c.DeleteVolume(ctx, &csipb.DeleteVolumeRequest{VolumeId: id}); err != nil {
			t.Errorf("cleanup DeleteVolume(%s): %v", id, err)
		}
	})

	vctx := resp.GetVolume().GetVolumeContext()
	if vctx["share"] == "" {
		t.Errorf("the volume context carries no share name: %v", redactedKeys(vctx))
	}
	// Credentials belong in a Kubernetes Secret; the volume context is written
	// verbatim into the PersistentVolume and read by anyone who can get it.
	for _, k := range []string{"password", "username"} {
		if _, ok := vctx[k]; ok {
			t.Errorf("the controller put %q into the volume context", k)
		}
	}

	// The share must really exist on the appliance, not just in the response.
	var shares []struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := e.client.CallJSON(ctx, &shares, "sharing.smb.query",
		[]any{[]any{"name", "=", vctx["share"]}}, map[string]any{}); err != nil {
		t.Fatalf("querying the SMB share back: %v", err)
	}
	if len(shares) == 0 {
		t.Fatalf("no SMB share named %q exists on the appliance", vctx["share"])
	}
}
