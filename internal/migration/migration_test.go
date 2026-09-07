package migration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/piwi3910/truenas-csi/internal/config"
	"github.com/piwi3910/truenas-csi/internal/truenas"
	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
	"github.com/piwi3910/truenas-csi/internal/volume"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kfake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

const (
	targetHandle = "nas1/nfs/Pool0/k8s/pvc-target"
	ownedTarget  = `[{"id":"Pool0/k8s/pvc-target","type":"FILESYSTEM",
	  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"LOCAL"}}}]`
	// The marker is present but INHERITED from the parent dataset: this is a
	// dataset the driver did NOT create, and writing into it would overwrite
	// the operator's real data.
	inheritedTarget = `[{"id":"Pool0/k8s/pvc-target","type":"FILESYSTEM",
	  "user_properties":{"io.truenas.csi:managed":{"value":"truenas-csi","source":"INHERITED"}}}]`
)

func backendCfg(url string) config.Backend {
	return config.Backend{
		Name: "nas1", Endpoint: url, Username: "truenas_admin", APIKey: "8-x",
		Pool: "Pool0", ParentDataset: "k8s", InsecureSkipVerify: true,
	}
}

func datasets(t *testing.T, body string) func([]json.RawMessage) (any, error) {
	t.Helper()
	return func([]json.RawMessage) (any, error) {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		return v, nil
	}
}

func claim(ns, name, volName string, gib int64) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:  volName,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(gib<<30, resource.BinarySI),
			},
		},
	}
}

func vol(name, driver, handle string, gib int64) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(gib<<30, resource.BinarySI),
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: driver, VolumeHandle: handle},
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
}

// cluster returns a fake API server holding a Longhorn source PVC and a
// driver-managed target PVC, both bound.
func cluster() kubernetes.Interface {
	return kfake.NewSimpleClientset(
		claim("apps", "data-src", "pv-src", 1),
		vol("pv-src", "driver.longhorn.io", "longhorn-vol-1", 1),
		claim("apps", "data-dst", "pv-dst", 2),
		vol("pv-dst", "csi.truenas.watteel.com", targetHandle, 2),
	)
}

func migrator(t *testing.T, body string, kube kubernetes.Interface) (*Migrator, *fake.Server) {
	t.Helper()
	s := fake.Start(t, fake.Options{})
	s.Handle("pool.dataset.query", datasets(t, body))
	c, err := truenas.Dial(context.Background(), backendCfg(s.URL()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return New(kube, c, backendCfg(s.URL())), s
}

func req() Request {
	return Request{SourceNamespace: "apps", SourcePVC: "data-src", TargetPVC: "data-dst"}
}

// TestMigrationRefusesUnownedTarget catches the break where migration would copy
// into a dataset this driver did not create, overwriting real user data.
func TestMigrationRefusesUnownedTarget(t *testing.T) {
	kube := cluster()
	m, _ := migrator(t, inheritedTarget, kube)

	_, err := m.Plan(context.Background(), req())
	if err == nil {
		t.Fatal("Plan must refuse a target this driver does not own")
	}
	if !errors.Is(err, volume.ErrNotManaged) {
		t.Fatalf("want volume.ErrNotManaged, got %v", err)
	}
	for _, a := range kube.(*kfake.Clientset).Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			t.Fatal("no copy Job may be created for an unowned target")
		}
	}
}

// TestMigrationMountsSourceReadOnly catches the break where a migration could
// modify the volume it is copying from.
func TestMigrationMountsSourceReadOnly(t *testing.T) {
	m, _ := migrator(t, ownedTarget, cluster())
	p, err := m.Plan(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	spec := p.Job.Spec.Template.Spec

	var srcVol, dstVol *corev1.Volume
	for i := range spec.Volumes {
		switch spec.Volumes[i].Name {
		case SourceVolumeName:
			srcVol = &spec.Volumes[i]
		case TargetVolumeName:
			dstVol = &spec.Volumes[i]
		}
	}
	if srcVol == nil || dstVol == nil {
		t.Fatalf("job must mount both volumes, got %+v", spec.Volumes)
	}
	if srcVol.PersistentVolumeClaim == nil || !srcVol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("source PVC volume must be readOnly: %+v", srcVol.PersistentVolumeClaim)
	}
	if dstVol.PersistentVolumeClaim == nil || dstVol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("target PVC volume must be writable: %+v", dstVol.PersistentVolumeClaim)
	}

	if len(spec.Containers) != 1 {
		t.Fatalf("want exactly one container, got %d", len(spec.Containers))
	}
	var srcMount, dstMount *corev1.VolumeMount
	for i, mnt := range spec.Containers[0].VolumeMounts {
		switch mnt.Name {
		case SourceVolumeName:
			srcMount = &spec.Containers[0].VolumeMounts[i]
		case TargetVolumeName:
			dstMount = &spec.Containers[0].VolumeMounts[i]
		}
	}
	if srcMount == nil || !srcMount.ReadOnly || srcMount.MountPath != SourceMountPath {
		t.Errorf("source mount must be readOnly at %s: %+v", SourceMountPath, srcMount)
	}
	if dstMount == nil || dstMount.ReadOnly || dstMount.MountPath != TargetMountPath {
		t.Errorf("target mount must be writable at %s: %+v", TargetMountPath, dstMount)
	}
}

// TestMigrationVerifiesChecksum catches the break where a partial copy is
// reported as complete.
func TestMigrationVerifiesChecksum(t *testing.T) {
	const src = `aaa  a/one.txt
bbb  b/two.txt
ccc  three.txt
`
	full, err := ParseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Files) != 3 {
		t.Fatalf("want 3 files, got %d", len(full.Files))
	}
	if err := Verify(full, full); err != nil {
		t.Fatalf("identical manifests must verify: %v", err)
	}

	partial, err := ParseManifest(strings.NewReader("aaa  a/one.txt\nbbb  b/two.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = Verify(full, partial)
	if err == nil {
		t.Fatal("a target missing a file must fail verification")
	}
	if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), "three.txt") {
		t.Fatalf("error must name the missing file: %v", err)
	}

	corrupt, err := ParseManifest(strings.NewReader("aaa  a/one.txt\nbbb  b/two.txt\nzzz  three.txt\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(full, corrupt); err == nil {
		t.Fatal("a target with a differing checksum must fail verification")
	}

	// The same rule applied to the summary the copy Job reports back.
	if err := (&Summary{SourceFiles: 3, TargetFiles: 2, SourceDigest: "x", TargetDigest: "y",
		Verified: false}).Check(); err == nil {
		t.Fatal("a summary reporting fewer target files must fail")
	}
	if err := (&Summary{SourceFiles: 3, TargetFiles: 3, SourceDigest: "x", TargetDigest: "x",
		Verified: true}).Check(); err != nil {
		t.Fatalf("a matching summary must pass: %v", err)
	}
	// A job that exits 0 without reporting a summary is NOT verified.
	var none *Summary
	if err := none.Check(); err == nil {
		t.Fatal("a missing summary must never count as verified")
	}
}

// TestMigrationIsResumable catches the break where re-running a failed
// migration starts from scratch or fails outright.
func TestMigrationIsResumable(t *testing.T) {
	kube := cluster()
	m, _ := migrator(t, ownedTarget, kube)
	p, err := m.Plan(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}

	script := strings.Join(p.Job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"rsync", "-aHAX", "--numeric-ids", "--partial", "tar"} {
		if !strings.Contains(script, want) {
			t.Errorf("copy script must contain %q:\n%s", want, script)
		}
	}

	if _, err := m.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(context.Background(), p); err != nil {
		t.Fatalf("Run must be safe to call again after a failure: %v", err)
	}

	creates := 0
	for _, a := range kube.(*kfake.Clientset).Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "jobs" {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("re-running must adopt the existing Job, got %d creates", creates)
	}
}

// TestMigrationNeverDeletesSource catches the break where migration reclaims the
// volume it copied from. Retiring the old volume is the operator's decision.
func TestMigrationNeverDeletesSource(t *testing.T) {
	kube := cluster()
	m, s := migrator(t, ownedTarget, kube)
	p, err := m.Plan(context.Background(), req())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := m.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.SourceRetained {
		t.Error("report must state the source was retained")
	}

	for _, a := range kube.(*kfake.Clientset).Actions() {
		if a.GetVerb() == "delete" || a.GetVerb() == "deletecollection" {
			t.Fatalf("migration must never delete anything: %v %v", a.GetVerb(), a.GetResource())
		}
		if u, ok := a.(ktesting.UpdateAction); ok {
			if a.GetResource().Resource == "persistentvolumes" ||
				a.GetResource().Resource == "persistentvolumeclaims" {
				t.Fatalf("migration must not modify source/target claims: %v", u.GetObject())
			}
		}
	}
	for _, c := range s.Calls() {
		if strings.Contains(c, "delete") || strings.Contains(c, "destroy") {
			t.Fatalf("migration must issue no destructive middleware call, saw %q", c)
		}
	}
	script := strings.Join(p.Job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, forbidden := range []string{"--delete", "rm -", "mkfs", "shred"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("copy script must not contain %q", forbidden)
		}
	}
}
