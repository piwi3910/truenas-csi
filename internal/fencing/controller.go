// Package fencing force-deletes a pod that is stuck on a failed node, but only
// after the APPLIANCE has confirmed that the node can no longer reach the
// pod's volumes.
//
// # Why the order matters
//
// Kubernetes will not delete a pod whose node has stopped answering: the pod
// object stays Terminating forever, and a StatefulSet or a
// ReadWriteOnce-attached Deployment cannot start its replacement while it
// exists. The standard workaround — `kubectl delete pod --force` — is a data
// corruption bug waiting for the right timing: the old node may be alive and
// mid-write, merely partitioned from the API server, and the replacement pod
// will happily mount the same volume and write to it too.
//
// The fix, which is Dell's design for CSM Resiliency and is correct, is to make
// the deletion safe rather than to make it faster:
//
//  1. the pod is not ready, and its node is tainted unreachable or is otherwise
//     failed;
//  2. the pod's UID is re-read, so a pod recreated under the same name is never
//     the one killed;
//  3. the APPLIANCE is asked whether that node still holds a session or a lease.
//     Connected, an unknown, or any error at all aborts;
//  4. appliance-side access is REVOKED for every volume the pod holds. If any
//     single revoke fails the whole cleanup aborts — a partially fenced node is
//     still a writer;
//  5. only then: taint the node, delete the VolumeAttachments, and force-delete
//     the pod.
//
// Step 5 comes last for one reason. The pod object is the interlock: while it
// exists the scheduler will not place its replacement. Deleting it before
// access is provably gone is precisely the race the force-delete workaround
// loses.
//
// # Opt-in, and off by default
//
// Only pods carrying the opt-in label are ever considered, and the chart ships
// the controller disabled. Force-deleting pods is destructive and irreversible;
// an operator asks for it per workload, the way Dell does.
package fencing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"

	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/podmon"
	"github.com/piwi3910/truenas-csi/internal/volume"
)

// DefaultLabelKey is the label a pod must carry to be eligible for fencing, and
// DefaultLabelValue is the value it must have.
//
// Opt-in, never opt-out. A cluster-wide fencer that force-deletes anything it
// finds stuck is a footgun; the workloads that actually need this are the ones
// whose owner knows they hold a single-writer volume.
const (
	DefaultLabelKey   = driver.DriverName + "/fence"
	DefaultLabelValue = "true"
)

// TaintKey marks a node this controller has fenced.
//
// It is applied with NoSchedule so nothing new lands on a node whose storage
// access has just been revoked. It is deliberately NOT NoExecute: the pods
// already there are being evicted by the node lifecycle controller anyway, and
// a second eviction mechanism racing it adds nothing. An operator removes the
// taint when the node comes back and has been checked.
const TaintKey = driver.DriverName + "/fenced"

// DefaultInterval is how often the controller sweeps.
//
// Node readiness and the unreachable taint move on a timescale of tens of
// seconds (the node monitor grace period is 40s by default and the eviction
// timeout 300s), so polling every 15s adds no meaningful delay to a process
// that is already minutes long — and one LIST filtered server-side by the
// opt-in label is far cheaper than watching every pod in the cluster.
const DefaultInterval = 15 * time.Second

// Event reasons. They are read by operators during an incident, so they say
// what happened rather than what was intended.
const (
	// EventFenced: appliance access was revoked and the pod force-deleted.
	EventFenced = "VolumeAccessFenced"
	// EventAborted: the cleanup stopped before deleting anything.
	EventAborted = "FencingAborted"
	// EventRevokeFailed: an appliance-side revoke failed, so nothing was deleted.
	EventRevokeFailed = "FencingRevokeFailed"
)

// ConnectivityChecker answers, from the appliance, whether a node still holds
// the named volumes. *podmon.Connectivity implements it.
//
// It is an interface here so that this package cannot accidentally be wired to
// the node-side self-check: anything satisfying it must produce podmon.Reports,
// which only the appliance-backed service does.
type ConnectivityChecker interface {
	Check(ctx context.Context, nodeID string, ids []volume.ID) []podmon.Report
}

// VolumeFencer revokes appliance-side access for one volume and one node. It is
// the same fence ControllerUnpublishVolume performs.
type VolumeFencer interface {
	Fence(ctx context.Context, id volume.ID, nodeID string) error
}

// Controller watches opted-in pods and fences the ones stuck on a failed node.
type Controller struct {
	// Client talks to the API server.
	Client kubernetes.Interface
	// Connectivity is the appliance-side question. Never a node-side probe.
	Connectivity ConnectivityChecker
	// Fencer revokes appliance-side access.
	Fencer VolumeFencer
	// Events records what happened on the pod, so `kubectl describe pod` shows
	// both a fence and, more often, why one was refused.
	Events record.EventRecorder

	// LabelKey and LabelValue are the opt-in label. Empty uses the defaults.
	LabelKey   string
	LabelValue string
	// Interval is the sweep period. Zero uses DefaultInterval.
	Interval time.Duration
	// DriverName restricts the volumes this controller will touch to its own.
	DriverName string
}

// New builds a controller with the defaults filled in.
func New(client kubernetes.Interface, conn ConnectivityChecker, fencer VolumeFencer,
	events record.EventRecorder) *Controller {
	return &Controller{
		Client:       client,
		Connectivity: conn,
		Fencer:       fencer,
		Events:       events,
		LabelKey:     DefaultLabelKey,
		LabelValue:   DefaultLabelValue,
		Interval:     DefaultInterval,
		DriverName:   driver.DriverName,
	}
}

// Run sweeps until ctx is cancelled.
//
// It must run under leader election: two replicas racing to fence the same pod
// would each revoke access the other is relying on, and both would report
// success. See cmd/truenas-csi.
func (c *Controller) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	obs.Logger(ctx).Info("pod fencing enabled",
		"label", c.labelSelector(), "interval", interval.String())
	for {
		c.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep examines every opted-in pod once.
func (c *Controller) Sweep(ctx context.Context) {
	pods, err := c.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx,
		metav1.ListOptions{LabelSelector: c.labelSelector()})
	if err != nil {
		obs.Logger(ctx).Warn("fencing sweep could not list pods", "error", obs.Redact(err.Error()))
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := c.handle(ctx, pod); err != nil {
			obs.Logger(ctx).Warn("fencing did not complete for this pod",
				"pod", pod.Namespace+"/"+pod.Name, "error", obs.Redact(err.Error()))
		}
	}
}

// handle runs the protocol for one pod. Every early return is a deliberate
// refusal: doing nothing is always the safe outcome here.
func (c *Controller) handle(ctx context.Context, pod *corev1.Pod) error {
	if pod.Labels[c.labelKey()] != c.labelValue() {
		return nil
	}
	if pod.Spec.NodeName == "" || podIsReady(pod) {
		return nil
	}

	node, err := c.Client.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		// A node that cannot be read is not a node that has been proven dead.
		return fmt.Errorf("reading node %s: %w", pod.Spec.NodeName, err)
	}
	failureReason, failed := nodeHasFailed(node)
	if !failed {
		return nil
	}

	// Re-read the pod. Between the LIST and here the pod may have been deleted
	// and recreated under the same name by its controller — a new pod, a new
	// UID, quite possibly already running elsewhere. Killing that one would be
	// an outage this controller caused rather than one it repaired.
	live, err := c.Client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("re-reading pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if live.UID != pod.UID {
		obs.Logger(ctx).Info("pod was recreated under the same name; leaving it alone",
			"pod", pod.Namespace+"/"+pod.Name, "was", string(pod.UID), "now", string(live.UID))
		return nil
	}
	pod = live

	ids, err := c.volumesOf(ctx, pod)
	if err != nil {
		c.abort(pod, fmt.Sprintf("the volumes this pod holds could not be resolved: %v", err))
		return err
	}
	if len(ids) == 0 {
		// No volume of this driver's: there is nothing here to fence, and
		// force-deleting a pod for somebody else's storage is not this
		// controller's business.
		return nil
	}

	// The appliance's answer. Any veto, any unknown, any error stops here.
	reports := c.Connectivity.Check(ctx, pod.Spec.NodeName, ids)
	if safe, reason := podmon.SafeToFence(reports); !safe {
		c.abort(pod, "not fencing: "+reason)
		return nil
	}

	// Revoke first, and all-or-nothing. A pod deleted after a partial revoke
	// would be replaced while the old node still reaches some of its volumes,
	// which is the exact corruption this controller exists to prevent.
	for _, id := range ids {
		if err := c.Fencer.Fence(ctx, id, pod.Spec.NodeName); err != nil {
			c.event(pod, corev1.EventTypeWarning, EventRevokeFailed, fmt.Sprintf(
				"aborting: appliance access for volume %s could not be revoked (%v); "+
					"the pod has NOT been deleted, because a partially fenced node is still a writer",
				id, err))
			return fmt.Errorf("revoking %s on %s: %w", id, pod.Spec.NodeName, err)
		}
	}

	// Access is gone. Now, and only now, the Kubernetes objects.
	if err := c.taintNode(ctx, node); err != nil {
		// The revokes already happened, so the dangerous window is closed. The
		// taint is defence in depth; failing it is worth a log, not an abort.
		obs.Logger(ctx).Warn("could not taint the fenced node",
			"node", node.Name, "error", obs.Redact(err.Error()))
	}
	if err := c.deleteAttachments(ctx, pod.Spec.NodeName, ids); err != nil {
		obs.Logger(ctx).Warn("could not delete every VolumeAttachment for the fenced node",
			"node", node.Name, "error", obs.Redact(err.Error()))
	}
	if err := c.forceDelete(ctx, pod); err != nil {
		return fmt.Errorf("force-deleting pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	c.event(pod, corev1.EventTypeNormal, EventFenced, fmt.Sprintf(
		"revoked appliance access to %d volume(s) for node %s and force-deleted this pod (%s); "+
			"the appliance reported no session or lease held by that node",
		len(ids), pod.Spec.NodeName, failureReason))
	obs.Logger(ctx).Info("fenced a pod stuck on a failed node",
		"pod", pod.Namespace+"/"+pod.Name, "node", pod.Spec.NodeName, "volumes", len(ids))
	return nil
}

// volumesOf resolves the pod's PersistentVolumeClaims to this driver's volume
// handles. A claim belonging to another driver is skipped; a claim that cannot
// be read is an error, because an unresolved volume is one this controller
// would fail to fence while believing it had.
func (c *Controller) volumesOf(ctx context.Context, pod *corev1.Pod) ([]volume.ID, error) {
	var ids []volume.ID
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := c.Client.CoreV1().PersistentVolumeClaims(pod.Namespace).
			Get(ctx, vol.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading claim %s: %w", vol.PersistentVolumeClaim.ClaimName, err)
		}
		if pvc.Spec.VolumeName == "" {
			continue
		}
		pv, err := c.Client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading volume %s: %w", pvc.Spec.VolumeName, err)
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != c.driverName() {
			continue
		}
		id, err := volume.ParseID(pv.Spec.CSI.VolumeHandle)
		if err != nil {
			return nil, fmt.Errorf("volume handle on %s: %w", pv.Name, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// taintNode adds TaintKey with NoSchedule, if it is not already there.
//
// It patches rather than updates so the controller needs only `patch` on nodes:
// a driver that can rewrite an arbitrary Node object is a much larger blast
// radius than one that can add a taint.
func (c *Controller) taintNode(ctx context.Context, node *corev1.Node) error {
	for _, t := range node.Spec.Taints {
		if t.Key == TaintKey {
			return nil
		}
	}
	taints := append(append([]corev1.Taint{}, node.Spec.Taints...), corev1.Taint{
		Key:       TaintKey,
		Value:     "true",
		Effect:    corev1.TaintEffectNoSchedule,
		TimeAdded: &metav1.Time{Time: time.Now()},
	})
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"taints": taints}})
	if err != nil {
		return err
	}
	_, err = c.Client.CoreV1().Nodes().Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// deleteAttachments removes the VolumeAttachments binding these volumes to the
// fenced node, so the attacher does not keep trying to detach through a node
// that will never answer, and the replacement pod's attachment is not queued
// behind one that cannot complete.
func (c *Controller) deleteAttachments(ctx context.Context, nodeName string, ids []volume.ID) error {
	list, err := c.Client.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	handles := map[string]bool{}
	for _, id := range ids {
		handles[id.String()] = true
	}
	var errs []error
	for i := range list.Items {
		va := &list.Items[i]
		if va.Spec.NodeName != nodeName || va.Spec.Attacher != c.driverName() {
			continue
		}
		if !attachmentMatches(ctx, c, va, handles) {
			continue
		}
		if err := c.Client.StorageV1().VolumeAttachments().Delete(ctx, va.Name,
			metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// attachmentMatches reports whether a VolumeAttachment refers to one of the
// fenced volumes. An attachment naming a PersistentVolume is resolved through
// it; an inline source names the handle directly.
func attachmentMatches(ctx context.Context, c *Controller, va *storagev1.VolumeAttachment,
	handles map[string]bool) bool {
	if src := va.Spec.Source.InlineVolumeSpec; src != nil && src.CSI != nil {
		return handles[src.CSI.VolumeHandle]
	}
	if va.Spec.Source.PersistentVolumeName == nil {
		return false
	}
	pv, err := c.Client.CoreV1().PersistentVolumes().Get(ctx, *va.Spec.Source.PersistentVolumeName,
		metav1.GetOptions{})
	if err != nil || pv.Spec.CSI == nil {
		return false
	}
	return handles[pv.Spec.CSI.VolumeHandle]
}

// forceDelete removes the pod with no grace period, guarded by its UID.
//
// The UID precondition is not belt and braces: without it, a pod recreated
// between the check and this call would be deleted by name, killing a healthy
// replacement. With it, the API server refuses the delete instead.
func (c *Controller) forceDelete(ctx context.Context, pod *corev1.Pod) error {
	grace := int64(0)
	uid := pod.UID
	err := c.Client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		Preconditions:      &metav1.Preconditions{UID: &uid},
	})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		// Gone already, or no longer the pod we checked. Either way there is
		// nothing here to delete.
		return nil
	}
	return err
}

// abort records why no fencing happened. It is a Warning because somebody
// waiting for a stuck pod needs to find this in `kubectl describe`.
func (c *Controller) abort(pod *corev1.Pod, msg string) {
	c.event(pod, corev1.EventTypeWarning, EventAborted, msg)
}

func (c *Controller) event(pod *corev1.Pod, eventType, reason, msg string) {
	if c.Events == nil {
		return
	}
	c.Events.Event(pod, eventType, reason, msg)
}

func (c *Controller) labelKey() string {
	if c.LabelKey == "" {
		return DefaultLabelKey
	}
	return c.LabelKey
}

func (c *Controller) labelValue() string {
	if c.LabelValue == "" {
		return DefaultLabelValue
	}
	return c.LabelValue
}

func (c *Controller) labelSelector() string {
	return c.labelKey() + "=" + c.labelValue()
}

func (c *Controller) driverName() string {
	if c.DriverName == "" {
		return driver.DriverName
	}
	return c.DriverName
}

// podIsReady reports whether the pod's Ready condition is True. A ready pod is
// never a fencing candidate, whatever its node looks like.
func podIsReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeHasFailed reports whether the node has failed, and says how.
//
// The signals are Kubernetes' own: the unreachable or not-ready NoExecute taint
// that the node lifecycle controller applies, or a Ready condition that is not
// True. A node that is merely cordoned, drained or unschedulable is NOT failed —
// it is being maintained, by somebody who did not ask for its storage access to
// be revoked.
func nodeHasFailed(node *corev1.Node) (string, bool) {
	for _, t := range node.Spec.Taints {
		if t.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if t.Key == corev1.TaintNodeUnreachable || t.Key == corev1.TaintNodeNotReady {
			return "node carries the " + t.Key + " NoExecute taint", true
		}
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
			return "node Ready condition is " + string(cond.Status), true
		}
	}
	return "", false
}
