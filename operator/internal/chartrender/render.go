// Package chartrender turns a TrueNASCSIDriver into the object set the in-repo
// Helm chart produces.
//
// The operator renders the same chart Helm users install. There is deliberately
// no second copy of the manifests here: a Deployment written twice drifts, and
// the drift is only ever discovered by whichever install path is not the one
// being tested. The chart is the single source of the manifests, and this
// package is a values compiler plus a renderer.
package chartrender

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	truenasv1alpha1 "github.com/piwi3910/truenas-csi/operator/api/v1alpha1"
)

// DefaultReleaseName is the Helm release name the operator renders under. It is
// fixed so that an operator-managed install produces byte-identical resource
// names to `helm install truenas-csi ./deploy/helm/truenas-csi`, which is what
// makes the migration in docs/operator.md an adoption rather than a reinstall.
const DefaultReleaseName = "truenas-csi"

// Credentials are the resolved Secret contents for one TrueNASCSIDriver, keyed
// by backend name. They exist only in memory for the duration of a render: the
// rendered Secret is applied to the API server, and nothing writes a key to a
// log, an annotation or a status field.
type Credentials struct {
	APIKeys map[string]string
	CACerts map[string]string
}

// LoadChart reads the chart from a directory.
func LoadChart(dir string) (*chart.Chart, error) {
	ch, err := loader.LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load chart from %s: %w", dir, err)
	}
	return ch, nil
}

// KubeVersion is the cluster version the chart renders against. It defaults to
// the driver's documented minimum when the operator cannot discover the real
// one, because a chart that renders for 1.31 renders for everything above it.
type KubeVersion struct {
	Version string
	Major   string
	Minor   string
}

// DefaultKubeVersion is the driver's documented Kubernetes minimum.
var DefaultKubeVersion = KubeVersion{Version: "v1.31.0", Major: "1", Minor: "31"}

// Values compiles a TrueNASCSIDriver plus its resolved credentials into the
// chart's values.
//
// Two values are overridden regardless of what the CR says, because the
// operator owns the behaviour they control:
//
//   - node.updateStrategy.type is forced to OnDelete. The operator rolls node
//     plugin pods itself, one node at a time, and only when that node reports no
//     volume mid-stage. Leaving RollingUpdate in place would let the DaemonSet
//     controller rip a plugin out from under a mounted volume the moment the pod
//     template changed.
//   - snapshotter.install stays false. The VolumeSnapshot CRDs and controller
//     are a cluster-wide singleton; two drivers installing them break snapshots
//     for both.
func Values(cr *truenasv1alpha1.TrueNASCSIDriver, creds Credentials) (map[string]any, error) {
	if cr == nil {
		return nil, errors.New("nil TrueNASCSIDriver")
	}
	if len(cr.Spec.Backends) == 0 {
		return nil, errors.New("spec.backends is empty: a driver with no appliance can serve no volume")
	}

	backends := map[string]any{}
	for _, b := range cr.Spec.Backends {
		key, ok := creds.APIKeys[b.Name]
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("backend %q: no API key resolved from Secret %q key %q",
				b.Name, b.APIKeySecretRef.Name, b.APIKeySecretRef.SecretKey())
		}
		entry := map[string]any{
			"name":               b.Name,
			"endpoint":           b.Endpoint,
			"username":           b.Username,
			"apiKey":             key,
			"pool":               b.Pool,
			"parentDataset":      b.ParentDataset,
			"insecureSkipVerify": b.InsecureSkipVerify,
		}
		if ca, ok := creds.CACerts[b.Name]; ok && strings.TrimSpace(ca) != "" {
			entry["caCert"] = ca
		}
		if b.ReservedBytes != nil {
			entry["reservedBytes"] = b.ReservedBytes.String()
		}
		if b.ReservedPercent != nil {
			entry["reservedPercent"] = int64(*b.ReservedPercent)
		}
		backends[b.Name] = entry
	}

	vals := map[string]any{
		"backends": backends,
		"logLevel": defaultString(cr.Spec.LogLevel, "info"),
		"image": map[string]any{
			"repository": defaultString(cr.Spec.Image.Repository, "ghcr.io/piwi3910/truenas-csi"),
			"tag":        cr.Spec.Image.Tag,
			"pullPolicy": defaultString(cr.Spec.Image.PullPolicy, "IfNotPresent"),
		},
		"controller": map[string]any{
			"replicas": int64(defaultInt32(cr.Spec.Controller.Replicas, 2)),
		},
		"node": map[string]any{
			"kubeletDir": defaultString(cr.Spec.Node.KubeletDir, "/var/lib/kubelet"),
			// The operator, not the DaemonSet controller, decides when a node's
			// plugin pod may be replaced.
			"updateStrategy": map[string]any{"type": "OnDelete"},
		},
		"snapshotter": map[string]any{
			"enabled": cr.Spec.Snapshotter.Enabled,
			"install": false,
		},
		"volumeSnapshotClass": map[string]any{
			"enabled":        cr.Spec.VolumeSnapshotClass.Enabled,
			"name":           cr.Spec.VolumeSnapshotClass.Name,
			"isDefault":      cr.Spec.VolumeSnapshotClass.IsDefault,
			"deletionPolicy": defaultString(cr.Spec.VolumeSnapshotClass.DeletionPolicy, "Delete"),
		},
		"storageClasses": storageClassValues(cr),
	}

	if len(cr.Spec.Image.PullSecrets) > 0 {
		refs := make([]any, 0, len(cr.Spec.Image.PullSecrets))
		for _, n := range cr.Spec.Image.PullSecrets {
			refs = append(refs, map[string]any{"name": n})
		}
		vals["imagePullSecrets"] = refs
	}
	if len(cr.Spec.Controller.NodeSelector) > 0 {
		vals["controller"].(map[string]any)["nodeSelector"] = toAnyMap(cr.Spec.Controller.NodeSelector)
	}
	if len(cr.Spec.Node.NodeSelector) > 0 {
		vals["node"].(map[string]any)["nodeSelector"] = toAnyMap(cr.Spec.Node.NodeSelector)
	}
	return vals, nil
}

// storageClassValues maps the CR's list of StorageClasses onto the chart's map.
// The chart ranges over whatever keys it is given, so an arbitrary number of
// classes renders without touching a template.
func storageClassValues(cr *truenasv1alpha1.TrueNASCSIDriver) map[string]any {
	classes := map[string]any{}
	for _, sc := range cr.Spec.StorageClasses {
		params := map[string]any{
			"backend":  sc.Backend,
			"protocol": sc.Protocol,
		}
		if sc.Pool != "" {
			params["pool"] = sc.Pool
		}
		if sc.ParentDataset != "" {
			params["parentDataset"] = sc.ParentDataset
		}
		if sc.NFS != nil {
			putIf(params, "nfsVersion", sc.NFS.Version)
			if len(sc.NFS.Networks) > 0 {
				params["networks"] = strings.Join(sc.NFS.Networks, ",")
			}
			putIf(params, "mode", sc.NFS.Mode)
			if sc.NFS.UID != nil {
				params["uid"] = fmt.Sprintf("%d", *sc.NFS.UID)
			}
			if sc.NFS.GID != nil {
				params["gid"] = fmt.Sprintf("%d", *sc.NFS.GID)
			}
		}
		if sc.ISCSI != nil {
			putIf(params, "fsType", sc.ISCSI.FSType)
			putBool(params, "sparse", sc.ISCSI.Sparse)
			if sc.ISCSI.PortalID != nil && *sc.ISCSI.PortalID > 0 {
				params["portalID"] = fmt.Sprintf("%d", *sc.ISCSI.PortalID)
			}
			putBool(params, "chap", sc.ISCSI.CHAP)
			putBool(params, "initiatorACL", sc.ISCSI.InitiatorACL)
			putBool(params, "multipath", sc.ISCSI.Multipath)
		}

		mountOptions := make([]any, 0, len(sc.MountOptions))
		for _, o := range sc.MountOptions {
			mountOptions = append(mountOptions, o)
		}
		expansion := true
		if sc.AllowVolumeExpansion != nil {
			expansion = *sc.AllowVolumeExpansion
		}
		classes[sc.Name] = map[string]any{
			"enabled":              true,
			"name":                 sc.Name,
			"isDefault":            sc.IsDefault,
			"reclaimPolicy":        defaultString(sc.ReclaimPolicy, "Delete"),
			"allowVolumeExpansion": expansion,
			"volumeBindingMode":    defaultString(sc.VolumeBindingMode, "Immediate"),
			"mountOptions":         mountOptions,
			"parameters":           params,
		}
	}
	return classes
}

// Render renders the chart and returns the objects it produced, sorted into a
// stable order so that two renders of the same spec produce the same slice.
func Render(ch *chart.Chart, cr *truenasv1alpha1.TrueNASCSIDriver, creds Credentials, kube KubeVersion) ([]*unstructured.Unstructured, error) {
	vals, err := Values(cr, creds)
	if err != nil {
		return nil, err
	}
	if kube.Version == "" {
		kube = DefaultKubeVersion
	}
	caps := &chartutil.Capabilities{
		KubeVersion: chartutil.KubeVersion{Version: kube.Version, Major: kube.Major, Minor: kube.Minor},
		APIVersions: chartutil.DefaultVersionSet,
		HelmVersion: chartutil.DefaultCapabilities.HelmVersion,
	}
	opts := chartutil.ReleaseOptions{
		Name:      DefaultReleaseName,
		Namespace: namespaceOf(cr),
		IsInstall: true,
	}
	rendered, err := chartutil.ToRenderValues(ch, vals, opts, caps)
	if err != nil {
		return nil, fmt.Errorf("build render values: %w", err)
	}
	files, err := engine.Render(ch, rendered)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var objs []*unstructured.Unstructured
	for _, name := range names {
		base := path.Base(name)
		if strings.HasPrefix(base, "_") || strings.EqualFold(base, "NOTES.txt") {
			continue
		}
		if strings.TrimSpace(files[name]) == "" {
			continue
		}
		parsed, err := decodeManifests(files[name])
		if err != nil {
			return nil, fmt.Errorf("parse rendered %s: %w", name, err)
		}
		objs = append(objs, parsed...)
	}
	sortObjects(objs)
	return objs, nil
}

// decodeManifests splits a rendered file into objects, dropping empty documents.
func decodeManifests(manifest string) ([]*unstructured.Unstructured, error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewBufferString(manifest), 4096)
	var out []*unstructured.Unstructured
	for {
		raw := map[string]any{}
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			continue
		}
		u := &unstructured.Unstructured{Object: raw}
		if u.GetKind() == "" || u.GetAPIVersion() == "" {
			return nil, fmt.Errorf("rendered document has no kind or apiVersion")
		}
		out = append(out, u)
	}
	return out, nil
}

// applyOrder puts an object's kind in the order it must be applied. A Secret
// that lands after the Deployment that mounts it costs a crash loop and a
// restart; a CSIDriver that lands after the DaemonSet costs a failed
// registration.
var applyOrder = map[string]int{
	"Namespace":            0,
	"ServiceAccount":       1,
	"Secret":               2,
	"ConfigMap":            3,
	"ClusterRole":          4,
	"ClusterRoleBinding":   5,
	"Role":                 6,
	"RoleBinding":          7,
	"CSIDriver":            8,
	"StorageClass":         9,
	"VolumeSnapshotClass":  10,
	"Service":              11,
	"Deployment":           12,
	"DaemonSet":            13,
	"PodDisruptionBudget":  14,
	"PriorityClass":        15,
	"MutatingWebhookConfg": 16,
}

func sortObjects(objs []*unstructured.Unstructured) {
	sort.SliceStable(objs, func(i, j int) bool {
		oi, ok := applyOrder[objs[i].GetKind()]
		if !ok {
			oi = 50
		}
		oj, ok := applyOrder[objs[j].GetKind()]
		if !ok {
			oj = 50
		}
		if oi != oj {
			return oi < oj
		}
		if objs[i].GetKind() != objs[j].GetKind() {
			return objs[i].GetKind() < objs[j].GetKind()
		}
		return objs[i].GetName() < objs[j].GetName()
	})
}

func namespaceOf(cr *truenasv1alpha1.TrueNASCSIDriver) string {
	if cr.Spec.Namespace != "" {
		return cr.Spec.Namespace
	}
	return "truenas-csi"
}

// Namespace is the namespace the driver's workloads are installed into.
func Namespace(cr *truenasv1alpha1.TrueNASCSIDriver) string { return namespaceOf(cr) }

func defaultString(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defaultInt32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func putIf(m map[string]any, key, value string) {
	if value != "" {
		m[key] = value
	}
}

func putBool(m map[string]any, key string, value *bool) {
	if value != nil {
		m[key] = fmt.Sprintf("%t", *value)
	}
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
