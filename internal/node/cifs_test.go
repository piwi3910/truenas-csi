package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// smbSecret is the node-stage secret the kubelet resolves for an SMB volume.
const (
	smbTestUser     = "csi-smb"
	smbTestPassword = "S3cret-not-in-argv"
)

func smbContext() map[string]string {
	return map[string]string{
		KeyProtocol: ProtocolSMB,
		KeyServer:   "192.168.10.253",
		KeyShare:    "csi-pvc-abc",
		KeyUID:      "1000",
		KeyGID:      "1000",
		KeyFileMode: "0755",
		KeyDirMode:  "0755",
	}
}

func smbSecrets() map[string]string {
	return map[string]string{KeySMBUsername: smbTestUser, KeySMBPassword: smbTestPassword}
}

func smbStageRequest(staging string) StageRequest {
	return StageRequest{
		VolumeID:       "nas1/smb/Pool0/csi/pvc-abc",
		StagingPath:    staging,
		PublishContext: smbContext(),
		Secrets:        smbSecrets(),
	}
}

// credentialsOptionOf extracts the credentials= path from a recorded mount
// command, so a test can inspect the file mount.cifs would have read.
func credentialsOptionOf(t *testing.T, cmd string) string {
	t.Helper()
	for _, field := range strings.Fields(cmd) {
		for _, opt := range strings.Split(field, ",") {
			if path, ok := strings.CutPrefix(opt, "credentials="); ok {
				return path
			}
		}
	}
	t.Fatalf("the mount command carries no credentials= option: %q", cmd)
	return ""
}

// TestSMBStageMountLifecycle pins the whole SMB data path: the mount command,
// the credentials file mount.cifs reads, and the file's disappearance
// afterwards. Before this existed no pod could consume an SMB volume at all —
// internal/node had no cifs path, so NodeStageVolume rejected every SMB volume
// as an unsupported protocol.
func TestSMBStageMountLifecycle(t *testing.T) {
	root := hostRoot(t)
	staging := filepath.Join(t.TempDir(), "globalmount")

	// The credentials file only exists while mount runs, so it is read from
	// inside the fake mount rather than after Stage returns.
	var credContent string
	var credMode os.FileMode
	e := &fakeExec{}
	e.handler = func(name string, args []string) ([]byte, error) {
		if name != "mount" {
			return nil, nil
		}
		p := filepath.Join(root, credentialsOptionOf(t, strings.Join(args, " ")))
		fi, err := os.Stat(p)
		if err != nil {
			t.Errorf("mount.cifs would find no credentials file at %s: %v", p, err)
			return nil, nil
		}
		credMode = fi.Mode().Perm()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("read credentials file: %v", err)
		}
		credContent = string(b)
		return nil, nil
	}

	n := newTestNode(t, root, e, CapSMB)
	if err := n.Stage(context.Background(), smbStageRequest(staging)); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	got := e.only("mount")
	if len(got) != 1 {
		t.Fatalf("stage issued %q, want exactly one mount", e.cmds())
	}
	if !strings.Contains(got[0], "-t cifs ") ||
		!strings.HasSuffix(got[0], " //192.168.10.253/csi-pvc-abc "+staging) {
		t.Fatalf("mount command does not mount the share at the staging path: %q", got[0])
	}
	for _, want := range []string{"uid=1000", "gid=1000", "file_mode=0755", "dir_mode=0755"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("mount command is missing the %s option, so the pod would not own its "+
				"files: %q", want, got[0])
		}
	}

	if credMode != 0o600 {
		t.Fatalf("credentials file mode is %v; a credential readable by anything but root "+
			"defeats the point of using a file", credMode)
	}
	wantContent := "username=" + smbTestUser + "\npassword=" + smbTestPassword + "\n"
	if credContent != wantContent {
		t.Fatalf("credentials file content:\n got %q\nwant %q", credContent, wantContent)
	}

	// The file must be gone the instant the mount returns: the kernel holds the
	// credential in the session it established and never rereads it.
	leftover := filepath.Join(root, n.smbCredentialsPath("nas1/smb/Pool0/csi/pvc-abc"))
	if _, err := os.Stat(leftover); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the credentials file survived a successful stage at %s (%v)", leftover, err)
	}

	// Unstage umounts the staging path and leaves nothing behind.
	writeMounts(t, root, staging)
	e.mu.Lock()
	e.calls = nil
	e.mu.Unlock()
	req := UnstageRequest{VolumeID: "nas1/smb/Pool0/csi/pvc-abc", StagingPath: staging,
		PublishContext: smbContext()}
	if err := n.Unstage(context.Background(), req); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if cmds := e.cmds(); len(cmds) != 1 || cmds[0] != "umount "+staging {
		t.Fatalf("unstage commands: %q", cmds)
	}
}

// TestSMBCredentialNeverReachesAnArgument is the regression that matters most
// here: /proc/<pid>/cmdline is world-readable, so a password passed as
// `-o password=…` is visible to every process on the node, including any pod
// with hostPID. No argument this driver constructs may ever contain it.
func TestSMBCredentialNeverReachesAnArgument(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*StageRequest)
		wantErr bool
	}{
		{"a plain mount", func(*StageRequest) {}, false},
		{"a read-only mount", func(r *StageRequest) { r.VolumeCapability.Readonly = true }, false},
		{"a mount with StorageClass options",
			func(r *StageRequest) { r.VolumeCapability.MountFlags = []string{"vers=3.0", "noserverino"} }, false},
		{"a mount with a domain",
			func(r *StageRequest) { r.Secrets[KeySMBDomain] = "WORKGROUP" }, false},
		{"a mount the host refuses", func(*StageRequest) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hostRoot(t)
			e := &fakeExec{}
			if tc.wantErr {
				e.handler = func(string, []string) ([]byte, error) {
					return []byte("mount error(13): Permission denied"), errors.New("exit status 32")
				}
			}
			n := newTestNode(t, root, e, CapSMB)
			req := smbStageRequest(filepath.Join(t.TempDir(), "globalmount"))
			tc.mutate(&req)

			err := n.Stage(context.Background(), req)
			if tc.wantErr && err == nil {
				t.Fatal("Stage succeeded although the host refused the mount")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Stage: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), smbTestPassword) {
				t.Fatal("the error message leaks the SMB password")
			}
			for _, cmd := range e.cmds() {
				if strings.Contains(cmd, smbTestPassword) {
					t.Fatalf("the SMB password reached a process argument list, where "+
						"/proc makes it readable node-wide: %q", cmd)
				}
			}
			// A failed mount must not leave the credential on the node either.
			leftover := filepath.Join(root, n.smbCredentialsPath(req.VolumeID))
			if _, statErr := os.Stat(leftover); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("the credentials file survived at %s (%v)", leftover, statErr)
			}
		})
	}
}

func TestSMBStageIsIdempotent(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapSMB)
	staging := filepath.Join(t.TempDir(), "globalmount")
	req := smbStageRequest(staging)

	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	if len(e.only("mount")) != 1 {
		t.Fatalf("first stage mounts: %q", e.only("mount"))
	}

	// The host now reports the staging path as mounted, as it would after a real
	// mount. A repeated stage must be a no-op success.
	writeMounts(t, root, staging)
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if got := e.only("mount"); len(got) != 1 {
		t.Fatalf("second stage mounted again: %q", got)
	}
}

// TestSMBUnstageAndUnpublishAreIdempotent covers the two teardown paths the
// kubelet retries after a partial failure and after a node reboot.
func TestSMBUnstageAndUnpublishAreIdempotent(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapSMB)
	staging := filepath.Join(t.TempDir(), "globalmount")
	target := filepath.Join(t.TempDir(), "mount")
	unstage := UnstageRequest{VolumeID: "nas1/smb/Pool0/csi/pvc-abc", StagingPath: staging,
		PublishContext: smbContext()}

	for i := 0; i < 2; i++ {
		if err := n.Unstage(context.Background(), unstage); err != nil {
			t.Fatalf("Unstage #%d of an absent mount: %v", i+1, err)
		}
		if err := n.Unpublish(context.Background(),
			UnpublishRequest{VolumeID: unstage.VolumeID, TargetPath: target}); err != nil {
			t.Fatalf("Unpublish #%d of an absent mount: %v", i+1, err)
		}
	}
	if got := e.cmds(); len(got) != 0 {
		t.Fatalf("teardown of an absent mount issued %q", got)
	}
}

// TestSMBUnstageRemovesAStrandedCredential covers the one thing SMB teardown has
// that the other protocols do not: a stage killed between writing the
// credentials file and mounting leaves a password on the node, and unstaging the
// volume has to clear it.
func TestSMBUnstageRemovesAStrandedCredential(t *testing.T) {
	root := hostRoot(t)
	n := newTestNode(t, root, &fakeExec{}, CapSMB)
	volumeID := "nas1/smb/Pool0/csi/pvc-abc"

	stranded := filepath.Join(root, n.smbCredentialsPath(volumeID))
	if err := os.MkdirAll(filepath.Dir(stranded), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stranded, []byte("username=u\npassword=p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := UnstageRequest{VolumeID: volumeID, StagingPath: filepath.Join(t.TempDir(), "globalmount"),
		PublishContext: smbContext()}
	if err := n.Unstage(context.Background(), req); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	if _, err := os.Stat(stranded); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stranded credential survived unstaging at %s (%v)", stranded, err)
	}
}

// TestSMBStageRejectsAnIncompleteRequest checks that every missing input is
// reported as an invalid request naming what is absent, rather than as a mount
// failure an operator has to decode.
func TestSMBStageRejectsAnIncompleteRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*StageRequest)
		wantIn  string
		wantCap bool
	}{
		{"no server", func(r *StageRequest) { delete(r.PublishContext, KeyServer) }, `"server"`, true},
		{"no share", func(r *StageRequest) { delete(r.PublishContext, KeyShare) }, `"share"`, true},
		{"no staging path", func(r *StageRequest) { r.StagingPath = "" }, "staging path", true},
		{"no secret at all", func(r *StageRequest) { r.Secrets = nil }, `"password"`, true},
		{"a secret without a password",
			func(r *StageRequest) { delete(r.Secrets, KeySMBPassword) }, `"password"`, true},
		{"a node without cifs-utils", func(*StageRequest) {}, "cifs-utils", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &fakeExec{}
			var have []Capability
			if tc.wantCap {
				have = []Capability{CapSMB}
			}
			n := newTestNode(t, hostRoot(t), e, have...)
			req := smbStageRequest(filepath.Join(t.TempDir(), "globalmount"))
			tc.mutate(&req)

			err := n.Stage(context.Background(), req)
			if err == nil {
				t.Fatal("Stage succeeded on an incomplete request")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error %q does not name %s", err, tc.wantIn)
			}
			if got := e.cmds(); len(got) != 0 {
				t.Fatalf("a request that cannot succeed still touched the host: %q", got)
			}
		})
	}
}

// TestProtocolOfResolvesSMB fails if protocolOf stops recognising SMB. That is
// exactly the regression this task fixed: the node package knew nothing of smb
// or cifs, so protocolOf fell through to "" and NodeStageVolume rejected every
// SMB volume with InvalidArgument, no matter how correctly the controller had
// provisioned it.
func TestProtocolOfResolvesSMB(t *testing.T) {
	for _, tc := range []struct {
		name string
		pc   map[string]string
		want string
	}{
		{"an explicit smb protocol", map[string]string{KeyProtocol: "smb"}, ProtocolSMB},
		{"an upper-case smb protocol", map[string]string{KeyProtocol: "SMB"}, ProtocolSMB},
		{"the smb backend's own publish context", smbContext(), ProtocolSMB},
		{"a share name with no protocol",
			map[string]string{KeyServer: "nas", KeyShare: "csi-pvc-abc"}, ProtocolSMB},
		{"an export path with no protocol",
			map[string]string{KeyServer: "nas", KeyShare: "/mnt/Pool0/csi/pvc-abc"}, ProtocolNFS},
		{"a server alone is nfs",
			map[string]string{KeyServer: "nas"}, ProtocolNFS},
		{"an explicit nfs protocol still wins",
			map[string]string{KeyProtocol: "nfs", KeyServer: "nas", KeyShare: "weird-name"}, ProtocolNFS},
		{"nothing at all", map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := protocolOf(tc.pc); got != tc.want {
				t.Fatalf("protocolOf(%v) = %q, want %q — a node that cannot resolve the "+
					"protocol fails NodeStageVolume with InvalidArgument", tc.pc, got, tc.want)
			}
		})
	}
}

// TestPreflightDetectsCIFS fails if the cifs capability disappears from the
// preflight table. Without it no node publishes the smb topology label, and
// because the controller requires that label for a protocol: smb volume, every
// SMB PVC becomes unschedulable — the second half of the same original bug.
func TestPreflightDetectsCIFS(t *testing.T) {
	if _, ok := requirements[CapSMB]; !ok {
		t.Fatal("CapSMB has no entry in the preflight requirements table")
	}
	found := false
	for _, c := range capabilityOrder {
		if c == CapSMB {
			found = true
		}
	}
	if !found {
		t.Fatal("CapSMB is absent from capabilityOrder, so no node would publish an smb " +
			"topology label and every SMB PVC would be unschedulable")
	}

	// A node with neither the binary nor the module must name the package an
	// operator installs, not just the binary that is missing.
	bare, err := Detect(context.Background(), fakeRoot(t, nil, nil, nil), func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if bare.Found[CapSMB] {
		t.Fatal("CapSMB reported available on a root without mount.cifs")
	}
	if !strings.Contains(strings.Join(bare.Missing[CapSMB], ","), "cifs-utils") {
		t.Fatalf("the missing list must name the cifs-utils package, got %v", bare.Missing[CapSMB])
	}

	// A node that has both must advertise it as a topology segment.
	ready, err := Detect(context.Background(),
		fakeRoot(t, []string{"sbin/mount.cifs"}, []string{"cifs"}, nil),
		func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !ready.Found[CapSMB] {
		t.Fatalf("CapSMB must be available with mount.cifs and the cifs module present, missing %v",
			ready.Missing[CapSMB])
	}
	if got := ready.TopologyLabels()[TopologyKey(CapSMB)]; got != "true" {
		t.Fatalf("topology label %s = %q, want \"true\"", TopologyKey(CapSMB), got)
	}
}

// TestSMBHealthTargetAddress pins the address the health monitor probes for an
// SMB volume: without it a staged SMB volume is watched with no data address and
// its backend can never be reported unreachable.
func TestSMBHealthTargetAddress(t *testing.T) {
	if got := dataAddrOf(smbContext()); got != "192.168.10.253:445" {
		t.Fatalf("dataAddrOf(smb) = %q, want the server on the SMB port", got)
	}
}
