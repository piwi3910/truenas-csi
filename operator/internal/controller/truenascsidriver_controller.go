// Package controller reconciles TrueNASCSIDriver resources.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	truenasv1alpha1 "github.com/piwi3910/truenas-csi/operator/api/v1alpha1"
	"github.com/piwi3910/truenas-csi/operator/internal/chartrender"
	"github.com/piwi3910/truenas-csi/operator/internal/credentials"
	"github.com/piwi3910/truenas-csi/operator/internal/health"
	"github.com/piwi3910/truenas-csi/operator/internal/rollout"
	"github.com/piwi3910/truenas-csi/operator/internal/skew"
	"github.com/piwi3910/truenas-csi/operator/internal/upgrade"
)

const (
	// FieldOwner is the server-side-apply field manager. Every object the
	// operator applies is owned under this name, so a field an administrator
	// edits by hand shows up as a conflict rather than being silently reverted
	// on the next reconcile.
	FieldOwner = "truenas-csi-operator"

	// RevisionAnnotation carries the operator's own hash of the node plugin pod
	// template. The DaemonSet is set to OnDelete, so the DaemonSet controller
	// will not roll pods itself; this annotation is how the operator tells an
	// existing pod apart from one created since the template last changed.
	RevisionAnnotation = "truenas-csi.watteel.com/revision"

	// ManagedByLabel marks every object the operator applies.
	ManagedByLabel = "truenas-csi.watteel.com/managed-by"

	// PreviousVersionAnnotation records the driver version the operator last
	// applied and rolled out. It is the input to the upgrade-path check, and it
	// lives in an annotation rather than in status because it must survive the
	// status subresource being cleared: the whole point of the value is to
	// remember which driver code actually ran on this cluster.
	//
	// It is written only once a reconcile has applied the release and has no
	// rollout or credential rotation left in flight. A refused or failed upgrade
	// leaves the previous value in place, so the next attempt is judged against
	// the version that really ran rather than one that never came up. An absent
	// annotation means a fresh install, which is never gated.
	PreviousVersionAnnotation = "truenas-csi.watteel.com/previous-version"

	// driverName is immutable and must match the chart's. It is written into
	// every PersistentVolume; changing it orphans them all.
	driverName = "csi.truenas.watteel.com"

	// requeueSettled is how often a healthy driver is re-checked. Backend
	// reachability comes from scraping the driver, so the status is only as
	// fresh as this interval.
	requeueSettled = 2 * time.Minute
	// requeueProgressing is the poll while a rollout or restart is in flight.
	requeueProgressing = 10 * time.Second
)

// Applier applies one rendered object to the cluster. It is an interface so the
// reconciler's decisions can be tested without an API server: the tests inject
// an applier that records objects instead of sending them.
type Applier interface {
	Apply(ctx context.Context, obj *unstructured.Unstructured) error
}

// BackendProbe reads per-backend health from a driver metrics endpoint.
type BackendProbe interface {
	Scrape(ctx context.Context, url string) (map[string]health.Backend, error)
}

// TrueNASCSIDriverReconciler reconciles a TrueNASCSIDriver.
//
// It renders the in-repo Helm chart and applies the result. Everything it does
// beyond that — refusing an incompatible upgrade, restarting pods when a key
// rotates, rolling node plugins one drained node at a time, and reporting what
// the driver says about its appliances — is lifecycle behaviour a chart cannot
// express, and is the reason this operator exists at all.
type TrueNASCSIDriverReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Chart is the loaded in-repo chart. There is exactly one copy of the
	// manifests in this repository and this is it.
	Chart *chart.Chart
	// KubeVersion is the cluster version the chart renders against.
	KubeVersion chartrender.KubeVersion
	// Applier applies rendered objects; nil means server-side apply.
	Applier Applier
	// Probe reads the driver's metrics; nil means no backend health reporting.
	Probe BackendProbe
	// Recorder publishes events on the TrueNASCSIDriver. A refusal that exists
	// only as a status condition is easy to miss; `kubectl describe` and every
	// event-scraping alert pipeline see an event. Nil disables events.
	Recorder events.EventRecorder
	// UpgradeTable declares which driver versions may be reached from which.
	// Nil means the table this operator ships; the field exists so the refusal
	// branches can be tested against a table with more than one release in it.
	UpgradeTable upgrade.Table
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=truenas.watteel.com,resources=truenascsidrivers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=truenas.watteel.com,resources=truenascsidrivers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=truenas.watteel.com,resources=truenascsidrivers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes;persistentvolumeclaims,verbs=get;list;watch
// The events.k8s.io group is what the CURRENT events API writes to; the core
// group is kept because a cluster's own aggregation and some tooling still read
// events there, and because dropping it would silently stop any code path still
// on the old recorder. Getting this wrong has no error: events simply never
// appear, and a refusal that exists only in a status condition is easy to miss.
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;daemonsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch;create;update;patch;delete;escalate;bind
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csidrivers;storageclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csinodes;volumeattachments,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=csistoragecapacities,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshotclasses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile brings one TrueNASCSIDriver to its desired state.
func (r *TrueNASCSIDriverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cr := &truenasv1alpha1.TrueNASCSIDriver{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if cr.DeletionTimestamp != nil {
		// Everything the operator applies carries an owner reference to this
		// resource, so the garbage collector removes it. There is deliberately
		// no finalizer: a finalizer that fails would strand the driver, and
		// nothing here needs to run before deletion.
		return ctrl.Result{}, nil
	}

	status := cr.Status.DeepCopy()
	status.ObservedGeneration = cr.Generation

	result, err := r.reconcile(ctx, cr, status)
	if serr := r.writeStatus(ctx, cr, status); serr != nil {
		logger.Error(serr, "write status")
		if err == nil {
			err = serr
		}
	}
	return result, err
}

func (r *TrueNASCSIDriverReconciler) reconcile(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	status *truenasv1alpha1.TrueNASCSIDriverStatus,
) (ctrl.Result, error) {
	ns := chartrender.Namespace(cr)

	creds, refs, err := r.resolveCredentials(ctx, cr, ns)
	if err != nil {
		r.degrade(status, cr.Generation, truenasv1alpha1.ReasonSecretMissing, err.Error())
		// A missing Secret is an operator error, not a transient fault. Requeue
		// slowly and let the Secret watch wake us the moment it appears.
		return ctrl.Result{RequeueAfter: requeueSettled}, nil
	}

	// Version skew is checked BEFORE anything is applied. A refused upgrade
	// must leave the previous release exactly as it was: half a release, with a
	// new driver image and sidecars it cannot talk to, is worse than no upgrade.
	driverVersion := cr.Spec.Image.Tag
	if driverVersion == "" && r.Chart != nil && r.Chart.Metadata != nil {
		driverVersion = r.Chart.Metadata.AppVersion
	}
	sidecars := skew.SidecarsFromValues(r.chartValues())
	if err := skew.Check(driverVersion, sidecars); err != nil {
		r.degrade(status, cr.Generation, truenasv1alpha1.ReasonVersionSkew, err.Error())
		r.event(cr, corev1.EventTypeWarning, truenasv1alpha1.ReasonVersionSkew, err.Error())
		return ctrl.Result{RequeueAfter: requeueSettled}, nil
	}

	// The upgrade path is a separate question from skew. A driver can be
	// perfectly compatible with the sidecars the chart pins and still be
	// unreachable from the release already installed, because the step between
	// them needs a migration only an intermediate release performs. It is
	// refused here, before anything is applied, for the same reason skew is: a
	// half-applied release is worse than an unchanged one.
	previousVersion := cr.Annotations[PreviousVersionAnnotation]
	if err := r.upgradeTable().Check(previousVersion, driverVersion); err != nil {
		r.degrade(status, cr.Generation, truenasv1alpha1.ReasonUpgradeNotSupported, err.Error())
		r.event(cr, corev1.EventTypeWarning, truenasv1alpha1.ReasonUpgradeNotSupported, err.Error())
		// status.AppliedVersion deliberately keeps whatever it already said:
		// nothing was applied, and naming the requested version there would
		// claim the cluster is running something it is not.
		return ctrl.Result{RequeueAfter: requeueSettled}, nil
	}

	objs, err := chartrender.Render(r.Chart, cr, creds, r.KubeVersion)
	if err != nil {
		r.degrade(status, cr.Generation, truenasv1alpha1.ReasonRenderFailed, err.Error())
		return ctrl.Result{RequeueAfter: requeueSettled}, nil
	}

	revision := podTemplateRevision(objs)
	decorate(objs, cr, ns, revision)

	if err := r.ensureNamespace(ctx, ns, cr); err != nil {
		r.degrade(status, cr.Generation, truenasv1alpha1.ReasonApplyFailed, err.Error())
		return ctrl.Result{}, err
	}
	applier := r.applier()
	for _, obj := range objs {
		if err := applier.Apply(ctx, obj); err != nil {
			msg := fmt.Sprintf("apply %s/%s: %v", obj.GetKind(), obj.GetName(), err)
			r.degrade(status, cr.Generation, truenasv1alpha1.ReasonApplyFailed, msg)
			return ctrl.Result{}, fmt.Errorf("%s", msg)
		}
	}

	status.AppliedVersion = driverVersion
	desiredFingerprint := credentials.Fingerprint(refs)

	// Credential rotation ordering, then the node rollout. Both stages are
	// driven by the operator because the DaemonSet is set to OnDelete.
	controllerUpdated, controllerReady, err := r.controllerState(ctx, ns)
	if err != nil {
		return ctrl.Result{}, err
	}
	rot := credentials.Plan(status.CredentialsRevision, desiredFingerprint, controllerUpdated, controllerReady)

	rolloutStatus := &truenasv1alpha1.RolloutStatus{DesiredRevision: revision}
	progressing := ""

	switch rot.Stage {
	case credentials.StageController, credentials.StageWaitController:
		// The controller Deployment already carries the new configuration
		// checksum from the render above, so the Deployment controller is
		// restarting it. Nodes wait: if the new key is wrong, the failure should
		// land on two controller pods, not on every node in the cluster.
		progressing = rot.Reason
		rolloutStatus.WaitingFor = rot.Reason
	default:
		plan, err := r.rollNodes(ctx, cr, ns, revision)
		if err != nil {
			return ctrl.Result{}, err
		}
		rolloutStatus.UpdatedNodes = int32(plan.Updated)
		rolloutStatus.TotalNodes = int32(plan.Total)
		rolloutStatus.WaitingFor = plan.WaitingFor
		rolloutStatus.CurrentNode = plan.CurrentNode
		if !plan.Done {
			progressing = "node plugin rollout in progress"
			if plan.WaitingFor != "" {
				progressing = plan.WaitingFor
			}
		}
		if plan.Done && rot.Changed {
			// Nodes are on the new credentials; the rotation is complete.
			status.CredentialsRevision = desiredFingerprint
		}
		if plan.Done && !rot.Changed {
			status.CredentialsRevision = desiredFingerprint
		}
	}
	status.Rollout = rolloutStatus

	backendStatus, unreachable := r.backendHealth(ctx, cr, ns)
	status.Backends = backendStatus

	if progressing == "" {
		// The release is applied and no rollout is still in flight. That, and
		// not backend reachability, is what makes this the version a future
		// upgrade must be judged from: an appliance that is unreachable says
		// nothing about which driver code is running on the cluster.
		if err := r.recordAppliedVersion(ctx, cr, previousVersion, driverVersion); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch {
	case progressing != "":
		reason := truenasv1alpha1.ReasonRollingOut
		if rot.Changed {
			reason = truenasv1alpha1.ReasonRotatingCredentials
		}
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionProgressing, metav1.ConditionTrue, reason, progressing)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionDegraded, metav1.ConditionFalse, reason, progressing)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionReady, metav1.ConditionFalse, reason, progressing)
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	case len(unreachable) > 0:
		msg := fmt.Sprintf("backend(s) unreachable: %v", unreachable)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionProgressing, metav1.ConditionFalse, truenasv1alpha1.ReasonBackendUnreachable, msg)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionDegraded, metav1.ConditionTrue, truenasv1alpha1.ReasonBackendUnreachable, msg)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionReady, metav1.ConditionFalse, truenasv1alpha1.ReasonBackendUnreachable, msg)
		return ctrl.Result{RequeueAfter: requeueProgressing}, nil
	default:
		msg := fmt.Sprintf("driver %s applied and healthy", status.AppliedVersion)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionProgressing, metav1.ConditionFalse, truenasv1alpha1.ReasonReconciled, msg)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionDegraded, metav1.ConditionFalse, truenasv1alpha1.ReasonReconciled, msg)
		setCondition(status, cr.Generation, truenasv1alpha1.ConditionReady, metav1.ConditionTrue, truenasv1alpha1.ReasonReconciled, msg)
		return ctrl.Result{RequeueAfter: requeueSettled}, nil
	}
}

// recordAppliedVersion stamps the version that has just finished rolling out
// onto the CR, so a later reconcile knows where an upgrade would start from.
//
// It is a no-op when the value has not changed: a metadata write wakes the
// resource's own watch, and rewriting the same annotation every reconcile would
// be a self-sustaining loop.
func (r *TrueNASCSIDriverReconciler) recordAppliedVersion(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	previous, applied string,
) error {
	if applied == "" || previous == applied {
		return nil
	}
	patch := client.MergeFrom(cr.DeepCopy())
	annotations := cr.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[PreviousVersionAnnotation] = applied
	cr.SetAnnotations(annotations)
	if err := r.Patch(ctx, cr, patch); err != nil {
		return fmt.Errorf("record applied version %s: %w", applied, err)
	}
	if previous != "" {
		r.event(cr, corev1.EventTypeNormal, truenasv1alpha1.ReasonUpgraded,
			fmt.Sprintf("driver upgraded from %s to %s", previous, applied))
	}
	return nil
}

// upgradeTable returns the declared upgrade paths, defaulting to the shipped
// table.
func (r *TrueNASCSIDriverReconciler) upgradeTable() upgrade.Table {
	if r.UpgradeTable != nil {
		return r.UpgradeTable
	}
	return upgrade.Shipped
}

// event publishes one event on the resource, when a recorder is configured.
func (r *TrueNASCSIDriverReconciler) event(cr *truenasv1alpha1.TrueNASCSIDriver, eventType, reason, message string) {
	if r.Recorder == nil {
		return
	}
	// The new events API needs an `action`: what the reporting controller did,
	// distinct from `reason` (why). The reason is already a verb-ish constant
	// like UpgradeNotSupported, so the action is the operation it happened
	// during, which is what makes an event greppable by what the operator was
	// attempting.
	r.Recorder.Eventf(cr, nil, eventType, reason, "Reconcile", "%s", message)
}

// chartValues returns the chart's own default values, which is where the CSI
// sidecar pins live.
func (r *TrueNASCSIDriverReconciler) chartValues() map[string]any {
	if r.Chart == nil {
		return map[string]any{}
	}
	return r.Chart.Values
}

func (r *TrueNASCSIDriverReconciler) applier() Applier {
	if r.Applier != nil {
		return r.Applier
	}
	return &serverSideApplier{client: r.Client}
}

func (r *TrueNASCSIDriverReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// resolveCredentials reads the referenced Secrets. The key material stays in
// memory: it goes into the rendered Secret and nowhere else.
func (r *TrueNASCSIDriverReconciler) resolveCredentials(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	ns string,
) (chartrender.Credentials, []credentials.Ref, error) {
	creds := chartrender.Credentials{APIKeys: map[string]string{}, CACerts: map[string]string{}}
	var refs []credentials.Ref
	cache := map[string]*corev1.Secret{}

	read := func(ref truenasv1alpha1.SecretKeyReference) (string, string, error) {
		s, ok := cache[ref.Name]
		if !ok {
			s = &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, s); err != nil {
				if apierrors.IsNotFound(err) {
					return "", "", fmt.Errorf("secret %s/%s does not exist", ns, ref.Name)
				}
				return "", "", err
			}
			cache[ref.Name] = s
		}
		v, ok := s.Data[ref.SecretKey()]
		if !ok {
			return "", "", fmt.Errorf("secret %s/%s has no key %q", ns, ref.Name, ref.SecretKey())
		}
		return string(v), s.ResourceVersion, nil
	}

	for _, b := range cr.Spec.Backends {
		key, rv, err := read(b.APIKeySecretRef)
		if err != nil {
			return creds, nil, fmt.Errorf("backend %q: %w", b.Name, err)
		}
		if err := credentials.Validate(b.Name, key); err != nil {
			return creds, nil, err
		}
		creds.APIKeys[b.Name] = key
		refs = append(refs, credentials.Ref{
			Secret: b.APIKeySecretRef.Name, Key: b.APIKeySecretRef.SecretKey(), ResourceVersion: rv,
		})
		if b.CACertSecretRef != nil {
			ca, carv, err := read(*b.CACertSecretRef)
			if err != nil {
				return creds, nil, fmt.Errorf("backend %q CA certificate: %w", b.Name, err)
			}
			creds.CACerts[b.Name] = ca
			refs = append(refs, credentials.Ref{
				Secret: b.CACertSecretRef.Name, Key: b.CACertSecretRef.SecretKey(), ResourceVersion: carv,
			})
		}
	}
	return creds, refs, nil
}

func (r *TrueNASCSIDriverReconciler) ensureNamespace(ctx context.Context, ns string, cr *truenasv1alpha1.TrueNASCSIDriver) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   ns,
			"labels": map[string]any{ManagedByLabel: cr.Name},
		},
	}}
	setOwner(obj, cr)
	return r.applier().Apply(ctx, obj)
}

// controllerState reports whether the controller Deployment is fully rolled to
// its current pod template and healthy.
func (r *TrueNASCSIDriverReconciler) controllerState(ctx context.Context, ns string) (updated, ready bool, err error) {
	list := &appsv1.DeploymentList{}
	if err := r.List(ctx, list, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil {
		return false, false, err
	}
	if len(list.Items) == 0 {
		// Nothing deployed yet: the very first reconcile. Treat it as "not
		// updated" so a first install does not look like a finished rotation.
		return false, false, nil
	}
	for _, d := range list.Items {
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		if d.Status.UpdatedReplicas < desired || d.Status.ObservedGeneration < d.Generation {
			return false, false, nil
		}
		if d.Status.ReadyReplicas < desired {
			return true, false, nil
		}
	}
	return true, true, nil
}

// nodeDaemonSetState reads how many nodes the node DaemonSet targets, and
// whether that number has been reported at all.
//
// The distinction it carries is the difference between a rollout that has not
// started and one that has nothing to do. A DaemonSet whose pods have not been
// created yet and a DaemonSet excluded from every node by a nodeSelector both
// show zero pods; only status.desiredNumberScheduled, and only once the
// DaemonSet controller has observed the current generation, tells them apart.
//
// A DaemonSet that is not there yet — the very first reconcile, before the
// apply has been read back — is reported as unobserved rather than as zero
// nodes, for the same reason controllerState treats a missing Deployment as
// "not updated": an object nobody has seen says nothing about the cluster.
func (r *TrueNASCSIDriverReconciler) nodeDaemonSetState(ctx context.Context, ns string) (rollout.DaemonSetState, error) {
	list := &appsv1.DaemonSetList{}
	if err := r.List(ctx, list, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/component": "node"}); err != nil {
		return rollout.DaemonSetState{}, err
	}
	state := rollout.DaemonSetState{Observed: len(list.Items) > 0}
	for i := range list.Items {
		d := &list.Items[i]
		if d.Status.ObservedGeneration < d.Generation {
			// The status still describes the previous pod template. Its
			// desiredNumberScheduled may be right, but nothing here may assume
			// it is.
			state.Observed = false
		}
		state.Desired += int(d.Status.DesiredNumberScheduled)
	}
	return state, nil
}

// nodeRolloutResult is what rollNodes learned.
type nodeRolloutResult struct {
	Updated     int
	Total       int
	Done        bool
	WaitingFor  string
	CurrentNode string
}

// rollNodes performs at most one node's worth of rollout per reconcile.
func (r *TrueNASCSIDriverReconciler) rollNodes(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	ns, revision string,
) (nodeRolloutResult, error) {
	logger := log.FromContext(ctx)

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/component": "node"}); err != nil {
		return nodeRolloutResult{}, err
	}

	ourPVCs, err := r.driverPVCs(ctx)
	if err != nil {
		return nodeRolloutResult{}, err
	}
	workloads := &corev1.PodList{}
	if err := r.List(ctx, workloads); err != nil {
		return nodeRolloutResult{}, err
	}
	busy := rollout.BusyNodes(workloads.Items, ourPVCs)

	states := make([]rollout.NodeState, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == "" {
			continue
		}
		reason, isBusy := busy[p.Spec.NodeName]
		states = append(states, rollout.NodeState{
			Name:       p.Spec.NodeName,
			PodName:    p.Name,
			UpToDate:   p.Annotations[RevisionAnnotation] == revision,
			Ready:      podReady(p),
			Busy:       isBusy,
			BusyReason: reason,
		})
	}

	ds, err := r.nodeDaemonSetState(ctx, ns)
	if err != nil {
		return nodeRolloutResult{}, err
	}

	plan := rollout.Next(states, ds)
	res := nodeRolloutResult{
		Updated:     plan.Updated,
		Total:       plan.Total,
		Done:        plan.Done,
		WaitingFor:  plan.WaitingFor,
		CurrentNode: rollout.CurrentNode(states, plan),
	}
	for _, podName := range plan.Roll {
		logger.Info("rolling node plugin", "pod", podName, "node", res.CurrentNode, "revision", revision)
		pod := &corev1.Pod{}
		pod.Namespace = ns
		pod.Name = podName
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return res, err
		}
	}
	return res, nil
}

// driverPVCs returns the PVCs bound to PersistentVolumes this driver owns.
func (r *TrueNASCSIDriverReconciler) driverPVCs(ctx context.Context) (map[string]bool, error) {
	pvs := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, pvs); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driverName {
			continue
		}
		if pv.Spec.ClaimRef == nil {
			continue
		}
		out[rollout.DriverPVCKey(pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)] = true
	}
	return out, nil
}

// backendHealth asks the driver what it thinks of its appliances.
func (r *TrueNASCSIDriverReconciler) backendHealth(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	ns string,
) ([]truenasv1alpha1.BackendStatus, []string) {
	now := metav1.NewTime(r.now())
	statuses := make([]truenasv1alpha1.BackendStatus, 0, len(cr.Spec.Backends))
	unknown := func(msg string) []truenasv1alpha1.BackendStatus {
		for _, b := range cr.Spec.Backends {
			statuses = append(statuses, truenasv1alpha1.BackendStatus{
				Name: b.Name, Reachable: "Unknown", Message: msg, LastProbeTime: &now,
			})
		}
		return statuses
	}

	if r.Probe == nil {
		return unknown("backend health reporting is disabled"), nil
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(ns),
		client.MatchingLabels{"app.kubernetes.io/component": "controller"}); err != nil {
		return unknown(fmt.Sprintf("list controller pods: %v", err)), nil
	}
	var target string
	for i := range pods.Items {
		p := &pods.Items[i]
		if podReady(p) && p.Status.PodIP != "" {
			target = fmt.Sprintf("http://%s:9090/metrics", p.Status.PodIP)
			break
		}
	}
	if target == "" {
		return unknown("no ready controller pod to scrape"), nil
	}

	observed, err := r.Probe.Scrape(ctx, target)
	if err != nil {
		return unknown(fmt.Sprintf("scrape driver metrics: %v", err)), nil
	}

	var unreachable []string
	for _, b := range cr.Spec.Backends {
		bs := truenasv1alpha1.BackendStatus{Name: b.Name, LastProbeTime: &now}
		h, ok := observed[b.Name]
		switch {
		case !ok:
			bs.Reachable = "Unknown"
			bs.Message = "the driver has not reported this backend yet"
		case h.Up:
			bs.Reachable = "True"
			orphans := h.Orphans
			bs.OrphanedVolumes = &orphans
		default:
			bs.Reachable = "False"
			bs.Message = "the driver reports no websocket connection to this appliance"
			orphans := h.Orphans
			bs.OrphanedVolumes = &orphans
			unreachable = append(unreachable, b.Name)
		}
		statuses = append(statuses, bs)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	sort.Strings(unreachable)
	return statuses, unreachable
}

func (r *TrueNASCSIDriverReconciler) writeStatus(
	ctx context.Context,
	cr *truenasv1alpha1.TrueNASCSIDriver,
	status *truenasv1alpha1.TrueNASCSIDriverStatus,
) error {
	if equalStatus(&cr.Status, status) {
		return nil
	}
	cr.Status = *status
	return r.Status().Update(ctx, cr)
}

func (r *TrueNASCSIDriverReconciler) degrade(
	status *truenasv1alpha1.TrueNASCSIDriverStatus,
	generation int64,
	reason, message string,
) {
	setCondition(status, generation, truenasv1alpha1.ConditionDegraded, metav1.ConditionTrue, reason, message)
	setCondition(status, generation, truenasv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	setCondition(status, generation, truenasv1alpha1.ConditionProgressing, metav1.ConditionFalse, reason, message)
}

func setCondition(
	status *truenasv1alpha1.TrueNASCSIDriverStatus,
	generation int64,
	condType string,
	state metav1.ConditionStatus,
	reason, message string,
) {
	if len(message) > 1024 {
		message = message[:1024]
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             state,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

func equalStatus(a, b *truenasv1alpha1.TrueNASCSIDriverStatus) bool {
	ja, err1 := json.Marshal(stripTransitionTimes(a))
	jb, err2 := json.Marshal(stripTransitionTimes(b))
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ja) == string(jb)
}

// stripTransitionTimes removes the timestamps that change on every write, so a
// status that says the same thing is not rewritten in a loop.
func stripTransitionTimes(s *truenasv1alpha1.TrueNASCSIDriverStatus) *truenasv1alpha1.TrueNASCSIDriverStatus {
	out := s.DeepCopy()
	for i := range out.Conditions {
		out.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	for i := range out.Backends {
		out.Backends[i].LastProbeTime = nil
	}
	return out
}

// decorate stamps ownership, management labels and the rollout revision onto
// every rendered object.
func decorate(objs []*unstructured.Unstructured, cr *truenasv1alpha1.TrueNASCSIDriver, ns, revision string) {
	for _, obj := range objs {
		if obj.GetNamespace() == "" && namespaced(obj.GetKind()) {
			obj.SetNamespace(ns)
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ManagedByLabel] = cr.Name
		obj.SetLabels(labels)
		setOwner(obj, cr)

		if obj.GetKind() == "DaemonSet" {
			annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[RevisionAnnotation] = revision
			_ = unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations")
		}
	}
}

// podTemplateRevision hashes the node DaemonSet's pod template. It is the
// operator's own notion of "which version of the plugin is this pod", because
// the DaemonSet is set to OnDelete and therefore does not roll pods itself.
func podTemplateRevision(objs []*unstructured.Unstructured) string {
	h := sha256.New()
	for _, obj := range objs {
		if obj.GetKind() != "DaemonSet" {
			continue
		}
		tmpl, found, err := unstructured.NestedMap(obj.Object, "spec", "template")
		if err != nil || !found {
			continue
		}
		raw, err := json.Marshal(tmpl)
		if err != nil {
			continue
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func setOwner(obj *unstructured.Unstructured, cr *truenasv1alpha1.TrueNASCSIDriver) {
	// The owner is cluster-scoped, so it can own both the cluster-scoped
	// objects (CSIDriver, ClusterRole, StorageClass) and the namespaced ones.
	obj.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: truenasv1alpha1.GroupVersion.String(),
		Kind:       "TrueNASCSIDriver",
		Name:       cr.Name,
		UID:        cr.UID,
		Controller: ptr(true),
	}})
}

func ptr[T any](v T) *T { return &v }

// namespaced reports whether a kind the chart renders is namespaced. The chart
// renders a fixed, known set of kinds, so a hard-coded list is honest here and
// avoids a discovery round trip on every reconcile.
func namespaced(kind string) bool {
	switch kind {
	case "CSIDriver", "StorageClass", "VolumeSnapshotClass", "ClusterRole", "ClusterRoleBinding", "Namespace", "CustomResourceDefinition", "PriorityClass":
		return false
	default:
		return true
	}
}

func podReady(p *corev1.Pod) bool {
	if p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// serverSideApplier applies objects with server-side apply.
type serverSideApplier struct {
	client client.Client
}

func (a *serverSideApplier) Apply(ctx context.Context, obj *unstructured.Unstructured) error {
	if a.client == nil {
		return errors.New("no client configured")
	}
	// staticcheck flags client.Apply as deprecated in favour of
	// client.Client.Apply(). That replacement takes a runtime.ApplyConfiguration
	// -- a TYPED apply configuration, which must implement IsApplyConfiguration()
	// -- and *unstructured.Unstructured does not implement it. This applier
	// exists precisely to apply arbitrary rendered chart manifests, for which no
	// typed configuration exists, so the suggested API cannot express this call.
	// Patch with client.Apply remains the way to server-side apply an
	// unstructured object; revisit when controller-runtime offers an
	// unstructured Apply.
	//nolint:staticcheck // no unstructured form of the replacement API exists
	return a.client.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// SetupWithManager wires the reconciler up.
func (r *TrueNASCSIDriverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&truenasv1alpha1.TrueNASCSIDriver{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.DaemonSet{}).
		// The credential Secret is created by the administrator, not by the
		// operator, so it carries no owner reference and Owns() would never see
		// it. Without this watch an API key rotation would go unnoticed until
		// the next periodic resync, and the driver would keep presenting a key
		// the appliance has already stopped accepting.
		Watches(&corev1.Secret{}, r.secretHandler()).
		Complete(r)
}

// secretHandler maps a Secret to every TrueNASCSIDriver that references it.
func (r *TrueNASCSIDriverReconciler) secretHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		list := &truenasv1alpha1.TrueNASCSIDriverList{}
		if err := r.List(ctx, list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			cr := &list.Items[i]
			if chartrender.Namespace(cr) != obj.GetNamespace() {
				continue
			}
			for _, b := range cr.Spec.Backends {
				if b.APIKeySecretRef.Name == obj.GetName() ||
					(b.CACertSecretRef != nil && b.CACertSecretRef.Name == obj.GetName()) {
					out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
					break
				}
			}
		}
		return out
	})
}
