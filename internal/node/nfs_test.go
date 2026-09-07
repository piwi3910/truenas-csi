package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeExec records every command the node issues and answers from a scripted
// handler. It is the only way the tests observe what the node would do to a host.
type fakeExec struct {
	mu    sync.Mutex
	calls [][]string
	// handler answers a call. A nil handler answers every call with empty output
	// and no error.
	handler func(name string, args []string) ([]byte, error)
}

func (f *fakeExec) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	h := f.handler
	f.mu.Unlock()
	if h == nil {
		return nil, nil
	}
	return h(name, args)
}

// cmds renders every recorded call as a single space-joined string, in order.
func (f *fakeExec) cmds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// only returns the recorded calls whose binary is name.
func (f *fakeExec) only(name string) []string {
	var out []string
	for _, c := range f.cmds() {
		if strings.HasPrefix(c, name+" ") || c == name {
			out = append(out, c)
		}
	}
	return out
}

// hostRoot builds a temporary stand-in for the host filesystem root with an empty
// mount table, which the node reads to decide whether a path is already mounted.
func hostRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatalf("mkdir proc: %v", err)
	}
	writeMounts(t, root)
	return root
}

// writeMounts rewrites the fake host's mount table with one entry per mounted path.
func writeMounts(t *testing.T, root string, mounted ...string) {
	t.Helper()
	var b strings.Builder
	for _, m := range mounted {
		b.WriteString("source " + m + " nfs4 rw,relatime 0 0\n")
	}
	if err := os.WriteFile(filepath.Join(root, "proc", "mounts"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write proc/mounts: %v", err)
	}
}

// testPreflight builds a Preflight reporting exactly the listed capabilities as
// available and every other known capability as missing.
func testPreflight(have ...Capability) *Preflight {
	p := &Preflight{
		Found:   map[Capability]bool{},
		Missing: map[Capability][]string{},
	}
	want := map[Capability]bool{}
	for _, c := range have {
		want[c] = true
	}
	for _, c := range capabilityOrder {
		if want[c] {
			p.Found[c] = true
			continue
		}
		p.Found[c] = false
		p.Missing[c] = []string{requirements[c].pkg}
	}
	return p
}

// newTestNode wires a node onto a fake host root and a recording executor.
func newTestNode(t *testing.T, root string, e Executor, have ...Capability) *Node {
	t.Helper()
	n := NewNode("worker-21", testPreflight(have...), e)
	n.Root = root
	return n
}

func nfsContext() map[string]string {
	return map[string]string{
		KeyProtocol: ProtocolNFS,
		KeyServer:   "192.168.10.253",
		KeyShare:    "/mnt/Pool0/csi/pvc-abc",
	}
}

func TestE2ENFSMountLifecycle(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	staging := filepath.Join(t.TempDir(), "globalmount")

	req := StageRequest{
		VolumeID:         "pvc-abc",
		StagingPath:      staging,
		PublishContext:   nfsContext(),
		VolumeCapability: VolumeCapability{FsType: "nfs"},
	}
	if err := n.Stage(context.Background(), req); err != nil {
		t.Fatalf("Stage: %v", err)
	}

	want := "mount -t nfs -o vers=4 192.168.10.253:/mnt/Pool0/csi/pvc-abc " + staging
	got := e.cmds()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("stage commands:\n got %q\nwant [%q]", got, want)
	}

	// nfsVersion in the publish context selects the protocol version.
	e3 := &fakeExec{}
	n3 := newTestNode(t, hostRoot(t), e3, CapNFS)
	ctx3 := nfsContext()
	ctx3[KeyNFSVersion] = "3"
	req3 := req
	req3.PublishContext = ctx3
	if err := n3.Stage(context.Background(), req3); err != nil {
		t.Fatalf("Stage vers=3: %v", err)
	}
	if got := e3.cmds(); len(got) != 1 || !strings.Contains(got[0], "-o vers=3 ") {
		t.Fatalf("vers=3 stage commands: %q", got)
	}

	// Unstage umounts the staging path and does nothing else.
	writeMounts(t, root, staging)
	e.mu.Lock()
	e.calls = nil
	e.mu.Unlock()
	if err := n.Unstage(context.Background(), UnstageRequest{VolumeID: "pvc-abc", StagingPath: staging}); err != nil {
		t.Fatalf("Unstage: %v", err)
	}
	wantU := "umount " + staging
	if got := e.cmds(); len(got) != 1 || got[0] != wantU {
		t.Fatalf("unstage commands:\n got %q\nwant [%q]", got, wantU)
	}
}

func TestNFSStageIsIdempotent(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	staging := filepath.Join(t.TempDir(), "globalmount")
	req := StageRequest{
		VolumeID:       "pvc-abc",
		StagingPath:    staging,
		PublishContext: nfsContext(),
	}
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

func TestNFSUnstageAbsentMountSucceeds(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	staging := filepath.Join(t.TempDir(), "globalmount")

	if err := n.Unstage(context.Background(), UnstageRequest{VolumeID: "pvc-abc", StagingPath: staging}); err != nil {
		t.Fatalf("Unstage of absent mount: %v", err)
	}
	if got := e.cmds(); len(got) != 0 {
		t.Fatalf("unstage of absent mount issued %q", got)
	}
}

func TestNodePublishBindMounts(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)
	staging := filepath.Join(t.TempDir(), "globalmount")
	target := filepath.Join(t.TempDir(), "mount")

	req := PublishRequest{
		VolumeID:       "pvc-abc",
		StagingPath:    staging,
		TargetPath:     target,
		PublishContext: nfsContext(),
	}
	if err := n.Publish(context.Background(), req); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := e.cmds()
	if len(got) != 1 {
		t.Fatalf("publish commands: %q", got)
	}
	want := "mount -o bind " + staging + " " + target
	if got[0] != want {
		t.Fatalf("publish:\n got %q\nwant %q", got[0], want)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target directory not created: %v", err)
	}

	// A read-only publish must carry the ro option through a bind remount, which
	// is the only way Linux makes a bind mount read-only.
	eRO := &fakeExec{}
	rootRO := hostRoot(t)
	nRO := newTestNode(t, rootRO, eRO, CapNFS)
	targetRO := filepath.Join(t.TempDir(), "mount")
	reqRO := req
	reqRO.TargetPath = targetRO
	reqRO.Readonly = true
	if err := nRO.Publish(context.Background(), reqRO); err != nil {
		t.Fatalf("readonly Publish: %v", err)
	}
	joined := strings.Join(eRO.cmds(), "\n")
	if !strings.Contains(joined, "remount") || !strings.Contains(joined, "ro") {
		t.Fatalf("readonly publish did not remount ro: %q", eRO.cmds())
	}

	// Unpublish umounts the target when it is mounted, and is a success when not.
	writeMounts(t, root, target)
	e.mu.Lock()
	e.calls = nil
	e.mu.Unlock()
	if err := n.Unpublish(context.Background(), UnpublishRequest{VolumeID: "pvc-abc", TargetPath: target}); err != nil {
		t.Fatalf("Unpublish: %v", err)
	}
	if got := e.cmds(); len(got) != 1 || got[0] != "umount "+target {
		t.Fatalf("unpublish commands: %q", got)
	}
}

func TestNodeGetInfoReportsTopology(t *testing.T) {
	n := newTestNode(t, hostRoot(t), &fakeExec{}, CapNFS, CapExt4)
	info := n.GetInfo(context.Background())
	if info.NodeID != "worker-21" {
		t.Fatalf("node id %q", info.NodeID)
	}
	if info.MaxVolumesPerNode != MaxVolumesPerNode {
		t.Fatalf("max volumes %d", info.MaxVolumesPerNode)
	}
	if info.AccessibleTopology[topologyPrefix+string(CapNFS)] != "true" {
		t.Fatalf("topology: %v", info.AccessibleTopology)
	}
	if info.AccessibleTopology[topologyPrefix+string(CapXFS)] != "false" {
		t.Fatalf("topology: %v", info.AccessibleTopology)
	}
}
