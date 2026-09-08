package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	truenasv1alpha1 "github.com/piwi3910/truenas-csi/operator/api/v1alpha1"
	"github.com/piwi3910/truenas-csi/operator/internal/chartrender"
	"github.com/piwi3910/truenas-csi/operator/internal/credentials"
	"github.com/piwi3910/truenas-csi/operator/internal/health"
	"github.com/piwi3910/truenas-csi/operator/internal/upgrade"
)

// chartDir is the ONE copy of the driver's manifests in this repository. The
// operator renders it; `helm install` installs it. Nothing here re-implements a
// Deployment, and this path is what keeps that honest.
const chartDir = "../../../deploy/helm/truenas-csi"

// recordingApplier stands in for the API server. Reconciliation is a decision
// procedure, and these tests are about the decisions: what gets rendered, what
// gets applied, and what deliberately does not.
type recordingApplier struct {
	mu      sync.Mutex
	applied []*unstructured.Unstructured
	err     error
}

func (a *recordingApplier) Apply(_ context.Context, obj *unstructured.Unstructured) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.applied = append(a.applied, obj.DeepCopy())
	return nil
}

func (a *recordingApplier) kinds() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.applied))
	for _, o := range a.applied {
		out = append(out, o.GetKind()+"/"+o.GetName())
	}
	sort.Strings(out)
	return out
}

func (a *recordingApplier) find(kind string) *unstructured.Unstructured {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, o := range a.applied {
		if o.GetKind() == kind {
			return o
		}
	}
	return nil
}

func (a *recordingApplier) snapshot(t *testing.T) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	docs := make([]string, 0, len(a.applied))
	for _, o := range a.applied {
		raw, err := json.Marshal(o.Object)
		if err != nil {
			t.Fatalf("marshal applied object: %v", err)
		}
		docs = append(docs, string(raw))
	}
	sort.Strings(docs)
	return strings.Join(docs, "\n")
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme, appsv1.AddToScheme, truenasv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("build scheme: %v", err)
		}
	}
	return s
}

func loadTestChart(t *testing.T) *chart.Chart {
	t.Helper()
	ch, err := chartrender.LoadChart(filepath.Clean(chartDir))
	if err != nil {
		t.Fatalf("load in-repo chart: %v", err)
	}
	return ch
}

func testDriver() *truenasv1alpha1.TrueNASCSIDriver {
	return &truenasv1alpha1.TrueNASCSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: "truenas", Generation: 1, UID: types.UID("uid-1")},
		Spec: truenasv1alpha1.TrueNASCSIDriverSpec{
			Namespace: "truenas-csi",
			LogLevel:  "info",
			Backends: []truenasv1alpha1.BackendSpec{{
				Name:            "nas1",
				Endpoint:        "wss://nas1.example.com/api/current",
				Username:        "csi",
				APIKeySecretRef: truenasv1alpha1.SecretKeyReference{Name: "truenas-credentials", Key: "apiKey"},
				Pool:            "tank",
				ParentDataset:   "tank/k8s",
			}},
			Controller: truenasv1alpha1.ControllerSpec{Replicas: 2},
			Node:       truenasv1alpha1.NodeSpec{KubeletDir: "/var/lib/kubelet"},
			StorageClasses: []truenasv1alpha1.StorageClassSpec{{
				Name:     "truenas-nfs",
				Protocol: "nfs",
				Backend:  "nas1",
			}},
		},
	}
}

func credentialSecret(resourceVersion, key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       "truenas-csi",
			Name:            "truenas-credentials",
			ResourceVersion: resourceVersion,
		},
		Data: map[string][]byte{"apiKey": []byte(key)},
	}
}

func newReconciler(t *testing.T, applier Applier, probe BackendProbe, objs ...client.Object) *TrueNASCSIDriverReconciler {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&truenasv1alpha1.TrueNASCSIDriver{}).
		Build()
	return &TrueNASCSIDriverReconciler{
		Client:  c,
		Scheme:  s,
		Chart:   loadTestChart(t),
		Applier: applier,
		Probe:   probe,
		Now:     func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// nodeDaemonSet is the node plugin DaemonSet as the API server would report it,
// targeting `desired` nodes and having observed the current generation.
//
// Tests that want a settled cluster seed it with desired = 0, which is the
// truth in an envtest-less fake client: there are no Node objects, so a
// DaemonSet belongs on no node and its rollout really is finished. Without it
// the reconciler sees no DaemonSet at all and correctly refuses to call the
// rollout done — see TestStatusReportsBackendHealth.
func nodeDaemonSet(desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "truenas-csi",
			Name:       "truenas-csi-node",
			Generation: 1,
			Labels:     map[string]string{"app.kubernetes.io/component": "node"},
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     1,
			DesiredNumberScheduled: desired,
		},
	}
}

func reconcileOnce(t *testing.T, r *TrueNASCSIDriverReconciler) *truenasv1alpha1.TrueNASCSIDriver {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "truenas"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &truenasv1alpha1.TrueNASCSIDriver{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "truenas"}, got); err != nil {
		t.Fatalf("read back resource: %v", err)
	}
	return got
}

// TestReconcileRendersChart proves the operator installs the in-repo chart, not
// a private copy of the manifests. If someone ever "simplifies" this by
// hand-writing the Deployment in Go, the two install paths start drifting and
// only the untested one breaks.
func TestReconcileRendersChart(t *testing.T) {
	applier := &recordingApplier{}
	r := newReconciler(t, applier, nil, testDriver(), credentialSecret("1", "1-secret-key"))
	reconcileOnce(t, r)

	want := map[string]bool{"Deployment": false, "DaemonSet": false, "CSIDriver": false, "Secret": false, "StorageClass": false}
	for _, obj := range applier.applied {
		if _, tracked := want[obj.GetKind()]; tracked {
			want[obj.GetKind()] = true
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("no %s was applied; rendered set was %v", kind, applier.kinds())
		}
	}

	// The rendered Secret must carry the key resolved from the referenced
	// Secret. That is the only place the credential is allowed to appear.
	secret := applier.find("Secret")
	if secret == nil {
		t.Fatal("no Secret rendered")
	}
	cfg, found, err := unstructured.NestedString(secret.Object, "stringData", "config.yaml")
	if err != nil || !found {
		t.Fatalf("rendered Secret has no config.yaml: %v", err)
	}
	if !strings.Contains(cfg, "1-secret-key") {
		t.Error("the rendered driver config does not contain the resolved API key")
	}
	if !strings.Contains(cfg, "wss://nas1.example.com/api/current") {
		t.Error("the rendered driver config does not contain the backend endpoint")
	}

	// The operator drives the node rollout, so the DaemonSet must be OnDelete.
	// Left on RollingUpdate the DaemonSet controller would replace plugin pods
	// on its own, with no idea which node has a volume mid-stage.
	ds := applier.find("DaemonSet")
	if ds == nil {
		t.Fatal("no DaemonSet rendered")
	}
	strategy, _, _ := unstructured.NestedString(ds.Object, "spec", "updateStrategy", "type")
	if strategy != "OnDelete" {
		t.Errorf("DaemonSet updateStrategy = %q, want OnDelete: the operator owns the rollout", strategy)
	}
	if _, ok := ds.GetAnnotations()[RevisionAnnotation]; ok {
		t.Error("the revision annotation belongs on the pod template, not on the DaemonSet object")
	}
	tmplAnnotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if tmplAnnotations[RevisionAnnotation] == "" {
		t.Error("the node pod template carries no revision annotation, so the operator cannot tell an old plugin pod from a new one")
	}

	// Everything applied must be owned by the CR, or deleting the CR leaves the
	// driver behind.
	for _, obj := range applier.applied {
		owners := obj.GetOwnerReferences()
		if len(owners) != 1 || owners[0].Kind != "TrueNASCSIDriver" {
			t.Errorf("%s/%s has owner references %v, want exactly one TrueNASCSIDriver",
				obj.GetKind(), obj.GetName(), owners)
		}
	}
}

// TestReconcileIsIdempotent checks that a second reconcile of an unchanged spec
// produces the same objects. A render that is not deterministic makes every
// reconcile a write, which means a rolling restart of the whole driver every
// time the operator resyncs.
func TestReconcileIsIdempotent(t *testing.T) {
	driver := testDriver()
	secret := credentialSecret("1", "1-secret-key")

	first := &recordingApplier{}
	r := newReconciler(t, first, nil, driver, secret)
	reconcileOnce(t, r)
	before := first.snapshot(t)

	second := &recordingApplier{}
	r.Applier = second
	reconcileOnce(t, r)
	after := second.snapshot(t)

	if before != after {
		t.Errorf("two reconciles of an unchanged spec produced different objects.\nfirst:\n%s\n\nsecond:\n%s", before, after)
	}
	if len(first.applied) != len(second.applied) {
		t.Errorf("object count changed between reconciles: %d then %d", len(first.applied), len(second.applied))
	}
}

// TestVersionSkewRefused checks that an incompatible driver version is refused
// outright rather than half-applied.
//
// The failure this prevents is specific: a new driver image rolled out against
// sidecars it cannot talk to leaves the controller up, the provisioner failing,
// PVCs unbound, and the DaemonSet mid-rollout with half the nodes unable to
// mount anything. An unchanged cluster with a Degraded condition on the
// resource is strictly better.
func TestVersionSkewRefused(t *testing.T) {
	driver := testDriver()
	driver.Spec.Image.Tag = "2.0.0" // beyond what this operator knows how to manage

	applier := &recordingApplier{}
	r := newReconciler(t, applier, nil, driver, credentialSecret("1", "1-secret-key"))
	got := reconcileOnce(t, r)

	if len(applier.applied) != 0 {
		t.Fatalf("a refused upgrade applied %d object(s): %v", len(applier.applied), applier.kinds())
	}

	degraded := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %+v, want True", degraded)
	}
	if degraded.Reason != truenasv1alpha1.ReasonVersionSkew {
		t.Errorf("Degraded reason = %q, want %q", degraded.Reason, truenasv1alpha1.ReasonVersionSkew)
	}
	if degraded.Message == "" {
		t.Error("a refusal with no message leaves an administrator with nothing to act on")
	}
	ready := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready condition = %+v, want False", ready)
	}
	if got.Status.ObservedGeneration != driver.Generation {
		t.Errorf("observedGeneration = %d, want %d: a stale status is indistinguishable from a missing one",
			got.Status.ObservedGeneration, driver.Generation)
	}
}

// TestKeyRotationRestartsPods checks that changing the referenced Secret
// actually restarts the driver.
//
// A CSI driver reads its API key once and holds the websocket open for the life
// of the process, and it treats an authentication failure as terminal and never
// retries. So a rotated key that does not restart the pods means the driver
// keeps presenting the old one until the appliance rejects it, and then stays
// broken.
func TestKeyRotationRestartsPods(t *testing.T) {
	driver := testDriver()

	before := &recordingApplier{}
	r := newReconciler(t, before, nil, driver, credentialSecret("1", "1-old-key"))
	reconcileOnce(t, r)

	oldDS := before.find("DaemonSet")
	oldDeploy := before.find("Deployment")
	if oldDS == nil || oldDeploy == nil {
		t.Fatal("first reconcile rendered no workloads")
	}
	oldNodeRevision := templateAnnotation(t, oldDS, RevisionAnnotation)
	oldControllerChecksum := templateAnnotation(t, oldDeploy, "checksum/config")

	// The administrator rotates the key on the appliance and updates the Secret.
	rotated := &corev1.Secret{}
	if err := r.Get(context.Background(),
		types.NamespacedName{Namespace: "truenas-csi", Name: "truenas-credentials"}, rotated); err != nil {
		t.Fatalf("read secret: %v", err)
	}
	rotated.Data["apiKey"] = []byte("1-new-key")
	if err := r.Update(context.Background(), rotated); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}

	after := &recordingApplier{}
	r.Applier = after
	reconcileOnce(t, r)

	newDS := after.find("DaemonSet")
	newDeploy := after.find("Deployment")
	if newDS == nil || newDeploy == nil {
		t.Fatal("second reconcile rendered no workloads")
	}
	if got := templateAnnotation(t, newDeploy, "checksum/config"); got == oldControllerChecksum {
		t.Error("the controller pod template is unchanged after a key rotation, so the controller keeps the old key in memory")
	}
	if got := templateAnnotation(t, newDS, RevisionAnnotation); got == oldNodeRevision {
		t.Error("the node pod template is unchanged after a key rotation, so node plugins keep the old key in memory")
	}
	cfg, _, _ := unstructured.NestedString(after.find("Secret").Object, "stringData", "config.yaml")
	if !strings.Contains(cfg, "1-new-key") {
		t.Error("the rendered config still carries the old API key")
	}

	// And the restarts happen in order: the controller first, nodes only once it
	// is healthy on the new key. If the new key is wrong, the failure should
	// land on two controller pods, not on every node in the cluster at once.
	if got := credentials.Plan("old", "new", false, false); got.Stage != credentials.StageController {
		t.Errorf("rotation starts at stage %q, want the controller first", got.Stage)
	}
	if got := credentials.Plan("old", "new", true, false); got.Stage != credentials.StageWaitController {
		t.Errorf("rotation with an unhealthy controller is at stage %q, want it to wait", got.Stage)
	}
	if got := credentials.Plan("old", "new", true, true); got.Stage != credentials.StageNodes {
		t.Errorf("rotation with a healthy controller is at stage %q, want the node roll", got.Stage)
	}
}

// TestStatusReportsBackendHealth checks that the driver's own view of its
// appliances reaches the resource's status, including the orphan count.
func TestStatusReportsBackendHealth(t *testing.T) {
	driver := testDriver()
	driver.Spec.Backends = append(driver.Spec.Backends, truenasv1alpha1.BackendSpec{
		Name:            "nas2",
		Endpoint:        "wss://nas2.example.com/api/current",
		Username:        "csi",
		APIKeySecretRef: truenasv1alpha1.SecretKeyReference{Name: "truenas-credentials", Key: "apiKey"},
		Pool:            "tank2",
		ParentDataset:   "tank2/k8s",
	})

	controllerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "truenas-csi",
			Name:      "truenas-csi-controller-abc",
			Labels:    map[string]string{"app.kubernetes.io/component": "controller"},
		},
		Status: corev1.PodStatus{
			PodIP:      "10.42.0.7",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}

	probe := &fakeProbe{result: map[string]health.Backend{
		"nas1": {Up: true, Orphans: 3},
		"nas2": {Up: false, Orphans: 0},
	}}

	// First, the state a fresh install is actually in: the DaemonSet has been
	// applied and wants a node, and no plugin pod exists yet. That must NOT
	// read as a finished rollout — it used to, because rollout.Next called
	// 0 updated of 0 total "done", so the same status said Ready=True and
	// "waiting for the node DaemonSet to create its pods" at once.
	pending := newReconciler(t, &recordingApplier{}, probe, driver.DeepCopy(),
		credentialSecret("1", "1-secret-key"), controllerPod.DeepCopy(), nodeDaemonSet(1))
	installing := reconcileOnce(t, pending)

	if ready := meta.FindStatusCondition(installing.Status.Conditions, truenasv1alpha1.ConditionReady); ready == nil ||
		ready.Status != metav1.ConditionFalse {
		t.Errorf("Ready = %+v while the node DaemonSet has created no pods, want False", ready)
	}
	if installing.Status.Rollout == nil || installing.Status.Rollout.WaitingFor == "" {
		t.Error("no rollout.waitingFor while the node plugin has not started anywhere")
	}
	if installing.Annotations[PreviousVersionAnnotation] != "" {
		t.Errorf("the install recorded version %q before any node plugin ran",
			installing.Annotations[PreviousVersionAnnotation])
	}

	// Then the settled cluster this test is really about. There are no Node
	// objects in a fake client, so a DaemonSet that has been observed and
	// targets zero nodes has genuinely finished rolling out — the case that
	// must not be confused with the one above.
	applier := &recordingApplier{}
	r := newReconciler(t, applier, probe, driver, credentialSecret("1", "1-secret-key"),
		controllerPod, nodeDaemonSet(0))
	got := reconcileOnce(t, r)

	if probe.url != "http://10.42.0.7:9090/metrics" {
		t.Errorf("probed %q, want the ready controller pod's metrics endpoint", probe.url)
	}
	if len(got.Status.Backends) != 2 {
		t.Fatalf("status reports %d backends, want 2", len(got.Status.Backends))
	}
	byName := map[string]truenasv1alpha1.BackendStatus{}
	for _, b := range got.Status.Backends {
		byName[b.Name] = b
	}
	if byName["nas1"].Reachable != "True" {
		t.Errorf("nas1 reachable = %q, want True", byName["nas1"].Reachable)
	}
	if byName["nas1"].OrphanedVolumes == nil || *byName["nas1"].OrphanedVolumes != 3 {
		t.Errorf("nas1 orphan count = %v, want 3", byName["nas1"].OrphanedVolumes)
	}
	if byName["nas2"].Reachable != "False" {
		t.Errorf("nas2 reachable = %q, want False", byName["nas2"].Reachable)
	}
	if byName["nas2"].Message == "" {
		t.Error("an unreachable backend with no message tells an administrator nothing")
	}

	// An unreachable appliance is a degraded driver: PVCs against it will fail.
	degraded := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Status != metav1.ConditionTrue || degraded.Reason != truenasv1alpha1.ReasonBackendUnreachable {
		t.Errorf("Degraded condition = %+v, want True/%s", degraded, truenasv1alpha1.ReasonBackendUnreachable)
	}
}

// TestReconcileRefusesMissingSecret checks the operator says which Secret is
// missing rather than rendering a driver with an empty credential.
func TestReconcileRefusesMissingSecret(t *testing.T) {
	applier := &recordingApplier{}
	r := newReconciler(t, applier, nil, testDriver())
	got := reconcileOnce(t, r)

	if len(applier.applied) != 0 {
		t.Fatalf("applied %d object(s) with no credential available", len(applier.applied))
	}
	degraded := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionDegraded)
	if degraded == nil || degraded.Reason != truenasv1alpha1.ReasonSecretMissing {
		t.Fatalf("Degraded condition = %+v, want reason %s", degraded, truenasv1alpha1.ReasonSecretMissing)
	}
	if !strings.Contains(degraded.Message, "truenas-credentials") {
		t.Errorf("message %q does not name the missing Secret", degraded.Message)
	}
}

// upgradeTable is a table with more than one release in it, so the refusal
// branches have something to refuse. The shipped table has a single entry
// because a single version has shipped; the mechanism, not that table's current
// contents, is what these tests are about.
var upgradeTable = upgrade.Table{
	"0.1.0": {},
	"0.3.0": {},
	"0.5.0": {MinUpgradeFrom: "0.3.0", MinDowngradeTo: "0.3.0"},
	"0.9.0": {MinUpgradeFrom: "0.5.0", MinDowngradeTo: "0.5.0"},
}

// upgradeCase drives one reconcile of a driver moving between two versions.
type upgradeCase struct {
	name string
	// previous is the version recorded on the CR; empty means a fresh install.
	previous string
	// requested is the version in spec.image.tag.
	requested string
	// wantRefused is true when the operator must apply nothing at all.
	wantRefused bool
	// wantMessage is a substring the refusal has to contain, so the message
	// stays something an administrator can act on rather than a bare "no".
	wantMessage string
	// wantRecorded is the version the CR should carry afterwards.
	wantRecorded string
}

// TestUpgradePathGating is the table for the whole mechanism: which steps are
// allowed, which are refused, and what the resource says afterwards.
//
// The failure being prevented is a version skipped over a release whose
// migration a later driver depends on. That leaves volumes on the appliance in
// a shape the running driver does not understand, and it is discovered one
// workload at a time. An unchanged cluster with a Degraded condition is
// strictly better, which is the same trade the sidecar skew check makes.
func TestUpgradePathGating(t *testing.T) {
	tests := []upgradeCase{
		{
			name:         "fresh install is never gated",
			previous:     "",
			requested:    "0.9.0",
			wantRecorded: "0.9.0",
		},
		{
			name:         "supported single step",
			previous:     "0.5.0",
			requested:    "0.9.0",
			wantRecorded: "0.9.0",
		},
		{
			name:         "no change at all",
			previous:     "0.9.0",
			requested:    "0.9.0",
			wantRecorded: "0.9.0",
		},
		{
			name:         "skipping the migration release is refused",
			previous:     "0.3.0",
			requested:    "0.9.0",
			wantRefused:  true,
			wantMessage:  "upgrade to v0.5.0 first",
			wantRecorded: "0.3.0",
		},
		{
			name:         "downgrade past the release's own floor is refused",
			previous:     "0.9.0",
			requested:    "0.3.0",
			wantRefused:  true,
			wantMessage:  "downgrade from v0.9.0 to v0.3.0 is not supported",
			wantRecorded: "0.9.0",
		},
		{
			name:         "downgrade within the declared window",
			previous:     "0.9.0",
			requested:    "0.5.0",
			wantRecorded: "0.5.0",
		},
		{
			name:         "a development tag is not gated in either direction",
			previous:     "0.3.0",
			requested:    "main",
			wantRecorded: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			driver := testDriver()
			driver.Spec.Image.Tag = tt.requested
			if tt.previous != "" {
				driver.Annotations = map[string]string{PreviousVersionAnnotation: tt.previous}
			}

			applier := &recordingApplier{}
			recorder := record.NewFakeRecorder(16)
			r := newReconciler(t, applier, nil, driver, credentialSecret("1", "1-secret-key"),
				nodeDaemonSet(0))
			r.Recorder = recorder
			r.UpgradeTable = upgradeTable
			got := reconcileOnce(t, r)

			degraded := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionDegraded)
			if tt.wantRefused {
				if len(applier.applied) != 0 {
					t.Fatalf("a refused upgrade applied %d object(s): %v", len(applier.applied), applier.kinds())
				}
				if degraded == nil || degraded.Status != metav1.ConditionTrue ||
					degraded.Reason != truenasv1alpha1.ReasonUpgradeNotSupported {
					t.Fatalf("Degraded condition = %+v, want True/%s", degraded, truenasv1alpha1.ReasonUpgradeNotSupported)
				}
				if !strings.Contains(degraded.Message, tt.wantMessage) {
					t.Errorf("refusal message %q does not say %q", degraded.Message, tt.wantMessage)
				}
				ready := meta.FindStatusCondition(got.Status.Conditions, truenasv1alpha1.ConditionReady)
				if ready == nil || ready.Status != metav1.ConditionFalse {
					t.Errorf("Ready condition = %+v, want False", ready)
				}
				if got.Status.AppliedVersion == tt.requested {
					t.Errorf("status reports appliedVersion %q, but nothing was applied", got.Status.AppliedVersion)
				}
				assertEvent(t, recorder, "Warning", truenasv1alpha1.ReasonUpgradeNotSupported)
			} else {
				if len(applier.applied) == 0 {
					t.Fatal("an allowed upgrade applied nothing")
				}
				if degraded != nil && degraded.Status == metav1.ConditionTrue {
					t.Errorf("Degraded condition = %+v, want False for an allowed upgrade", degraded)
				}
			}

			if recorded := got.Annotations[PreviousVersionAnnotation]; recorded != tt.wantRecorded {
				t.Errorf("recorded version = %q, want %q", recorded, tt.wantRecorded)
			}
		})
	}
}

// TestFailedUpgradeDoesNotRecordTheVersion is the reason the annotation is
// written at the end of a reconcile rather than at the start.
//
// If the requested version were recorded before it was known to work, a failed
// upgrade would leave the CR claiming a version that never ran — and the next
// attempt, including the attempt to go back, would be judged from a version
// that was never on the cluster.
func TestFailedUpgradeDoesNotRecordTheVersion(t *testing.T) {
	driver := testDriver()
	driver.Spec.Image.Tag = "0.9.0"
	driver.Annotations = map[string]string{PreviousVersionAnnotation: "0.5.0"}

	applier := &recordingApplier{err: errors.New("the API server said no")}
	r := newReconciler(t, applier, nil, driver, credentialSecret("1", "1-secret-key"))
	r.UpgradeTable = upgradeTable

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "truenas"},
	}); err == nil {
		t.Fatal("reconcile returned no error although every apply failed")
	}

	got := &truenasv1alpha1.TrueNASCSIDriver{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "truenas"}, got); err != nil {
		t.Fatalf("read back resource: %v", err)
	}
	if recorded := got.Annotations[PreviousVersionAnnotation]; recorded != "0.5.0" {
		t.Errorf("recorded version = %q after a failed apply, want the version that actually ran (0.5.0)", recorded)
	}
}

// TestSuccessfulUpgradeEmitsAnEvent checks the other half of "visible": a
// completed version change is something an operator can find in
// `kubectl describe` without diffing annotations.
func TestSuccessfulUpgradeEmitsAnEvent(t *testing.T) {
	driver := testDriver()
	driver.Spec.Image.Tag = "0.9.0"
	driver.Annotations = map[string]string{PreviousVersionAnnotation: "0.5.0"}

	recorder := record.NewFakeRecorder(16)
	r := newReconciler(t, &recordingApplier{}, nil, driver, credentialSecret("1", "1-secret-key"),
		nodeDaemonSet(0))
	r.Recorder = recorder
	r.UpgradeTable = upgradeTable
	reconcileOnce(t, r)

	assertEvent(t, recorder, "Normal", truenasv1alpha1.ReasonUpgraded)
}

// TestFreshInstallEmitsNoUpgradeEvent: a first install is not an upgrade, and
// an event claiming otherwise would be noise in exactly the place an operator
// looks during a real one.
func TestFreshInstallEmitsNoUpgradeEvent(t *testing.T) {
	driver := testDriver()
	driver.Spec.Image.Tag = "0.9.0"

	recorder := record.NewFakeRecorder(16)
	r := newReconciler(t, &recordingApplier{}, nil, driver, credentialSecret("1", "1-secret-key"),
		nodeDaemonSet(0))
	r.Recorder = recorder
	r.UpgradeTable = upgradeTable
	reconcileOnce(t, r)

	select {
	case e := <-recorder.Events:
		t.Errorf("a fresh install emitted the event %q", e)
	default:
	}
}

// assertEvent drains the recorder looking for one event of the given type and
// reason.
func assertEvent(t *testing.T, recorder *record.FakeRecorder, eventType, reason string) {
	t.Helper()
	var seen []string
	for {
		select {
		case e := <-recorder.Events:
			seen = append(seen, e)
			if strings.HasPrefix(e, eventType+" "+reason+" ") {
				return
			}
		default:
			t.Errorf("no %s/%s event; a refusal only visible in a status condition is easy to miss. Saw: %v",
				eventType, reason, seen)
			return
		}
	}
}

type fakeProbe struct {
	result map[string]health.Backend
	err    error
	url    string
}

func (p *fakeProbe) Scrape(_ context.Context, url string) (map[string]health.Backend, error) {
	p.url = url
	return p.result, p.err
}

func templateAnnotation(t *testing.T, obj *unstructured.Unstructured, key string) string {
	t.Helper()
	annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		t.Fatalf("read pod template annotations of %s/%s: %v", obj.GetKind(), obj.GetName(), err)
	}
	v, ok := annotations[key]
	if !ok {
		t.Fatalf("%s/%s pod template has no %s annotation (%v)", obj.GetKind(), obj.GetName(), key, annotations)
	}
	return v
}
