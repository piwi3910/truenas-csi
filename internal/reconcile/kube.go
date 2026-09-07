package reconcile

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// KubePVLister reads PersistentVolume handles from the API server.
type KubePVLister struct {
	client     kubernetes.Interface
	driverName string
}

// NewKubePVLister builds a lister from the pod's in-cluster credentials.
func NewKubePVLister(driverName string) (*KubePVLister, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return &KubePVLister{client: c, driverName: driverName}, nil
}

// VolumeHandles returns every volume handle belonging to this driver.
//
// Paging matters: a truncated list read as complete would make live volumes
// look orphaned, which is why the reconciler treats any error here as "report
// nothing" rather than "everything is an orphan".
func (l *KubePVLister) VolumeHandles(ctx context.Context) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	opts := metav1.ListOptions{Limit: 500}
	for {
		pvs, err := l.client.CoreV1().PersistentVolumes().List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PersistentVolumes: %w", err)
		}
		for i := range pvs.Items {
			csi := pvs.Items[i].Spec.CSI
			if csi == nil || csi.Driver != l.driverName {
				continue
			}
			out[csi.VolumeHandle] = struct{}{}
		}
		if pvs.Continue == "" {
			return out, nil
		}
		opts.Continue = pvs.Continue
	}
}
