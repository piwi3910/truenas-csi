package obs

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestRunLeaderRunsTheWorkOnlyWhileItLeads covers the contract array-metrics
// polling depends on: the work starts when the lease is acquired, and it stops
// when leadership goes away. A replica that kept polling after losing the lease
// would put two pollers on the same appliance, which is the exact load the
// election exists to prevent.
func TestRunLeaderRunsTheWorkOnlyWhileItLeads(t *testing.T) {
	client := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{}, 1)
	workEnded := make(chan struct{}, 1)
	stopped := make(chan struct{}, 1)

	done := make(chan error, 1)
	go func() {
		done <- RunLeader(ctx, LeaderConfig{
			Client:        client,
			Namespace:     "kube-system",
			Name:          "truenas-csi-array-metrics",
			Identity:      "controller-0",
			LeaseDuration: 2 * time.Second,
			RenewDeadline: time.Second,
			RetryPeriod:   100 * time.Millisecond,
		}, func(leaderCtx context.Context) {
			started <- struct{}{}
			<-leaderCtx.Done() // this is what the collector's Run does
			workEnded <- struct{}{}
		}, func() {
			stopped <- struct{}{}
		})
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("leadership was never acquired against an empty lease")
	}

	lease, err := client.CoordinationV1().Leases("kube-system").
		Get(ctx, "truenas-csi-array-metrics", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the lease was not created: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "controller-0" {
		t.Fatalf("lease holder = %v, want controller-0", lease.Spec.HolderIdentity)
	}

	cancel()
	for _, c := range []struct {
		name string
		ch   chan struct{}
	}{{"the leader-scoped work", workEnded}, {"the OnStoppedLeading callback", stopped}} {
		select {
		case <-c.ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never ran after leadership ended", c.name)
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunLeader did not return when its context was cancelled")
	}
}

func TestRunLeaderNeedsAClient(t *testing.T) {
	if err := RunLeader(context.Background(), LeaderConfig{Namespace: "x", Name: "y"}, nil, nil); err == nil {
		t.Fatal("leader election without a client must fail loudly, not silently do nothing")
	}
}

func TestInClusterLeaderConfigFailsOutsideACluster(t *testing.T) {
	// There is no service account token here, so this must report rather than
	// panic or return a config that would elect nobody.
	if _, err := InClusterLeaderConfig("truenas-csi-array-metrics"); err == nil {
		t.Skip("running inside a cluster; nothing to assert")
	}
}
