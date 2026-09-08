package fencing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/podmon"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

const (
	testHandle = "nas1/nfs/tank/k8s/pvc-a"
	testNode   = "worker-21"
	testUID    = types.UID("11111111-1111-1111-1111-111111111111")
)

// recordingFencer remembers every revoke, in order, and can be told to fail one
// of them. The order matters as much as the outcome: see TestRevokeHappensBeforeAnythingIsDeleted.
type recordingFencer struct {
	mu     sync.Mutex
	calls  []string
	failOn string
}

func (f *recordingFencer) Fence(_ context.Context, id volume.ID, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id.String()+"@"+nodeID)
	if f.failOn != "" && id.String() == f.failOn {
		return errors.New("the appliance refused to remove the host entry")
	}
	return nil
}

func (f *recordingFencer) revoked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// stubChecker returns a canned appliance answer.
type stubChecker struct{ reports []podmon.Report }

func (s stubChecker) Check(_ context.Context, nodeID string, ids []volume.ID) []podmon.Report {
	if s.reports != nil {
		return s.reports
	}
	// Default: the appliance positively reports nothing held.
	out := make([]podmon.Report, 0, len(ids))
	for _, id := range ids {
		out = append(out, podmon.Report{NodeID: nodeID, VolumeID: id.String(), Signals: []podmon.Signal{
			{Kind: podmon.SignalNFSLease, State: podmon.StateDisconnected, Reason: "no client"},
			{Kind: podmon.SignalVolumeIO, State: podmon.StateUnobservable, Reason: "no per-volume counter"},
		}})
	}
	return out
}

// stuckPod is the situation this controller exists for: an opted-in pod that is
// not ready, on a node the cluster has given up on.
func stuckPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-0",
			Namespace: "apps",
			UID:       testUID,
			Labels:    map[string]string{DefaultLabelKey: DefaultLabelValue},
		},
		Spec: corev1.PodSpec{
			NodeName: testNode,
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"},
				},
			}},
		},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}},
	}
}

func failedNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNode},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{
			{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute},
		}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionUnknown},
		}},
	}
}

func boundClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "apps"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-a"},
	}
}

func driverPV() *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-a"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: driver.DriverName, VolumeHandle: testHandle},
		}},
	}
}

func attachment() *storagev1.VolumeAttachment {
	pvName := "pv-a"
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "va-a"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: driver.DriverName,
			NodeName: testNode,
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
	}
}

// newTest wires a controller over a fake API server holding the given objects.
func newTest(t *testing.T, checker ConnectivityChecker, fencer VolumeFencer,
	objs ...runtime.Object) (*Controller, *fake.Clientset, *record.FakeRecorder) {
	t.Helper()
	client := fake.NewClientset(objs...)
	events := record.NewFakeRecorder(32)
	c := New(client, checker, fencer, events)
	return c, client, events
}

func podExists(t *testing.T, client *fake.Clientset) bool {
	t.Helper()
	_, err := client.CoreV1().Pods("apps").Get(context.Background(), "db-0", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("reading the pod back: %v", err)
	}
	return true
}

// TestFenceOnlyProceedsWhenEveryPreconditionHolds walks the protocol's decision
// points one at a time. Every row but the last must leave the pod alone AND
// leave the appliance untouched: a controller that revokes access and then
// decides not to delete has still cut a live workload off its data.
func TestFenceOnlyProceedsWhenEveryPreconditionHolds(t *testing.T) {
	unknown := []podmon.Report{{NodeID: testNode, VolumeID: testHandle, Signals: []podmon.Signal{
		{Kind: podmon.SignalSMBSession, State: podmon.StateUnknown, Reason: "no smb session call"},
	}}}
	connected := []podmon.Report{{NodeID: testNode, VolumeID: testHandle, Signals: []podmon.Signal{
		{Kind: podmon.SignalNFSLease, State: podmon.StateConnected, Reason: "renewed its lease 2s ago"},
	}}}
	askFailed := []podmon.Report{{NodeID: testNode, VolumeID: testHandle,
		Errors: []string{"appliance nas1 is unreachable"}}}

	cases := []struct {
		name        string
		mutatePod   func(*corev1.Pod)
		mutateNode  func(*corev1.Node)
		reports     []podmon.Report
		wantDeleted bool
		wantRevokes int
	}{
		{
			name:      "the pod did not opt in",
			mutatePod: func(p *corev1.Pod) { p.Labels = nil },
		},
		{
			name: "the pod is ready",
			mutatePod: func(p *corev1.Pod) {
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			},
		},
		{
			name: "the node is healthy: a pod can be unready for a hundred ordinary reasons",
			mutateNode: func(n *corev1.Node) {
				n.Spec.Taints = nil
				n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			},
		},
		{
			name: "the node is merely cordoned, which is maintenance and not failure",
			mutateNode: func(n *corev1.Node) {
				n.Spec.Unschedulable = true
				n.Spec.Taints = []corev1.Taint{{Key: corev1.TaintNodeUnschedulable, Effect: corev1.TaintEffectNoSchedule}}
				n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			},
		},
		{
			name:    "the node is still connected according to the appliance",
			reports: connected,
		},
		{
			name:    "the appliance could not answer for one signal",
			reports: unknown,
		},
		{
			name:    "the appliance could not be asked at all",
			reports: askFailed,
		},
		{
			name:        "everything says the node holds nothing",
			wantDeleted: true,
			wantRevokes: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod, node := stuckPod(), failedNode()
			if tc.mutatePod != nil {
				tc.mutatePod(pod)
			}
			if tc.mutateNode != nil {
				tc.mutateNode(node)
			}
			fencer := &recordingFencer{}
			c, client, _ := newTest(t, stubChecker{reports: tc.reports}, fencer,
				pod, node, boundClaim(), driverPV(), attachment())

			if err := c.handle(context.Background(), pod.DeepCopy()); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if got := podExists(t, client); got != !tc.wantDeleted {
				t.Fatalf("pod deleted = %v, want %v", !got, tc.wantDeleted)
			}
			if got := len(fencer.revoked()); got != tc.wantRevokes {
				t.Fatalf("%d revokes, want %d: %v", got, tc.wantRevokes, fencer.revoked())
			}
		})
	}
}

// TestPodIsNotDeletedWhenAnyRevokeFailed is the second load-bearing test.
//
// A pod whose access was only partly revoked must survive: its replacement will
// be scheduled the moment it disappears, and it would then share a volume with a
// node that still reaches it. Partial fencing is worse than none, because it
// looks like success.
func TestPodIsNotDeletedWhenAnyRevokeFailed(t *testing.T) {
	pod := stuckPod()
	// Two volumes, and the SECOND one's revoke fails: the first has already
	// succeeded by then, so this also pins that a half-done fence stops.
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "logs",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "logs-db-0"},
		},
	})
	claim2 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "logs-db-0", Namespace: "apps"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-b"},
	}
	pv2 := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-b"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{
				Driver: driver.DriverName, VolumeHandle: "nas1/nfs/tank/k8s/pvc-b"},
		}},
	}

	fencer := &recordingFencer{failOn: "nas1/nfs/tank/k8s/pvc-b"}
	c, client, events := newTest(t, stubChecker{}, fencer,
		pod, failedNode(), boundClaim(), driverPV(), claim2, pv2, attachment())

	if err := c.handle(context.Background(), pod.DeepCopy()); err == nil {
		t.Fatal("a failed revoke must be reported as an error, not swallowed")
	}
	if !podExists(t, client) {
		t.Fatal("the pod was deleted after a revoke failed: its replacement would now share " +
			"a volume with a node that still reaches it")
	}
	node, _ := client.CoreV1().Nodes().Get(context.Background(), testNode, metav1.GetOptions{})
	for _, taint := range node.Spec.Taints {
		if taint.Key == TaintKey {
			t.Error("the node was tainted despite the aborted fence")
		}
	}
	vas, _ := client.StorageV1().VolumeAttachments().List(context.Background(), metav1.ListOptions{})
	if len(vas.Items) != 1 {
		t.Error("VolumeAttachments were deleted despite the aborted fence")
	}
	assertEvent(t, events, EventRevokeFailed)
}

// TestRevokeHappensBeforeAnythingIsDeleted pins the ORDER, which is the part of
// the protocol that is easy to lose in a refactor and impossible to notice
// afterwards: deleting the pod object first hands the volume to the replacement
// while the old node may still be writing.
func TestRevokeHappensBeforeAnythingIsDeleted(t *testing.T) {
	pod := stuckPod()
	fencer := &recordingFencer{}
	c, client, _ := newTest(t, stubChecker{}, fencer, pod, failedNode(), boundClaim(), driverPV(), attachment())

	var order []string
	var mu sync.Mutex
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	client.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		note("delete-pod")
		return false, nil, nil
	})
	client.PrependReactor("delete", "volumeattachments", func(k8stesting.Action) (bool, runtime.Object, error) {
		note("delete-va")
		return false, nil, nil
	})
	client.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		note("taint-node")
		return false, nil, nil
	})

	if err := c.handle(context.Background(), pod.DeepCopy()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(fencer.revoked()) != 1 {
		t.Fatalf("want one revoke, got %v", fencer.revoked())
	}
	want := []string{"taint-node", "delete-va", "delete-pod"}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("cleanup order was %v, want %v (the pod object is the interlock and must go last)", order, want)
	}
	if podExists(t, client) {
		t.Fatal("the pod survived a complete fence")
	}
}

// TestARecreatedPodIsNeverKilled: between the sweep's LIST and the decision, a
// controller may have replaced the pod. Same name, new UID, quite possibly
// running happily on another node.
func TestARecreatedPodIsNeverKilled(t *testing.T) {
	stale := stuckPod()
	live := stuckPod()
	live.UID = types.UID("22222222-2222-2222-2222-222222222222")

	fencer := &recordingFencer{}
	c, client, _ := newTest(t, stubChecker{}, fencer, live, failedNode(), boundClaim(), driverPV())

	if err := c.handle(context.Background(), stale); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(fencer.revoked()) != 0 {
		t.Fatalf("access was revoked for a pod that no longer exists: %v", fencer.revoked())
	}
	if !podExists(t, client) {
		t.Fatal("the replacement pod was deleted")
	}
}

// TestVolumesOfOtherDriversAreLeftAlone: a stuck pod holding somebody else's
// storage is somebody else's problem, and force-deleting it would be this
// controller destroying a workload it cannot make safe.
func TestVolumesOfOtherDriversAreLeftAlone(t *testing.T) {
	pv := driverPV()
	pv.Spec.CSI.Driver = "ebs.csi.aws.com"
	fencer := &recordingFencer{}
	c, client, _ := newTest(t, stubChecker{}, fencer, stuckPod(), failedNode(), boundClaim(), pv)

	if err := c.handle(context.Background(), stuckPod()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if !podExists(t, client) || len(fencer.revoked()) != 0 {
		t.Fatal("a pod holding another driver's volume was fenced")
	}
}

// TestAnAbortEmitsAWarningEventSayingWhy. An operator staring at a pod that did
// not get fenced must be able to find the reason with kubectl describe.
func TestAnAbortEmitsAWarningEventSayingWhy(t *testing.T) {
	reports := []podmon.Report{{NodeID: testNode, VolumeID: testHandle, Signals: []podmon.Signal{
		{Kind: podmon.SignalNFSLease, State: podmon.StateConnected, Reason: "renewed its NFSv4 lease 2s ago"},
	}}}
	c, _, events := newTest(t, stubChecker{reports: reports}, &recordingFencer{},
		stuckPod(), failedNode(), boundClaim(), driverPV())
	if err := c.handle(context.Background(), stuckPod()); err != nil {
		t.Fatalf("handle: %v", err)
	}
	msg := assertEvent(t, events, EventAborted)
	if !strings.Contains(msg, "Warning") {
		t.Errorf("an aborted fence must be a Warning: %q", msg)
	}
	if !strings.Contains(msg, "lease") {
		t.Errorf("the abort event does not carry the appliance's reason: %q", msg)
	}
}

func assertEvent(t *testing.T, events *record.FakeRecorder, reason string) string {
	t.Helper()
	for {
		select {
		case msg := <-events.Events:
			if strings.Contains(msg, reason) {
				return msg
			}
		default:
			t.Fatalf("no %s event was recorded", reason)
			return ""
		}
	}
}

// TestFencingNeverConsultsTheNodeSelfCheck reads this package's source, in the
// style of TestLastIOProbeDoesNotUseModTime, because the regression is silent
// and catastrophic.
//
// The connectivity answer must come from the appliance. Wiring the fence to the
// node-side self-check — which is in the same package as the service it should
// use, and has an inviting name — would produce a controller that passes every
// unit test and, in production, asks a dead node whether it is dead. In the
// fencing case the node does not answer at all, so the check would either hang
// or fail; and a failure that was then read as "not connected" would fence a
// node on the strength of its own silence.
func TestFencingNeverConsultsTheNodeSelfCheck(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := stripComments(string(src))
		for _, banned := range []string{
			"NodeSelfCheck", "podmon.Serve", "internal/node", "Statfs", "iocounters",
		} {
			if strings.Contains(body, banned) {
				t.Errorf("%s references %q: the fencing decision must be sourced from the "+
					"APPLIANCE, never from the node that has stopped answering", f, banned)
			}
		}
	}
}

// stripComments removes // comments so a source guard reads code, not prose.
func stripComments(body string) string {
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
