// Package controller reconciles StorageProtectionGroup resources against the
// replication engine in internal/replication.
//
// The reconciler makes no safety decisions of its own. It translates the custom
// resource into a replication.Group, asks the engine to act, and writes back
// what happened — including, verbatim, whatever the engine refused and why. A
// refusal is recorded as a condition and NOT retried: a failover that was
// refused because a previous one may still be in flight must not be retried by
// a controller in a backoff loop, it must be looked at by a person.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/piwi3910/truenas-csi/internal/replication"
	v1alpha1 "github.com/piwi3910/truenas-csi/internal/replication/v1alpha1"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// finalizer keeps the TrueNAS tasks from outliving the resource that made them.
const finalizer = "replication.truenas.io/storageprotectiongroup"

// resyncInterval re-reads the replication task state so status.taskState does
// not go stale between edits.
const resyncInterval = 2 * time.Minute

// Reconciler reconciles StorageProtectionGroup objects.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Manager is the replication engine. It owns every safety decision.
	Manager *replication.Manager
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.StorageProtectionGroup{}).
		Named("storageprotectiongroup").
		Complete(r)
}

// +kubebuilder:rbac:groups=replication.truenas.io,resources=storageprotectiongroups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=replication.truenas.io,resources=storageprotectiongroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=replication.truenas.io,resources=storageprotectiongroups/finalizers,verbs=update

// Reconcile brings one group in line with its spec.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var spg v1alpha1.StorageProtectionGroup
	if err := r.Get(ctx, req.NamespacedName, &spg); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	group, err := GroupFromSpec(&spg)
	if err != nil {
		// A malformed spec is not retryable: nothing about the cluster will fix
		// a volume handle that does not parse.
		return ctrl.Result{}, r.refuse(ctx, &spg, err)
	}

	if !spg.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &spg, group)
	}
	if !controllerutil.ContainsFinalizer(&spg, finalizer) {
		controllerutil.AddFinalizer(&spg, finalizer)
		if err := r.Update(ctx, &spg); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The store is the object's own status, so the engine's idempotency record
	// survives a controller restart.
	store := &statusStore{group: &spg}
	mgr := r.managerWithStore(store)

	if _, err := mgr.Create(ctx, group); err != nil {
		return ctrl.Result{}, r.retryable(ctx, &spg, fmt.Errorf("ensuring group: %w", err))
	}

	if err := r.applyAction(ctx, mgr, &spg, group); err != nil {
		if refused(err) {
			return ctrl.Result{}, r.refuse(ctx, &spg, err)
		}
		return ctrl.Result{}, r.retryable(ctx, &spg, err)
	}

	status, err := mgr.Status(ctx, group)
	if err != nil {
		return ctrl.Result{}, r.retryable(ctx, &spg, err)
	}
	writeStatus(&spg, group, status)
	spg.Status.ObservedGeneration = spg.Generation
	setCondition(&spg, v1alpha1.ConditionReady, metav1.ConditionTrue, "Reconciled", spg.Status.Message)
	setCondition(&spg, v1alpha1.ConditionRefused, metav1.ConditionFalse, "NoRefusal", "")
	if err := r.Status().Update(ctx, &spg); err != nil {
		return ctrl.Result{}, err
	}
	logger.V(1).Info("reconciled", "group", spg.Name, "phase", spg.Status.Phase)
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// applyAction performs spec.action once per generation.
//
// The generation check is what stops a resync re-running a failover: the action
// field is a request, and a request that has already been served is not served
// again just because the object was re-read.
func (r *Reconciler) applyAction(ctx context.Context, mgr *replication.Manager,
	spg *v1alpha1.StorageProtectionGroup, g replication.Group) error {

	action := spg.Spec.Action
	if action == v1alpha1.ActionNone {
		return nil
	}
	if spg.Status.LastAction == action && spg.Status.LastActionGeneration == spg.Generation {
		return nil
	}

	opts := replication.ActionOptions{Force: spg.Spec.Force}
	var err error
	switch action {
	case v1alpha1.ActionFailover:
		_, err = mgr.Failover(ctx, g, opts)
	case v1alpha1.ActionTestFailover:
		_, err = mgr.TestFailover(ctx, g)
	case v1alpha1.ActionStopTestFailover:
		_, err = mgr.StopTestFailover(ctx, g)
	case v1alpha1.ActionFailback:
		_, err = mgr.Failback(ctx, g, opts)
	case v1alpha1.ActionSuspend:
		_, err = mgr.Suspend(ctx, g)
	case v1alpha1.ActionResume:
		_, err = mgr.Resume(ctx, g)
	case v1alpha1.ActionCreateRemoteVolumes:
		for _, id := range g.Volumes {
			if _, err = mgr.CreateRemoteVolume(ctx, g, id); err != nil {
				break
			}
		}
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	if err != nil {
		return err
	}
	spg.Status.LastAction = action
	spg.Status.LastActionGeneration = spg.Generation
	return nil
}

// finalize tears the group's TrueNAS tasks down. It removes the finalizer only
// once teardown succeeded: a refused teardown — a target dataset the driver
// does not own — must keep the object, and its explanation, in the cluster.
func (r *Reconciler) finalize(ctx context.Context, spg *v1alpha1.StorageProtectionGroup,
	g replication.Group) (ctrl.Result, error) {

	if !controllerutil.ContainsFinalizer(spg, finalizer) {
		return ctrl.Result{}, nil
	}
	mgr := r.managerWithStore(&statusStore{group: spg})
	err := mgr.Delete(ctx, g, replication.DeleteOptions{
		RemoveTargetDatasets: spg.Spec.DeleteTargetDatasets,
	})
	if err != nil {
		if refused(err) {
			return ctrl.Result{}, r.refuse(ctx, spg, err)
		}
		return ctrl.Result{}, r.retryable(ctx, spg, err)
	}
	controllerutil.RemoveFinalizer(spg, finalizer)
	return ctrl.Result{}, r.Update(ctx, spg)
}

func (r *Reconciler) managerWithStore(s replication.Store) *replication.Manager {
	return r.Manager.WithStore(s)
}

// refused reports whether an error is a safety refusal rather than a transient
// failure. Refusals are recorded and left alone; only transient failures are
// retried.
func refused(err error) bool {
	return errors.Is(err, volume.ErrNotManaged) ||
		errors.Is(err, volume.ErrOutsideParent) ||
		errors.Is(err, volume.ErrMalformedID) ||
		errors.Is(err, replication.ErrDiverged) ||
		errors.Is(err, replication.ErrFailoverIncomplete) ||
		errors.Is(err, replication.ErrTestFailoverActive) ||
		errors.Is(err, replication.ErrWrongPhase) ||
		errors.Is(err, replication.ErrForbiddenCall)
}

func (r *Reconciler) refuse(ctx context.Context, spg *v1alpha1.StorageProtectionGroup, err error) error {
	spg.Status.Message = err.Error()
	setCondition(spg, v1alpha1.ConditionRefused, metav1.ConditionTrue, "Refused", err.Error())
	setCondition(spg, v1alpha1.ConditionReady, metav1.ConditionFalse, "Refused", err.Error())
	if uerr := r.Status().Update(ctx, spg); uerr != nil {
		return uerr
	}
	// Deliberately NOT returned as an error: requeueing a refusal would retry a
	// decision only a human should make.
	log.FromContext(ctx).Error(err, "refused", "group", spg.Name)
	return nil
}

func (r *Reconciler) retryable(ctx context.Context, spg *v1alpha1.StorageProtectionGroup, err error) error {
	spg.Status.Message = err.Error()
	setCondition(spg, v1alpha1.ConditionReady, metav1.ConditionFalse, "Error", err.Error())
	if uerr := r.Status().Update(ctx, spg); uerr != nil {
		return uerr
	}
	return err
}

func setCondition(spg *v1alpha1.StorageProtectionGroup, typ string,
	status metav1.ConditionStatus, reason, message string) {

	cond := metav1.Condition{
		Type: typ, Status: status, Reason: reason, Message: message,
		ObservedGeneration: spg.Generation,
		LastTransitionTime: metav1.Now(),
	}
	for i := range spg.Status.Conditions {
		if spg.Status.Conditions[i].Type != typ {
			continue
		}
		if spg.Status.Conditions[i].Status == status {
			cond.LastTransitionTime = spg.Status.Conditions[i].LastTransitionTime
		}
		spg.Status.Conditions[i] = cond
		return
	}
	spg.Status.Conditions = append(spg.Status.Conditions, cond)
}

// GroupFromSpec turns a custom resource into an engine group.
//
// Parsing happens here and nowhere else, so a handle that cannot be parsed is
// rejected before it reaches a middleware call.
func GroupFromSpec(spg *v1alpha1.StorageProtectionGroup) (replication.Group, error) {
	g := replication.Group{
		Name:                   spg.Namespace + "-" + spg.Name,
		SourceBackend:          spg.Spec.SourceBackend,
		TargetBackend:          spg.Spec.TargetBackend,
		SSHCredentialID:        spg.Spec.SSHCredentialID,
		SSHCredentialIDReverse: spg.Spec.ReverseSSHCredentialID,
		Schedule: replication.Cron{
			Minute: spg.Spec.Schedule.Minute,
			Hour:   spg.Spec.Schedule.Hour,
			DOM:    spg.Spec.Schedule.DayOfMonth,
			Month:  spg.Spec.Schedule.Month,
			DOW:    spg.Spec.Schedule.DayOfWeek,
		},
	}
	if spg.Namespace == "" {
		g.Name = spg.Name
	}
	if r := spg.Spec.Retention; r != nil {
		g.SnapshotRetention = replication.Retention{Value: r.Value, Unit: r.Unit}
	}
	if spg.Spec.SourceBackend == "" || spg.Spec.TargetBackend == "" {
		return g, fmt.Errorf("sourceBackend and targetBackend are both required")
	}
	if spg.Spec.SourceBackend == spg.Spec.TargetBackend {
		return g, fmt.Errorf("sourceBackend and targetBackend are both %q; "+
			"replicating an appliance to itself would overwrite the source",
			spg.Spec.SourceBackend)
	}
	for _, h := range spg.Spec.VolumeHandles {
		id, err := volume.ParseID(h)
		if err != nil {
			return g, fmt.Errorf("volumeHandles: %w", err)
		}
		g.Volumes = append(g.Volumes, id)
	}
	if len(g.Volumes) == 0 {
		return g, fmt.Errorf("volumeHandles is empty")
	}
	return g, nil
}

// writeStatus projects engine state onto the resource.
func writeStatus(spg *v1alpha1.StorageProtectionGroup, g replication.Group, s *replication.Status) {
	spg.Status.Phase = string(s.Phase)
	spg.Status.Message = s.Message
	spg.Status.ReplicationTaskID = s.TaskID
	spg.Status.TaskState = s.TaskState
	if s.TaskError != "" {
		spg.Status.Message = s.TaskError
	}
	spg.Status.Suspended = s.Suspended
	spg.Status.TestFailoverActive = s.TestFailover != nil
	spg.Status.SnapshotTaskIDs = s.SnapshotTaskIDs

	spg.Status.Volumes = spg.Status.Volumes[:0]
	for _, id := range g.Volumes {
		vs := v1alpha1.VolumeStatus{
			Handle:           id.String(),
			FailoverSnapshot: s.FailoverSnapshots[id.Name],
		}
		if s.TestFailover != nil {
			vs.TestFailoverDataset = s.TestFailover.Datasets[id.Name]
		}
		spg.Status.Volumes = append(spg.Status.Volumes, vs)
	}
}

// statusStore backs the engine's Store with the resource's status, so the
// record that makes failover idempotent lives where an operator can read it.
type statusStore struct {
	group *v1alpha1.StorageProtectionGroup
}

func (s *statusStore) Load(_ context.Context, _ string) (*replication.State, error) {
	st := &replication.State{
		Phase:             replication.Phase(s.group.Status.Phase),
		TaskID:            s.group.Status.ReplicationTaskID,
		SnapshotTaskIDs:   s.group.Status.SnapshotTaskIDs,
		Suspended:         s.group.Status.Suspended,
		Message:           s.group.Status.Message,
		FailoverSnapshots: map[string]string{},
	}
	var tf *replication.TestFailoverState
	for _, v := range s.group.Status.Volumes {
		id, err := volume.ParseID(v.Handle)
		if err != nil {
			return nil, err
		}
		if v.FailoverSnapshot != "" {
			st.FailoverSnapshots[id.Name] = v.FailoverSnapshot
		}
		if v.TestFailoverDataset != "" {
			if tf == nil {
				tf = &replication.TestFailoverState{
					Datasets:  map[string]string{},
					Snapshots: map[string]string{},
				}
			}
			tf.Datasets[id.Name] = v.TestFailoverDataset
		}
	}
	if tf != nil {
		tf.Root = parentOf(anyValue(tf.Datasets))
		st.TestFailover = tf
	}
	if len(st.FailoverSnapshots) == 0 {
		st.FailoverSnapshots = nil
	}
	return st, nil
}

func (s *statusStore) Save(_ context.Context, _ string, st *replication.State) error {
	s.group.Status.Phase = string(st.Phase)
	s.group.Status.ReplicationTaskID = st.TaskID
	s.group.Status.SnapshotTaskIDs = st.SnapshotTaskIDs
	s.group.Status.Suspended = st.Suspended
	s.group.Status.Message = st.Message
	s.group.Status.TestFailoverActive = st.TestFailover != nil

	byName := map[string]*v1alpha1.VolumeStatus{}
	for i := range s.group.Status.Volumes {
		id, err := volume.ParseID(s.group.Status.Volumes[i].Handle)
		if err != nil {
			continue
		}
		byName[id.Name] = &s.group.Status.Volumes[i]
	}
	for _, h := range s.group.Spec.VolumeHandles {
		id, err := volume.ParseID(h)
		if err != nil {
			continue
		}
		vs, ok := byName[id.Name]
		if !ok {
			s.group.Status.Volumes = append(s.group.Status.Volumes,
				v1alpha1.VolumeStatus{Handle: h})
			vs = &s.group.Status.Volumes[len(s.group.Status.Volumes)-1]
			byName[id.Name] = vs
		}
		vs.FailoverSnapshot = st.FailoverSnapshots[id.Name]
		vs.TestFailoverDataset = ""
		if st.TestFailover != nil {
			vs.TestFailoverDataset = st.TestFailover.Datasets[id.Name]
		}
	}
	return nil
}

func parentOf(dataset string) string {
	for i := len(dataset) - 1; i >= 0; i-- {
		if dataset[i] == '/' {
			return dataset[:i]
		}
	}
	return dataset
}

func anyValue(m map[string]string) string {
	for _, v := range m {
		return v
	}
	return ""
}
