package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	truenasv1alpha1 "github.com/piwi3910/truenas-csi/operator/api/v1alpha1"
)

// envtestAssets returns the kube-apiserver/etcd binaries envtest needs, or "".
//
// The whole suite must run on a laptop with no cluster and no downloaded
// binaries, so the API-server-backed checks are additive: the schema tests in
// crd_validation_test.go already assert the same rules against the generated
// CRD, and these run on top when the assets happen to be there.
func envtestAssets() string {
	if p := os.Getenv("KUBEBUILDER_ASSETS"); p != "" {
		return p
	}
	return ""
}

func startEnvtest(t *testing.T) client.Client {
	t.Helper()
	assets := envtestAssets()
	if assets == "" {
		t.Skip("SKIP: envtest assets unavailable (set KUBEBUILDER_ASSETS, e.g. `make envtest`); the schema-level checks in crd_validation_test.go still ran")
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	scheme := runtime.NewScheme()
	if err := truenasv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

func sampleDriver(name, endpoint string) *truenasv1alpha1.TrueNASCSIDriver {
	return &truenasv1alpha1.TrueNASCSIDriver{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: truenasv1alpha1.TrueNASCSIDriverSpec{
			Namespace: "truenas-csi",
			Backends: []truenasv1alpha1.BackendSpec{{
				Name:            "nas1",
				Endpoint:        endpoint,
				Username:        "csi",
				APIKeySecretRef: truenasv1alpha1.SecretKeyReference{Name: "truenas-credentials", Key: "apiKey"},
				Pool:            "tank",
				ParentDataset:   "tank/k8s",
			}},
		},
	}
}

// TestCRDRejectsPlaintextEndpointOnAPIServer is the same rule as
// TestCRDRejectsPlaintextEndpoint, enforced by a real API server.
func TestCRDRejectsPlaintextEndpointOnAPIServer(t *testing.T) {
	c := startEnvtest(t)
	ctx := context.Background()

	bad := sampleDriver("plaintext", "ws://nas1.example.com/api/current")
	err := c.Create(ctx, bad)
	if err == nil {
		_ = c.Delete(ctx, bad)
		t.Fatal("the API server accepted a ws:// endpoint; presenting an API key over plaintext revokes it on TrueNAS 25.10")
	}
	if !strings.Contains(err.Error(), "endpoint") {
		t.Errorf("rejection did not mention the endpoint, so the operator would be hard to diagnose: %v", err)
	}

	good := sampleDriver("secure", "wss://nas1.example.com/api/current")
	if err := c.Create(ctx, good); err != nil {
		t.Fatalf("the API server rejected a legitimate wss:// endpoint: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, good) })
}

// TestCRDRejectsInlineAPIKeyOnAPIServer checks that a credential typed into the
// resource is pruned rather than persisted.
func TestCRDRejectsInlineAPIKeyOnAPIServer(t *testing.T) {
	c := startEnvtest(t)
	ctx := context.Background()

	raw := map[string]any{
		"apiVersion": truenasv1alpha1.GroupVersion.String(),
		"kind":       "TrueNASCSIDriver",
		"metadata":   map[string]any{"name": "inline-key"},
		"spec": map[string]any{
			"namespace": "truenas-csi",
			"backends": []any{map[string]any{
				"name":            "nas1",
				"endpoint":        "wss://nas1.example.com/api/current",
				"username":        "csi",
				"apiKey":          "1-should-never-be-stored",
				"apiKeySecretRef": map[string]any{"name": "truenas-credentials", "key": "apiKey"},
				"pool":            "tank",
				"parentDataset":   "tank/k8s",
			}},
		},
	}
	obj := &unstructured.Unstructured{Object: raw}
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create with an inline apiKey failed for an unrelated reason: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(ctx, obj) })

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(obj.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKey{Name: "inline-key"}, got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	backends, _ := got.Object["spec"].(map[string]any)["backends"].([]any)
	if len(backends) != 1 {
		t.Fatalf("expected one backend, got %d", len(backends))
	}
	if _, present := backends[0].(map[string]any)["apiKey"]; present {
		t.Error("the API server stored an inline apiKey; credentials must only ever be referenced")
	}
}
