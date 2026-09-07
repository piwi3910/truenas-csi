package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EndpointPattern is the only endpoint form the API server accepts.
//
// This is not stylistic and it is not a duplicate of the driver's own check.
// TrueNAS 25.10 REVOKES an API key the moment it is presented over a plaintext
// connection, so a `ws://` endpoint does not merely fail to connect — it
// destroys the credential and an administrator has to issue a new one by hand.
// Rejecting it in the CRD schema means `kubectl apply` fails before the
// operator ever renders a Secret, let alone before a pod dials the appliance.
//
// The expression is anchored at both ends: Kubernetes evaluates `pattern` as an
// unanchored match, so an unanchored expression would happily accept
// "http://evil/?x=wss://nas".
const EndpointPattern = `^wss://[^\s/?#]+(?:/[^\s]*)?$`

// SecretKeyReference names one key in one Secret in the driver's namespace.
//
// Credentials only ever reach this API as a reference. There is deliberately no
// field anywhere in this type set that holds an API key literally: a key typed
// into a CR would be readable by anyone with `get truenascsidrivers`, would be
// copied into every backup of the cluster's resource inventory, and would show
// up in `kubectl get -o yaml` output pasted into bug reports.
type SecretKeyReference struct {
	// Name of the Secret, which must live in the driver's namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Key within the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[-._a-zA-Z0-9]+$`
	// +kubebuilder:default=apiKey
	// +optional
	Key string `json:"key,omitempty"`
}

// SecretKey returns the key to read, applying the default the API server would
// have applied. A CR created before the default existed can still be read.
func (r SecretKeyReference) SecretKey() string {
	if r.Key == "" {
		return "apiKey"
	}
	return r.Key
}

// BackendSpec is one TrueNAS appliance. It mirrors internal/config.Backend,
// except that the API key is a reference rather than a value.
//
// +kubebuilder:validation:XValidation:rule="!(has(self.reservedBytes) && has(self.reservedPercent))",message="set at most one of reservedBytes and reservedPercent"
type BackendSpec struct {
	// Name selects this appliance from a StorageClass `backend` parameter, and
	// is part of every volume ID this backend provisions. Renaming a backend
	// orphans its volumes, so the name is effectively immutable in practice.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Endpoint is the appliance's websocket JSON-RPC endpoint. It must be
	// wss://; see EndpointPattern for why that is enforced here.
	// +kubebuilder:validation:MinLength=8
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^wss://[^\s/?#]+(?:/[^\s]*)?$`
	// +kubebuilder:example=`wss://nas1.example.com/api/current`
	Endpoint string `json:"endpoint"`

	// Username is the TrueNAS account the API key belongs to. It should be a
	// least-privilege account, not root.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Username string `json:"username"`

	// APIKeySecretRef points at the Secret key holding this appliance's API key.
	APIKeySecretRef SecretKeyReference `json:"apiKeySecretRef"`

	// Pool is the ZFS pool volumes are created in.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][-A-Za-z0-9_.:]*$`
	Pool string `json:"pool"`

	// ParentDataset is the dataset every volume is created under. It must not
	// contain "..": the driver builds dataset paths from volume IDs, and a
	// traversal component would let a volume escape its parent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][-A-Za-z0-9_.:]*(/[-A-Za-z0-9_.:]+)*$`
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="parentDataset must not contain .."
	ParentDataset string `json:"parentDataset"`

	// CACertSecretRef names a Secret key holding the PEM bundle trusted for this
	// appliance. A stock TrueNAS certificate is self-signed with
	// SAN=DNS:localhost and cannot be verified against a real address, so either
	// supply a certificate here or accept the risk below.
	// +optional
	CACertSecretRef *SecretKeyReference `json:"caCertSecretRef,omitempty"`

	// InsecureSkipVerify disables certificate verification for this appliance.
	// It exists because of the self-signed stock certificate, it is never the
	// default, and the driver logs a loud warning whenever it is on.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// ReservedBytes is capacity held back from the pool's reported free space so
	// the driver cannot provision the pool to full. Mutually exclusive with
	// ReservedPercent.
	// +optional
	ReservedBytes *resource.Quantity `json:"reservedBytes,omitempty"`

	// ReservedPercent is the same reserve expressed as a percentage of pool
	// capacity. Mutually exclusive with ReservedBytes.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=99
	// +optional
	ReservedPercent *int32 `json:"reservedPercent,omitempty"`
}

// ImageSpec pins the driver image.
type ImageSpec struct {
	// Repository is the driver image repository.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default="ghcr.io/piwi3910/truenas-csi"
	// +optional
	Repository string `json:"repository,omitempty"`

	// Tag is the driver version. Empty means the chart's appVersion. The
	// operator refuses to apply a tag whose version is incompatible with the
	// CSI sidecars the chart pins; see the VersionSkew condition reason.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][-A-Za-z0-9_.]*$`
	// +optional
	Tag string `json:"tag,omitempty"`

	// PullPolicy for the driver image.
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	PullPolicy string `json:"pullPolicy,omitempty"`

	// PullSecrets are image pull Secrets in the driver's namespace.
	// +optional
	// +listType=atomic
	PullSecrets []string `json:"pullSecrets,omitempty"`
}

// ControllerSpec configures the controller Deployment.
type ControllerSpec struct {
	// Replicas of the controller. Two is the useful minimum: leader election
	// makes the second a warm standby, so provisioning survives a node loss.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5
	// +kubebuilder:default=2
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// NodeSelector for the controller pods.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// NodeSpec configures the node DaemonSet.
type NodeSpec struct {
	// KubeletDir is the kubelet root directory. k3s and microk8s do not use the
	// default, and a wrong value makes every mount fail in a way that looks like
	// a driver bug.
	// +kubebuilder:validation:Pattern=`^/[^\s]*[^/\s]$`
	// +kubebuilder:validation:MaxLength=512
	// +kubebuilder:default="/var/lib/kubelet"
	// +optional
	KubeletDir string `json:"kubeletDir,omitempty"`

	// NodeSelector restricts which nodes run the plugin. A node without the
	// plugin cannot mount any volume of this driver at all, so leaving this
	// empty is almost always right.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// RolloutTimeoutSeconds bounds how long the operator waits for one node to
	// report its plugin ready, and for that node to report no volume mid-stage,
	// before it gives up and reports Degraded rather than rolling on regardless.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=600
	// +optional
	RolloutTimeoutSeconds int32 `json:"rolloutTimeoutSeconds,omitempty"`
}

// SnapshotterSpec toggles the snapshot sidecar.
type SnapshotterSpec struct {
	// Enabled deploys the csi-snapshotter sidecar and its RBAC. The
	// external-snapshotter CRDs and the cluster-wide snapshot controller must
	// already exist: they are a cluster singleton, and the operator will never
	// install them, because two drivers installing them breaks snapshots for
	// both.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// VolumeSnapshotClassSpec configures the VolumeSnapshotClass.
type VolumeSnapshotClassSpec struct {
	// Enabled creates the VolumeSnapshotClass. It needs the snapshot CRDs to
	// exist, so it is off by default.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Name of the VolumeSnapshotClass.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	// +optional
	Name string `json:"name,omitempty"`

	// IsDefault marks it the cluster's default VolumeSnapshotClass.
	// +kubebuilder:default=false
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`

	// DeletionPolicy decides whether deleting a VolumeSnapshot deletes the ZFS
	// snapshot behind it.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	// +optional
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
}

// StorageClassSpec is one StorageClass the operator creates.
//
// +kubebuilder:validation:XValidation:rule="self.protocol == 'iscsi' || !has(self.iscsi)",message="iscsi options are only valid when protocol is iscsi"
// +kubebuilder:validation:XValidation:rule="self.protocol == 'nfs' || !has(self.nfs)",message="nfs options are only valid when protocol is nfs"
type StorageClassSpec struct {
	// Name of the StorageClass.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Name string `json:"name"`

	// Protocol this class provisions with.
	// +kubebuilder:validation:Enum=nfs;iscsi
	Protocol string `json:"protocol"`

	// Backend names one of spec.backends. A CEL rule on the whole resource
	// checks that it actually exists, so a typo is rejected at apply time rather
	// than at the first PVC.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Backend string `json:"backend"`

	// Pool overrides the backend's pool for volumes of this class.
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Pool string `json:"pool,omitempty"`

	// ParentDataset overrides the backend's parent dataset for this class.
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:XValidation:rule="!self.contains('..')",message="parentDataset must not contain .."
	// +optional
	ParentDataset string `json:"parentDataset,omitempty"`

	// IsDefault marks this the cluster's default StorageClass.
	// +kubebuilder:default=false
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`

	// ReclaimPolicy for volumes of this class.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	// +optional
	ReclaimPolicy string `json:"reclaimPolicy,omitempty"`

	// AllowVolumeExpansion permits growing a PVC of this class.
	// +kubebuilder:default=true
	// +optional
	AllowVolumeExpansion *bool `json:"allowVolumeExpansion,omitempty"`

	// VolumeBindingMode for this class.
	// +kubebuilder:validation:Enum=Immediate;WaitForFirstConsumer
	// +kubebuilder:default=Immediate
	// +optional
	VolumeBindingMode string `json:"volumeBindingMode,omitempty"`

	// MountOptions applied to every volume of this class.
	// +optional
	// +listType=atomic
	MountOptions []string `json:"mountOptions,omitempty"`

	// NFS options, valid only when protocol is nfs.
	// +optional
	NFS *NFSClassOptions `json:"nfs,omitempty"`

	// ISCSI options, valid only when protocol is iscsi.
	// +optional
	ISCSI *ISCSIClassOptions `json:"iscsi,omitempty"`
}

// NFSClassOptions are the NFS-only StorageClass parameters.
type NFSClassOptions struct {
	// Version of NFS to export. 4 is preferred: NFSv3 locking is stateless and
	// interacts badly with pod rescheduling.
	// +kubebuilder:validation:Enum="3";"4"
	// +kubebuilder:default="4"
	// +optional
	Version string `json:"version,omitempty"`

	// Networks allowed to mount the export, as CIDRs.
	// +optional
	// +listType=atomic
	Networks []string `json:"networks,omitempty"`

	// Mode is the dataset's permission bits at creation. A fresh dataset is
	// root:root 0755, so a non-root pod cannot write to it unless this is set.
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	// +kubebuilder:default="0770"
	// +optional
	Mode string `json:"mode,omitempty"`

	// UID owning the dataset at creation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	UID *int64 `json:"uid,omitempty"`

	// GID owning the dataset at creation.
	// +kubebuilder:validation:Minimum=0
	// +optional
	GID *int64 `json:"gid,omitempty"`
}

// ISCSIClassOptions are the iSCSI-only StorageClass parameters.
type ISCSIClassOptions struct {
	// FSType laid down on the LUN.
	// +kubebuilder:validation:Enum=ext4;xfs
	// +kubebuilder:default=ext4
	// +optional
	FSType string `json:"fsType,omitempty"`

	// Sparse thin-provisions the zvol.
	// +kubebuilder:default=true
	// +optional
	Sparse *bool `json:"sparse,omitempty"`

	// PortalID reuses an existing TrueNAS portal instead of letting the driver
	// create one. Zero means the driver manages the portal.
	// +kubebuilder:validation:Minimum=0
	// +optional
	PortalID *int32 `json:"portalID,omitempty"`

	// CHAP enables CHAP on the shared target.
	// +kubebuilder:default=true
	// +optional
	CHAP *bool `json:"chap,omitempty"`

	// InitiatorACL restricts the shared target to the cluster's node IQNs.
	// Turning it off exposes every LUN on the target to any initiator that can
	// reach the portal.
	// +kubebuilder:default=true
	// +optional
	InitiatorACL *bool `json:"initiatorACL,omitempty"`

	// Multipath uses multipath when the node supports it, degrading to a single
	// path with a warning when it does not.
	// +kubebuilder:default=false
	// +optional
	Multipath *bool `json:"multipath,omitempty"`
}

// TrueNASCSIDriverSpec is a typed projection of the in-repo Helm chart's values.
//
// The projection is deliberately narrower than values.yaml. Everything here is
// validated by the API server at apply time, which is the entire reason for
// preferring this resource over a raw values file: a wrong endpoint scheme, a
// StorageClass naming a backend that does not exist, or a reserve set two ways
// at once is rejected by `kubectl apply`, not discovered by the first PVC that
// fails to bind an hour later.
//
// Name uniqueness within backends and storageClasses is enforced by the
// listType=map declarations below, not by a CEL rule.
//
// +kubebuilder:validation:XValidation:rule="!has(self.storageClasses) || self.storageClasses.all(c, self.backends.exists(b, b.name == c.backend))",message="every storageClass.backend must name a backend in spec.backends"
type TrueNASCSIDriverSpec struct {
	// Namespace the driver's workloads are installed into. The operator creates
	// it if it does not exist.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:default=truenas-csi
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Backends are the TrueNAS appliances this driver provisions against. At
	// least one is required: a driver with no backend cannot serve any volume.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Backends []BackendSpec `json:"backends"`

	// Image pins the driver image.
	// +optional
	Image ImageSpec `json:"image,omitempty"`

	// LogLevel for the driver.
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	LogLevel string `json:"logLevel,omitempty"`

	// Controller configures the controller Deployment.
	// +optional
	Controller ControllerSpec `json:"controller,omitempty"`

	// Node configures the node DaemonSet.
	// +optional
	Node NodeSpec `json:"node,omitempty"`

	// Snapshotter toggles the snapshot sidecar.
	// +optional
	Snapshotter SnapshotterSpec `json:"snapshotter,omitempty"`

	// VolumeSnapshotClass configures the VolumeSnapshotClass.
	// +optional
	VolumeSnapshotClass VolumeSnapshotClassSpec `json:"volumeSnapshotClass,omitempty"`

	// StorageClasses the operator creates. Omit them and manage StorageClasses
	// yourself; the driver works either way.
	// +kubebuilder:validation:MaxItems=32
	// +optional
	// +listType=map
	// +listMapKey=name
	StorageClasses []StorageClassSpec `json:"storageClasses,omitempty"`
}

// BackendStatus reports what the operator last observed about one appliance.
type BackendStatus struct {
	// Name of the backend.
	Name string `json:"name"`

	// Reachable is what the driver's own truenas_csi_backend_up gauge said the
	// last time the operator scraped it. Unknown until the controller is up.
	// +kubebuilder:validation:Enum=True;False;Unknown
	Reachable string `json:"reachable"`

	// OrphanedVolumes is the count of driver-owned datasets with no matching
	// PersistentVolume, as the driver's orphan reconciler reports it. The
	// reconciler never deletes anything; this number is a prompt to investigate,
	// not a failure.
	// +optional
	OrphanedVolumes *int32 `json:"orphanedVolumes,omitempty"`

	// Message explains a non-True Reachable.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Message string `json:"message,omitempty"`

	// LastProbeTime is when the operator last scraped the driver's metrics.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

// RolloutStatus reports the drain-aware node rollout.
type RolloutStatus struct {
	// DesiredRevision is the pod-template hash the node plugin should be at.
	// +optional
	DesiredRevision string `json:"desiredRevision,omitempty"`

	// UpdatedNodes have the plugin at DesiredRevision and report it ready.
	// +optional
	UpdatedNodes int32 `json:"updatedNodes,omitempty"`

	// TotalNodes are the nodes the plugin should run on.
	// +optional
	TotalNodes int32 `json:"totalNodes,omitempty"`

	// CurrentNode is the node the operator is rolling right now, if any. Only
	// ever one: taking the plugin off a node with a volume mid-stage is how a
	// storage upgrade takes workloads down, so nodes are rolled strictly one at
	// a time and only when the node reports clear.
	// +optional
	CurrentNode string `json:"currentNode,omitempty"`

	// WaitingFor explains why the rollout is not advancing, which is usually a
	// node that is legitimately busy rather than anything broken.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	WaitingFor string `json:"waitingFor,omitempty"`
}

// Condition types the operator sets.
const (
	// ConditionReady is true when the rendered release is fully applied, the
	// node rollout is complete and every backend is reachable.
	ConditionReady = "Ready"
	// ConditionProgressing is true while the operator is applying or rolling.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is true when the operator refused to act, or when
	// something it applied is not healthy.
	ConditionDegraded = "Degraded"
)

// Condition reasons the operator sets.
const (
	// ReasonVersionSkew is set when the driver image version is incompatible
	// with the CSI sidecar versions the chart pins. The operator applies
	// nothing in this state: a half-applied release with a driver that cannot
	// talk to its own sidecars is worse than an unchanged one.
	ReasonVersionSkew = "VersionSkew"
	// ReasonRenderFailed is set when the chart could not be rendered.
	ReasonRenderFailed = "RenderFailed"
	// ReasonApplyFailed is set when a rendered object could not be applied.
	ReasonApplyFailed = "ApplyFailed"
	// ReasonSecretMissing is set when a referenced credential Secret or key is
	// absent.
	ReasonSecretMissing = "SecretMissing"
	// ReasonRollingOut is set while the node plugin rollout is in progress.
	ReasonRollingOut = "RollingOut"
	// ReasonRotatingCredentials is set while the operator restarts pods after an
	// API key change.
	ReasonRotatingCredentials = "RotatingCredentials"
	// ReasonBackendUnreachable is set when a backend's connection is down.
	ReasonBackendUnreachable = "BackendUnreachable"
	// ReasonReconciled is set when everything is applied and healthy.
	ReasonReconciled = "Reconciled"
)

// TrueNASCSIDriverStatus is the observed state of a TrueNASCSIDriver.
type TrueNASCSIDriverStatus struct {
	// ObservedGeneration is the .metadata.generation this status describes. A
	// status whose observedGeneration lags the generation is stale, and every
	// condition in it describes the previous spec.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions are Ready, Progressing and Degraded.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// AppliedVersion is the driver version currently applied.
	// +optional
	AppliedVersion string `json:"appliedVersion,omitempty"`

	// CredentialsRevision fingerprints the referenced credential Secrets. When
	// it changes, the operator restarts the driver's pods in order rather than
	// leaving a stale key in a running process's memory.
	// +optional
	CredentialsRevision string `json:"credentialsRevision,omitempty"`

	// Backends reports per-appliance reachability and orphan counts.
	// +optional
	// +listType=map
	// +listMapKey=name
	Backends []BackendStatus `json:"backends,omitempty"`

	// Rollout reports the drain-aware node rollout.
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`
}

// TrueNASCSIDriver installs and owns the lifecycle of one TrueNAS CSI driver.
//
// The resource is cluster-scoped because most of what it owns is: the
// CSIDriver object, the ClusterRoles and the StorageClasses have no namespace,
// and a namespaced owner cannot own them.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tncsi;truenascsi
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.namespace`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.appliedVersion`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TrueNASCSIDriver struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrueNASCSIDriverSpec   `json:"spec,omitempty"`
	Status TrueNASCSIDriverStatus `json:"status,omitempty"`
}

// TrueNASCSIDriverList is a list of TrueNASCSIDriver.
//
// +kubebuilder:object:root=true
type TrueNASCSIDriverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrueNASCSIDriver `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrueNASCSIDriver{}, &TrueNASCSIDriverList{})
}
