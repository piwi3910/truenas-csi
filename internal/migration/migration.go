// Package migration imports an existing foreign PersistentVolume's data into a
// volume this driver provisioned, so a cluster can move off Longhorn — or any
// other CSI driver — without an operator hand-copying data between mounts.
//
// The driver process has neither volume mounted, so it does not copy anything
// itself: it renders a Kubernetes Job that mounts the source claim READ-ONLY
// and the target claim read-write, copies with rsync (tar when rsync is
// absent), and refuses to exit zero unless a checksum manifest of both sides
// matches file for file.
//
// Four rules are non-negotiable, and each has a test named after it:
//
//   - The target must be a dataset THIS DRIVER created — proven by the
//     ownership marker with source LOCAL. Copying into anything else would
//     overwrite an operator's real data.
//   - The source is mounted read-only. A migration must never be able to modify
//     the thing it is copying from.
//   - Success requires verification. A partial copy reported as complete is the
//     worst outcome available here, because the operator then deletes the source.
//   - The source is never deleted. Migration hands back a report; reclaiming the
//     old volume is the operator's explicit decision.
package migration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/driver"
	"github.com/piwi3910/truenas-csi/internal/obs"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/volume"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ErrNotEligible means the request does not describe a migration this driver
// can perform: a claim is unbound, the target is not served by this driver, or
// the target is too small for the source.
var ErrNotEligible = errors.New("migration request is not eligible")

// Migrator plans and runs migrations into one appliance's volumes.
type Migrator struct {
	kube    kubernetes.Interface
	client  *truenas.Client
	backend config.Backend
	// driverName is the CSI driver name a target PV must carry. It is a field
	// rather than a constant reference so a test can point it at a fixture.
	driverName string
}

// New builds a Migrator for one appliance.
func New(kube kubernetes.Interface, c *truenas.Client, b config.Backend) *Migrator {
	return &Migrator{kube: kube, client: c, backend: b, driverName: driver.DriverName}
}

// Endpoint is one side of a migration as Kubernetes describes it.
type Endpoint struct {
	Namespace     string
	Claim         string
	PersistentVol string
	Driver        string
	Handle        string
	CapacityBytes int64
}

// Request asks for a migration. Both claims must live in the same namespace,
// because one pod has to mount both.
type Request struct {
	SourceNamespace string
	SourcePVC       string
	TargetPVC       string
	Mode            Mode
	Image           string
	ServiceAccount  string
	BackoffLimit    int32
}

// Plan is what a migration would do. It is inert: nothing in the cluster and
// nothing on the appliance has been touched when Plan returns.
type Plan struct {
	Source Endpoint
	Target Endpoint

	// TargetVolume is the parsed handle of the driver-managed target.
	TargetVolume volume.ID
	// TargetDataset is the ZFS dataset the copy lands in. It has been proven
	// driver-owned before this field is set.
	TargetDataset string

	Namespace      string
	JobName        string
	Mode           Mode
	Image          string
	ServiceAccount string
	BackoffLimit   int32

	// Job is the copy Job exactly as Run would create it.
	Job *batchv1.Job
}

// Phase is where a migration has got to.
type Phase string

const (
	// PhaseRunning means the copy Job exists and has not finished.
	PhaseRunning Phase = "Running"
	// PhaseSucceeded means the Job finished AND its verification passed.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseFailed means the Job failed, or finished without proving the copy.
	PhaseFailed Phase = "Failed"
)

// Report is the outcome an operator acts on.
type Report struct {
	Namespace string
	JobName   string
	Source    Endpoint
	Target    Endpoint
	Phase     Phase
	Summary   *Summary
	Verified  bool
	Message   string

	// SourceRetained is always true. It is a field rather than a comment so
	// that the report an operator reads states, in the report itself, that
	// retiring the old volume is still their decision.
	SourceRetained bool
}

// Plan resolves both claims, proves the target is a volume this driver created,
// and renders the copy Job without creating it.
func (m *Migrator) Plan(ctx context.Context, r Request) (*Plan, error) {
	if r.SourceNamespace == "" || r.SourcePVC == "" || r.TargetPVC == "" {
		return nil, fmt.Errorf("%w: sourceNamespace, sourcePVC and targetPVC are all required", ErrNotEligible)
	}
	if r.SourcePVC == r.TargetPVC {
		return nil, fmt.Errorf("%w: source and target are the same claim %q", ErrNotEligible, r.SourcePVC)
	}

	src, err := m.endpoint(ctx, r.SourceNamespace, r.SourcePVC)
	if err != nil {
		return nil, fmt.Errorf("source: %w", err)
	}
	dst, err := m.endpoint(ctx, r.SourceNamespace, r.TargetPVC)
	if err != nil {
		return nil, fmt.Errorf("target: %w", err)
	}

	if dst.Driver != m.driverName {
		return nil, fmt.Errorf("%w: target %s/%s is served by %q, not %q",
			ErrNotEligible, dst.Namespace, dst.Claim, dst.Driver, m.driverName)
	}
	id, err := volume.ParseID(dst.Handle)
	if err != nil {
		return nil, fmt.Errorf("target %s/%s: %w", dst.Namespace, dst.Claim, err)
	}
	if id.Backend != m.backend.Name {
		return nil, fmt.Errorf("%w: target lives on backend %q, this migrator serves %q",
			ErrNotEligible, id.Backend, m.backend.Name)
	}
	if err := volume.Confine(id, m.backend.Pool, m.backend.ParentDataset); err != nil {
		return nil, fmt.Errorf("target %s/%s: %w", dst.Namespace, dst.Claim, err)
	}

	// The ownership guard. Everything downstream writes into this dataset, so
	// a dataset the driver did not create must stop the migration here, before
	// a Job that would overwrite it can exist.
	if err := m.verifyOwned(ctx, id.DatasetPath()); err != nil {
		return nil, err
	}

	if dst.CapacityBytes > 0 && src.CapacityBytes > 0 && dst.CapacityBytes < src.CapacityBytes {
		return nil, fmt.Errorf("%w: target is %d bytes, source is %d bytes",
			ErrNotEligible, dst.CapacityBytes, src.CapacityBytes)
	}

	p := &Plan{
		Source:         src,
		Target:         dst,
		TargetVolume:   id,
		TargetDataset:  id.DatasetPath(),
		Namespace:      r.SourceNamespace,
		JobName:        jobName(r.SourceNamespace, r.SourcePVC, r.TargetPVC),
		Mode:           r.Mode,
		Image:          r.Image,
		ServiceAccount: r.ServiceAccount,
		BackoffLimit:   r.BackoffLimit,
	}
	if p.Mode == "" {
		p.Mode = ModeAuto
	}
	if p.Image == "" {
		p.Image = DefaultImage
	}
	p.Job = buildJob(p)

	obs.Logger(ctx).Info("migration planned",
		"backend", m.backend.Name,
		"source_pvc", src.Namespace+"/"+src.Claim, "source_driver", src.Driver,
		"target_pvc", dst.Namespace+"/"+dst.Claim, "target_dataset", p.TargetDataset,
		"mode", string(p.Mode), "job", p.JobName)
	return p, nil
}

// Run creates the copy Job, or adopts the one a previous Run left behind, and
// reports where the migration has got to.
//
// It is safe to call again after a failure. The Job name is derived from the
// two claims, so a second Run adopts rather than starting a second copy into the
// same volume, and rsync re-runs over a partial target transferring only what
// differs.
func (m *Migrator) Run(ctx context.Context, p *Plan) (*Report, error) {
	if p == nil || p.Job == nil {
		return nil, fmt.Errorf("%w: Run needs a Plan from Plan()", ErrNotEligible)
	}
	jobs := m.kube.BatchV1().Jobs(p.Namespace)

	job, err := jobs.Get(ctx, p.JobName, metav1.GetOptions{})
	switch {
	case err == nil:
		obs.Logger(ctx).Info("migration job already exists, adopting it",
			"job", p.JobName, "namespace", p.Namespace)
	case apierrors.IsNotFound(err):
		job, err = jobs.Create(ctx, p.Job, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Lost a race with a concurrent Run; adopting is still correct.
				if job, err = jobs.Get(ctx, p.JobName, metav1.GetOptions{}); err != nil {
					return nil, fmt.Errorf("adopting migration job %s: %w", p.JobName, err)
				}
				break
			}
			return nil, fmt.Errorf("creating migration job %s: %w", p.JobName, err)
		}
	default:
		return nil, fmt.Errorf("reading migration job %s: %w", p.JobName, err)
	}

	rep := &Report{
		Namespace:      p.Namespace,
		JobName:        p.JobName,
		Source:         p.Source,
		Target:         p.Target,
		Phase:          PhaseRunning,
		SourceRetained: true,
	}

	switch {
	case job.Status.Failed > 0 && finished(job, batchv1.JobFailed):
		rep.Phase = PhaseFailed
		rep.Message = "the copy job failed; re-run to resume from the partial copy"
	case finished(job, batchv1.JobComplete):
		summary, serr := m.summary(ctx, p)
		if serr != nil {
			rep.Phase = PhaseFailed
			rep.Message = serr.Error()
			break
		}
		rep.Summary = summary
		if err := summary.Check(); err != nil {
			// The Job exited zero but could not prove the copy. Reporting this
			// as success is the one failure mode that loses data, because the
			// operator would then reclaim the source.
			rep.Phase = PhaseFailed
			rep.Message = err.Error()
			break
		}
		rep.Phase = PhaseSucceeded
		rep.Verified = true
		rep.Message = fmt.Sprintf("verified %d files; the source volume was NOT deleted — reclaiming it is your decision",
			summary.SourceFiles)
	default:
		rep.Message = "the copy job is running"
	}

	obs.Logger(ctx).Info("migration status",
		"job", rep.JobName, "phase", string(rep.Phase), "verified", rep.Verified,
		"source_retained", rep.SourceRetained, "message", rep.Message)
	return rep, nil
}

func finished(j *batchv1.Job, want batchv1.JobConditionType) bool {
	for _, c := range j.Status.Conditions {
		if c.Type == want && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// summary reads the verification the copy Job wrote to its termination log.
func (m *Migrator) summary(ctx context.Context, p *Plan) (*Summary, error) {
	pods, err := m.kube.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + p.JobName,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods of migration job %s: %w", p.JobName, err)
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase != corev1.PodSucceeded {
			continue
		}
		if newest == nil || pod.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = pod
		}
	}
	if newest == nil {
		return nil, nil
	}
	for _, cs := range newest.Status.ContainerStatuses {
		if cs.Name != containerName || cs.State.Terminated == nil {
			continue
		}
		return ParseSummary([]byte(cs.State.Terminated.Message))
	}
	return nil, nil
}

// endpoint resolves a bound claim to its PersistentVolume.
func (m *Migrator) endpoint(ctx context.Context, namespace, name string) (Endpoint, error) {
	pvc, err := m.kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Endpoint{}, fmt.Errorf("reading PersistentVolumeClaim %s/%s: %w", namespace, name, err)
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		return Endpoint{}, fmt.Errorf("%w: %s/%s is %s, not Bound",
			ErrNotEligible, namespace, name, pvc.Status.Phase)
	}
	pv, err := m.kube.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return Endpoint{}, fmt.Errorf("reading PersistentVolume %s: %w", pvc.Spec.VolumeName, err)
	}

	e := Endpoint{
		Namespace:     namespace,
		Claim:         name,
		PersistentVol: pv.Name,
	}
	if csi := pv.Spec.CSI; csi != nil {
		e.Driver = csi.Driver
		e.Handle = csi.VolumeHandle
	}
	if q, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		e.CapacityBytes = q.Value()
	}
	return e, nil
}

// verifyOwned refuses any target this driver did not create.
//
// The Source == "LOCAL" requirement is the whole point: ZFS user properties are
// inherited, so a marker on the operator's parent dataset would otherwise make
// every pre-existing dataset beneath it look driver-owned — and a migration
// would then copy over the operator's real data.
func (m *Migrator) verifyOwned(ctx context.Context, dataset string) error {
	type property struct {
		Value  string `json:"value"`
		Source string `json:"source"`
	}
	var out []struct {
		ID             string              `json:"id"`
		UserProperties map[string]property `json:"user_properties"`
	}
	start := time.Now()
	err := m.client.CallJSON(ctx, &out, "pool.dataset.query",
		[]any{[]any{"id", "=", dataset}}, map[string]any{})
	obs.ObserveMiddleware("pool.dataset.query", err, time.Since(start))
	if err != nil {
		if truenas.IsNotFound(err) {
			return fmt.Errorf("%w: %q does not exist on %s",
				volume.ErrNotManaged, dataset, m.backend.Name)
		}
		return fmt.Errorf("querying target dataset %q: %w", dataset, err)
	}
	if len(out) == 0 {
		return fmt.Errorf("%w: %q does not exist on %s",
			volume.ErrNotManaged, dataset, m.backend.Name)
	}

	ds := &volume.Dataset{ID: out[0].ID, UserProperties: map[string]volume.Property{}}
	for k, v := range out[0].UserProperties {
		ds.UserProperties[k] = volume.Property{Value: v.Value, Source: v.Source}
	}
	if err := volume.VerifyOwned(ds); err != nil {
		return fmt.Errorf("refusing to migrate into %q: %w", dataset, err)
	}
	return nil
}
