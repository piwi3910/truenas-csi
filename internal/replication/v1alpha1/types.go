// Package v1alpha1 defines the StorageProtectionGroup custom resource: the
// declarative face of internal/replication.
//
// The resource is deliberately thin. It names a set of driver-managed volumes,
// a source appliance and a target appliance, and one requested action. Every
// safety decision — is this volume ours, has a failover already happened, has
// the source diverged — is made in internal/replication against the appliance,
// not by whatever wrote the object.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version this package serves.
var GroupVersion = schema.GroupVersion{Group: "replication.truenas.io", Version: "v1alpha1"}

// SchemeBuilder registers the types with a runtime.Scheme.
//
// This is apimachinery's builder rather than controller-runtime's, which is
// deprecated for exactly the reason that applies here: an api package should be
// cheap to import, so it should not drag controller-runtime in behind it.
var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

// AddToScheme adds these types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &StorageProtectionGroup{}, &StorageProtectionGroupList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}

// Action is a requested replication operation.
//
// An action is a one-shot request, not a desired state: the controller records
// which action it has already carried out for which generation, so re-writing
// the same action never repeats it. That is what keeps "failover" from firing
// again every time the object is resynced.
type Action string

const (
	// ActionNone is the steady state: keep the group's tasks in place.
	ActionNone Action = ""
	// ActionFailover promotes the target appliance to primary.
	ActionFailover Action = "Failover"
	// ActionTestFailover clones the last replicated snapshot into scratch
	// datasets. It never touches production.
	ActionTestFailover Action = "TestFailover"
	// ActionStopTestFailover destroys the scratch clones.
	ActionStopTestFailover Action = "StopTestFailover"
	// ActionFailback returns primacy to the source appliance.
	ActionFailback Action = "Failback"
	// ActionSuspend stops replication transfers.
	ActionSuspend Action = "Suspend"
	// ActionResume restarts replication transfers.
	ActionResume Action = "Resume"
	// ActionCreateRemoteVolumes provisions the target-side datasets.
	ActionCreateRemoteVolumes Action = "CreateRemoteVolumes"
)

// CronSchedule is when snapshots are taken and replicated.
type CronSchedule struct {
	// +optional
	Minute string `json:"minute,omitempty"`
	// +optional
	Hour string `json:"hour,omitempty"`
	// +optional
	DayOfMonth string `json:"dayOfMonth,omitempty"`
	// +optional
	Month string `json:"month,omitempty"`
	// +optional
	DayOfWeek string `json:"dayOfWeek,omitempty"`
}

// SnapshotRetention is how long source snapshots are kept.
type SnapshotRetention struct {
	Value int `json:"value"`
	// +kubebuilder:validation:Enum=HOUR;DAY;WEEK;MONTH;YEAR
	Unit string `json:"unit"`
}

// StorageProtectionGroupSpec is the desired protection for a set of volumes.
type StorageProtectionGroupSpec struct {
	// SourceBackend is the configured appliance holding the volumes.
	SourceBackend string `json:"sourceBackend"`
	// TargetBackend is the configured appliance receiving the replicas.
	TargetBackend string `json:"targetBackend"`

	// VolumeHandles are the CSI volume handles to protect. Every one must name
	// a dataset this driver created; a group containing anything else is
	// refused, because replication overwrites its target.
	// +kubebuilder:validation:MinItems=1
	VolumeHandles []string `json:"volumeHandles"`

	// SSHCredentialID is the TrueNAS keychain credential (SSH_CREDENTIALS) on
	// the source appliance that describes the target.
	SSHCredentialID int `json:"sshCredentialID"`
	// ReverseSSHCredentialID is the equivalent credential on the target
	// describing the source. Failback requires it.
	// +optional
	ReverseSSHCredentialID int `json:"reverseSSHCredentialID,omitempty"`

	// Schedule is how often snapshots are taken and replicated.
	// +optional
	Schedule CronSchedule `json:"schedule,omitempty"`
	// Retention is how long source snapshots are kept.
	// +optional
	Retention *SnapshotRetention `json:"retention,omitempty"`

	// Action is the operation to perform once. Clearing it leaves the group as
	// it is; repeating it does nothing, because the controller records the
	// generation it acted on.
	// +optional
	// +kubebuilder:validation:Enum="";Failover;TestFailover;StopTestFailover;Failback;Suspend;Resume;CreateRemoteVolumes
	Action Action `json:"action,omitempty"`

	// Force overrides a refusal. Read status.message first: every refusal says
	// precisely what it refused and why.
	// +optional
	Force bool `json:"force,omitempty"`

	// DeleteTargetDatasets destroys the replicated datasets when the group is
	// deleted. Datasets this driver did not create are always refused.
	// +optional
	DeleteTargetDatasets bool `json:"deleteTargetDatasets,omitempty"`
}

// VolumeStatus is one member's replication state.
type VolumeStatus struct {
	Handle string `json:"handle"`
	// RemoteDataset is where the replica lives on the target appliance.
	// +optional
	RemoteDataset string `json:"remoteDataset,omitempty"`
	// FailoverSnapshot is the snapshot the target held when it was promoted.
	// Failback measures divergence against it.
	// +optional
	FailoverSnapshot string `json:"failoverSnapshot,omitempty"`
	// TestFailoverDataset is the scratch clone a rehearsal exposed.
	// +optional
	TestFailoverDataset string `json:"testFailoverDataset,omitempty"`
}

// StorageProtectionGroupStatus is the observed protection state.
//
// It is also the driver's durable memory: Phase is what makes failover
// idempotent, and losing it would allow a second promotion.
type StorageProtectionGroupStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Phase is Ready, Suspended, FailingOver, FailedOver or FailingBack.
	// +optional
	Phase string `json:"phase,omitempty"`
	// Message explains the phase, including exactly what was refused and why.
	// +optional
	Message string `json:"message,omitempty"`

	// ReplicationTaskID is the TrueNAS task backing this group.
	// +optional
	ReplicationTaskID int `json:"replicationTaskID,omitempty"`
	// TaskState is the middleware's own view of that task.
	// +optional
	TaskState string `json:"taskState,omitempty"`
	// Suspended mirrors the replication task's enabled flag.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
	// TestFailoverActive reports whether scratch clones are exposed.
	// +optional
	TestFailoverActive bool `json:"testFailoverActive,omitempty"`

	// LastAction and LastActionGeneration record which one-shot action has
	// already been carried out, so a resync cannot repeat it.
	// +optional
	LastAction Action `json:"lastAction,omitempty"`
	// +optional
	LastActionGeneration int64 `json:"lastActionGeneration,omitempty"`

	// Volumes is per-member detail.
	// +optional
	Volumes []VolumeStatus `json:"volumes,omitempty"`

	// SnapshotTaskIDs maps a volume name to its periodic snapshot task.
	// +optional
	SnapshotTaskIDs map[string]int `json:"snapshotTaskIDs,omitempty"`

	// Conditions carries Ready and Refused.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types set on a StorageProtectionGroup.
const (
	// ConditionReady is true when the group's tasks exist and match the spec.
	ConditionReady = "Ready"
	// ConditionRefused is true when an action was refused on safety grounds and
	// a human must decide. It is never cleared by a retry, only by a new spec.
	ConditionRefused = "Refused"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceBackend`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetBackend`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Task",type=string,JSONPath=`.status.taskState`

// StorageProtectionGroup is a set of driver-managed volumes replicated from one
// TrueNAS appliance to another.
type StorageProtectionGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StorageProtectionGroupSpec   `json:"spec,omitempty"`
	Status StorageProtectionGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StorageProtectionGroupList is a list of StorageProtectionGroups.
type StorageProtectionGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StorageProtectionGroup `json:"items"`
}

// DeepCopyInto copies the receiver into out.
func (in *CronSchedule) DeepCopyInto(out *CronSchedule) { *out = *in }

// DeepCopy returns a copy.
func (in *CronSchedule) DeepCopy() *CronSchedule {
	if in == nil {
		return nil
	}
	out := new(CronSchedule)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *SnapshotRetention) DeepCopyInto(out *SnapshotRetention) { *out = *in }

// DeepCopy returns a copy.
func (in *SnapshotRetention) DeepCopy() *SnapshotRetention {
	if in == nil {
		return nil
	}
	out := new(SnapshotRetention)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *VolumeStatus) DeepCopyInto(out *VolumeStatus) { *out = *in }

// DeepCopy returns a copy.
func (in *VolumeStatus) DeepCopy() *VolumeStatus {
	if in == nil {
		return nil
	}
	out := new(VolumeStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *StorageProtectionGroupSpec) DeepCopyInto(out *StorageProtectionGroupSpec) {
	*out = *in
	if in.VolumeHandles != nil {
		out.VolumeHandles = make([]string, len(in.VolumeHandles))
		copy(out.VolumeHandles, in.VolumeHandles)
	}
	out.Schedule = in.Schedule
	if in.Retention != nil {
		out.Retention = new(SnapshotRetention)
		*out.Retention = *in.Retention
	}
}

// DeepCopy returns a copy.
func (in *StorageProtectionGroupSpec) DeepCopy() *StorageProtectionGroupSpec {
	if in == nil {
		return nil
	}
	out := new(StorageProtectionGroupSpec)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *StorageProtectionGroupStatus) DeepCopyInto(out *StorageProtectionGroupStatus) {
	*out = *in
	if in.Volumes != nil {
		out.Volumes = make([]VolumeStatus, len(in.Volumes))
		copy(out.Volumes, in.Volumes)
	}
	if in.SnapshotTaskIDs != nil {
		out.SnapshotTaskIDs = make(map[string]int, len(in.SnapshotTaskIDs))
		for k, v := range in.SnapshotTaskIDs {
			out.SnapshotTaskIDs[k] = v
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

// DeepCopy returns a copy.
func (in *StorageProtectionGroupStatus) DeepCopy() *StorageProtectionGroupStatus {
	if in == nil {
		return nil
	}
	out := new(StorageProtectionGroupStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *StorageProtectionGroup) DeepCopyInto(out *StorageProtectionGroup) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a copy.
func (in *StorageProtectionGroup) DeepCopy() *StorageProtectionGroup {
	if in == nil {
		return nil
	}
	out := new(StorageProtectionGroup)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a copy as a runtime.Object.
func (in *StorageProtectionGroup) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *StorageProtectionGroupList) DeepCopyInto(out *StorageProtectionGroupList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]StorageProtectionGroup, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy.
func (in *StorageProtectionGroupList) DeepCopy() *StorageProtectionGroupList {
	if in == nil {
		return nil
	}
	out := new(StorageProtectionGroupList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject returns a copy as a runtime.Object.
func (in *StorageProtectionGroupList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
