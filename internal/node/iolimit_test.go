package node

// The cgroup path resolution is the riskiest part of this feature and the one
// with the worst failure mode: it varies by cgroup driver, by QoS class and by
// distribution, and when it is wrong nothing crashes — the pod simply runs
// unthrottled while the operator believes it is capped. So it is tested against
// fixture trees shaped like the real ones, and the io.max bytes are asserted
// exactly, because the kernel accepts a malformed line as a no-op.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPodUID = "1a2b3c4d-5e6f-4071-8899-aabbccddeeff"

// testPodUIDUnderscored is the same UID as systemd spells it inside a slice name.
var testPodUIDUnderscored = strings.ReplaceAll(testPodUID, "-", "_")

// makeCgroupTree builds a fixture cgroup hierarchy: every element of dirs is
// created relative to a fresh temp root, and each leaf gets an io.max file, as
// a real cgroup with the io controller enabled has.
func makeCgroupTree(t *testing.T, dirs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range dirs {
		full := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, ioMaxFile), nil, 0o644); err != nil {
			t.Fatalf("write io.max in %s: %v", full, err)
		}
	}
	return root
}

func TestResolvePodCgroupAcrossDriversAndQoSClasses(t *testing.T) {
	u := testPodUIDUnderscored
	cases := []struct {
		name string
		// tree is the set of directories the fixture contains; want is the one
		// the resolver must pick.
		tree []string
		want string
	}{
		{
			name: "systemd driver, guaranteed pod has no QoS segment",
			tree: []string{
				"kubepods.slice/kubepods-pod" + u + ".slice",
				"kubepods.slice/kubepods-burstable.slice",
			},
			want: "kubepods.slice/kubepods-pod" + u + ".slice",
		},
		{
			name: "systemd driver, burstable pod",
			tree: []string{
				"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + u + ".slice",
				"kubepods.slice/kubepods-besteffort.slice",
			},
			want: "kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod" + u + ".slice",
		},
		{
			name: "systemd driver, besteffort pod",
			tree: []string{
				"kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + u + ".slice",
			},
			want: "kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod" + u + ".slice",
		},
		{
			name: "cgroupfs driver, guaranteed pod keeps the UID verbatim",
			tree: []string{"kubepods/pod" + testPodUID},
			want: "kubepods/pod" + testPodUID,
		},
		{
			name: "cgroupfs driver, burstable pod",
			tree: []string{"kubepods/burstable/pod" + testPodUID},
			want: "kubepods/burstable/pod" + testPodUID,
		},
		{
			name: "cgroupfs driver, besteffort pod",
			tree: []string{"kubepods/besteffort/pod" + testPodUID},
			want: "kubepods/besteffort/pod" + testPodUID,
		},
		{
			// The direct candidates all miss here; only the fallback search
			// finds it. This is the shape a distribution that runs its kubelet
			// as a systemd service produces, and hardcoding the known paths
			// would leave those clusters silently unthrottled.
			name: "kubepods nested under the kubelet's own service slice",
			tree: []string{
				"system.slice/k3s.service/kubepods.slice/kubepods-burstable.slice/" +
					"kubepods-burstable-pod" + u + ".slice",
			},
			want: "system.slice/k3s.service/kubepods.slice/kubepods-burstable.slice/" +
				"kubepods-burstable-pod" + u + ".slice",
		},
		{
			// A QoS segment this code has never heard of must not be fatal:
			// the pod tail is what identifies the cgroup.
			name: "unknown QoS-style segment still resolves by the pod tail",
			tree: []string{"kubepods.slice/kubepods-someqos.slice/kubepods-someqos-pod" + u + ".slice"},
			want: "kubepods.slice/kubepods-someqos.slice/kubepods-someqos-pod" + u + ".slice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := makeCgroupTree(t, tc.tree...)
			got, err := resolvePodCgroup(root, testPodUID)
			if err != nil {
				t.Fatalf("resolvePodCgroup: %v", err)
			}
			want := filepath.Join(root, filepath.FromSlash(tc.want))
			if got != want {
				t.Fatalf("resolved %s, want %s", got, want)
			}
		})
	}
}

func TestResolvePodCgroupNotFound(t *testing.T) {
	cases := []struct {
		name string
		tree []string
		uid  string
	}{
		{
			name: "another pod's cgroup is never mistaken for ours",
			tree: []string{"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod99999999_0000_4000_8000_000000000000.slice"},
			uid:  testPodUID,
		},
		{
			name: "an empty hierarchy",
			tree: nil,
			uid:  testPodUID,
		},
		{
			// The pod cgroup exists but is buried deeper than the walk goes, so
			// the answer is "not found" rather than an unbounded walk of every
			// container cgroup on the node.
			name: "beyond the search depth",
			tree: []string{"a/b/c/d/e/f/g/h/kubepods-burstable-pod" + testPodUIDUnderscored + ".slice"},
			uid:  testPodUID,
		},
		{
			name: "no pod UID at all",
			tree: []string{"kubepods.slice/kubepods-pod" + testPodUIDUnderscored + ".slice"},
			uid:  "  ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := makeCgroupTree(t, tc.tree...)
			got, err := resolvePodCgroup(root, tc.uid)
			if err == nil {
				t.Fatalf("resolved %s, want an error", got)
			}
		})
	}
}

// TestResolvePodCgroupPrefersTheDirectPath pins the fast path: when both a
// known layout and something the search would also match exist, the known
// layout wins, so a node does not pay for a walk on every publish.
func TestResolvePodCgroupPrefersTheDirectPath(t *testing.T) {
	u := testPodUIDUnderscored
	root := makeCgroupTree(t,
		"aaa.slice/kubepods-burstable-pod"+u+".slice",
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod"+u+".slice",
	)
	got, err := resolvePodCgroup(root, testPodUID)
	if err != nil {
		t.Fatalf("resolvePodCgroup: %v", err)
	}
	want := filepath.Join(root, "kubepods.slice", "kubepods-burstable.slice",
		"kubepods-burstable-pod"+u+".slice")
	if got != want {
		t.Fatalf("resolved %s, want the direct candidate %s", got, want)
	}
}

// TestIOMaxLineBytes asserts the exact line written to io.max. A line the
// kernel cannot parse is rejected, but a line it CAN parse and that names the
// wrong thing is accepted and throttles nothing — so the bytes are the test.
func TestIOMaxLineBytes(t *testing.T) {
	cases := []struct {
		name   string
		limits IOLimits
		want   string
	}{
		{
			name:   "every knob unset renders as max, which also clears a previous limit",
			limits: IOLimits{},
			want:   "8:16 rbps=max wbps=max riops=max wiops=max\n",
		},
		{
			name:   "bandwidth only",
			limits: IOLimits{ReadBPS: 104857600, WriteBPS: 52428800},
			want:   "8:16 rbps=104857600 wbps=52428800 riops=max wiops=max\n",
		},
		{
			name:   "iops only",
			limits: IOLimits{ReadIOPS: 5000, WriteIOPS: 1000},
			want:   "8:16 rbps=max wbps=max riops=5000 wiops=1000\n",
		},
		{
			name:   "all four",
			limits: IOLimits{ReadBPS: 1, WriteBPS: 2, ReadIOPS: 3, WriteIOPS: 4},
			want:   "8:16 rbps=1 wbps=2 riops=3 wiops=4\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.limits.MaxLine(8, 16); got != tc.want {
				t.Fatalf("io.max line\n got %q\nwant %q", got, tc.want)
			}
		})
	}

	// A large minor number must not be truncated into a different device.
	if got, want := (IOLimits{ReadBPS: 7}).MaxLine(253, 1048575),
		"253:1048575 rbps=7 wbps=max riops=max wiops=max\n"; got != want {
		t.Fatalf("io.max line\n got %q\nwant %q", got, want)
	}
}

func TestParseIOLimits(t *testing.T) {
	cases := []struct {
		name    string
		vc      map[string]string
		want    IOLimits
		wantErr bool
	}{
		{
			name: "no limit keys at all is the free, common case",
			vc:   map[string]string{KeyProtocol: ProtocolISCSI},
			want: IOLimits{},
		},
		{
			name: "the undirected keys cap both directions",
			vc:   map[string]string{KeyBandwidthLimit: "100Mi", KeyIOPSLimit: "2000"},
			want: IOLimits{ReadBPS: 104857600, WriteBPS: 104857600, ReadIOPS: 2000, WriteIOPS: 2000},
		},
		{
			name: "a directional key overrides the undirected one",
			vc: map[string]string{
				KeyBandwidthLimit:      "100Mi",
				KeyWriteBandwidthLimit: "10Mi",
			},
			want: IOLimits{ReadBPS: 104857600, WriteBPS: 10485760},
		},
		{
			// Explicitly zeroing one direction must be possible without also
			// dropping the other.
			name: "a directional zero lifts the limit in that direction only",
			vc: map[string]string{
				KeyIOPSLimit:      "2000",
				KeyReadIOPSLimit:  "0",
				KeyWriteIOPSLimit: "500",
			},
			want: IOLimits{ReadIOPS: 0, WriteIOPS: 500},
		},
		{
			name: "decimal and binary suffixes differ, as they do everywhere else in Kubernetes",
			vc:   map[string]string{KeyReadBandwidthLimit: "1M", KeyWriteBandwidthLimit: "1Mi"},
			want: IOLimits{ReadBPS: 1000000, WriteBPS: 1048576},
		},
		{
			name: "plain byte counts need no suffix",
			vc:   map[string]string{KeyBandwidthLimit: "12345"},
			want: IOLimits{ReadBPS: 12345, WriteBPS: 12345},
		},
		{
			name: "max spells no limit",
			vc:   map[string]string{KeyBandwidthLimit: "max", KeyIOPSLimit: ""},
			want: IOLimits{},
		},
		{
			name:    "a fraction is rejected rather than rounded",
			vc:      map[string]string{KeyBandwidthLimit: "1.5Gi"},
			wantErr: true,
		},
		{
			name:    "a negative value is rejected",
			vc:      map[string]string{KeyIOPSLimit: "-1"},
			wantErr: true,
		},
		{
			name:    "an unknown unit is rejected",
			vc:      map[string]string{KeyBandwidthLimit: "100MB/s"},
			wantErr: true,
		},
		{
			name:    "an overflowing value is rejected",
			vc:      map[string]string{KeyBandwidthLimit: "18446744073709551615Ti"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIOLimits(tc.vc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %+v, want an error", got)
				}
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("error %v does not wrap ErrInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIOLimits: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parsed %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestCheckIOLimitsRefusesShareProtocols is the honesty guard. NFS and SMB
// volumes are mounts, not devices; there is nothing for io.max to throttle, and
// accepting the parameter would leave the user believing in a cap that does not
// exist.
func TestCheckIOLimitsRefusesShareProtocols(t *testing.T) {
	limited := map[string]string{KeyBandwidthLimit: "100Mi"}

	for _, proto := range []string{ProtocolNFS, ProtocolSMB} {
		t.Run(proto+" with a limit is refused", func(t *testing.T) {
			err := checkIOLimits(proto, limited)
			if err == nil {
				t.Fatal("a limit on a share-backed volume was accepted silently")
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error %v does not wrap ErrInvalidRequest", err)
			}
			// The message has to name the parameter, or the user cannot act on it.
			if !strings.Contains(err.Error(), KeyBandwidthLimit) {
				t.Fatalf("error %q does not name the offending parameter", err)
			}
		})
		t.Run(proto+" without a limit is untouched", func(t *testing.T) {
			if err := checkIOLimits(proto, map[string]string{KeyProtocol: proto}); err != nil {
				t.Fatalf("an unlimited %s volume was refused: %v", proto, err)
			}
		})
	}

	for _, proto := range []string{ProtocolISCSI, ProtocolNVMe} {
		t.Run(proto+" with a limit is accepted", func(t *testing.T) {
			if err := checkIOLimits(proto, limited); err != nil {
				t.Fatalf("a limit on a %s volume was refused: %v", proto, err)
			}
		})
	}

	t.Run("an invalid value is refused whatever the protocol", func(t *testing.T) {
		if err := checkIOLimits(ProtocolISCSI, map[string]string{KeyIOPSLimit: "lots"}); err == nil {
			t.Fatal("an unparseable limit was accepted")
		}
	})
}

// TestPublishRefusesLimitsOnNFS proves the refusal reaches the RPC — a check
// that is never called is the same as no check — and that the mount does not
// happen.
func TestPublishRefusesLimitsOnNFS(t *testing.T) {
	root := hostRoot(t)
	e := &fakeExec{}
	n := newTestNode(t, root, e, CapNFS)

	vc := nfsContext()
	vc[KeyBandwidthLimit] = "100Mi"
	err := n.Publish(t.Context(), PublishRequest{
		VolumeID:       "nas1/nfs/tank/k8s/pvc-abc",
		StagingPath:    filepath.Join(t.TempDir(), "globalmount"),
		TargetPath:     filepath.Join(t.TempDir(), "mount"),
		PublishContext: vc,
	})
	if err == nil {
		t.Fatal("Publish accepted an I/O limit on an NFS volume")
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error %v does not wrap ErrInvalidRequest", err)
	}
	if len(e.calls) != 0 {
		t.Fatalf("the volume was mounted before the limit was refused: %v", e.calls)
	}
}

// TestWriteCgroupFileNeedsAnExistingFile pins the no-O_CREATE rule: if the io
// controller is not enabled for a cgroup there is no io.max, and the driver
// must report that rather than leave a regular file behind that looks like a
// limit in force.
func TestWriteCgroupFileNeedsAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ioMaxFile)
	if err := writeCgroupFile(path, "8:16 rbps=1 wbps=max riops=max wiops=max\n"); err == nil {
		t.Fatal("wrote a limit into a cgroup that has no io.max")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a stray io.max file was created")
	}

	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed io.max: %v", err)
	}
	const line = "8:16 rbps=1048576 wbps=max riops=max wiops=max\n"
	if err := writeCgroupFile(path, line); err != nil {
		t.Fatalf("writeCgroupFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back io.max: %v", err)
	}
	if string(got) != line {
		t.Fatalf("io.max holds %q, want %q", got, line)
	}
}
