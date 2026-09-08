package obs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Leader-election timings. These are client-go's own defaults and there is no
// reason to be cleverer: array metrics are polled on a minute-scale interval,
// so a lease handover that takes a few seconds costs at most one sample.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// LeaderConfig describes one leader election.
type LeaderConfig struct {
	// Client talks to the API server that holds the Lease.
	Client kubernetes.Interface
	// Namespace and Name locate the coordination.k8s.io Lease.
	Namespace string
	Name      string
	// Identity distinguishes this candidate; it must be unique per replica.
	Identity string

	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// InClusterLeaderConfig builds a LeaderConfig for a named lease from the pod's
// own credentials and downward-API environment.
//
// The identity is the pod name, which Kubernetes guarantees is unique in the
// namespace at any instant. Falling back to the hostname keeps this usable
// outside a Deployment; falling back to a random string would leave a dead
// replica's lease held by a name nobody can trace back to a pod.
func InClusterLeaderConfig(name string) (LeaderConfig, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return LeaderConfig{}, fmt.Errorf("in-cluster config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return LeaderConfig{}, fmt.Errorf("kubernetes client: %w", err)
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		return LeaderConfig{}, errors.New("POD_NAMESPACE is not set: cannot locate the lease")
	}
	id := os.Getenv("POD_NAME")
	if id == "" {
		if h, hErr := os.Hostname(); hErr == nil {
			id = h
		}
	}
	if id == "" {
		return LeaderConfig{}, errors.New("neither POD_NAME nor a hostname is available for a lease identity")
	}
	return LeaderConfig{Client: client, Namespace: ns, Name: name, Identity: id}, nil
}

func (c LeaderConfig) withDefaults() LeaderConfig {
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = DefaultLeaseDuration
	}
	if c.RenewDeadline <= 0 {
		c.RenewDeadline = DefaultRenewDeadline
	}
	if c.RetryPeriod <= 0 {
		c.RetryPeriod = DefaultRetryPeriod
	}
	return c
}

// RunLeader contends for the lease and runs onStarted, in a context of its own,
// for as long as this replica holds it. onStopped is called when leadership is
// lost or the context ends. It blocks until ctx is done.
//
// ReleaseOnCancel is on: a controller shutting down hands the lease back
// immediately instead of leaving the survivors waiting out a full lease
// duration before anything polls the appliance again.
func RunLeader(ctx context.Context, cfg LeaderConfig, onStarted func(context.Context), onStopped func()) error {
	cfg = cfg.withDefaults()
	if cfg.Client == nil {
		return errors.New("leader election needs a Kubernetes client")
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: cfg.Name, Namespace: cfg.Namespace},
		Client:    cfg.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}
	el, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.RetryPeriod,
		ReleaseOnCancel: true,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leaderCtx context.Context) {
				if onStarted != nil {
					onStarted(leaderCtx)
				}
			},
			OnStoppedLeading: func() {
				if onStopped != nil {
					onStopped()
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election for %s/%s: %w", cfg.Namespace, cfg.Name, err)
	}
	el.Run(ctx)
	return ctx.Err()
}
