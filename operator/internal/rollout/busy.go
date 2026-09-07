package rollout

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

// DriverPVCKey identifies a PersistentVolumeClaim as "namespace/name".
func DriverPVCKey(namespace, name string) string { return namespace + "/" + name }

// BusyNodes reports, per node, why that node is not clear to have its plugin
// replaced.
//
// A node is busy when a pod on it is in the middle of acquiring or releasing one
// of this driver's volumes:
//
//   - the pod is terminating and still has one of our volumes, so an unstage is
//     either running or about to. Deleting the plugin now strands an iSCSI
//     session and a mount that nothing knows how to clean up.
//   - the pod is Pending with one of our volumes, so a stage/publish is in
//     flight. Deleting the plugin fails that call.
//
// A pod that is simply Running with a mounted volume is *not* busy: the volume
// is staged, nothing is in flight, and the plugin can be replaced under it. That
// is the ordinary case, and treating it as busy would mean the rollout never
// finishes on a cluster that is actually using its storage.
func BusyNodes(pods []corev1.Pod, ourPVCs map[string]bool) map[string]string {
	busy := map[string]string{}
	ordered := make([]corev1.Pod, len(pods))
	copy(ordered, pods)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Namespace != ordered[j].Namespace {
			return ordered[i].Namespace < ordered[j].Namespace
		}
		return ordered[i].Name < ordered[j].Name
	})

	for _, p := range ordered {
		node := p.Spec.NodeName
		if node == "" {
			continue
		}
		if _, already := busy[node]; already {
			continue
		}
		claim := firstDriverClaim(p, ourPVCs)
		if claim == "" {
			continue
		}
		switch {
		case p.DeletionTimestamp != nil:
			busy[node] = fmt.Sprintf("pod %s/%s is terminating with volume %s still staged",
				p.Namespace, p.Name, claim)
		case p.Status.Phase == corev1.PodPending:
			busy[node] = fmt.Sprintf("pod %s/%s is still acquiring volume %s",
				p.Namespace, p.Name, claim)
		}
	}
	return busy
}

func firstDriverClaim(p corev1.Pod, ourPVCs map[string]bool) string {
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim == nil {
			continue
		}
		key := DriverPVCKey(p.Namespace, v.PersistentVolumeClaim.ClaimName)
		if ourPVCs[key] {
			return key
		}
	}
	return ""
}
