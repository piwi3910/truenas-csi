package obs

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
)

// stubSource answers the collector from a map a test controls, standing in for
// the node's procfs reader.
type stubSource struct {
	samples map[string]VolumeIOSample
	calls   int
}

func (s *stubSource) sample(volumeID string) (VolumeIOSample, bool) {
	s.calls++
	v, ok := s.samples[volumeID]
	return v, ok
}

// gather renders the collector's output in the Prometheus text format, which
// is what an operator's scrape actually sees.
func gather(t *testing.T, c *VolumeIOCollector) string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var b strings.Builder
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode %s: %v", mf.GetName(), err)
		}
	}
	return b.String()
}

// TestVolumeIOMetricsCarryTheWorkloadLabels pins where each label comes from:
// the volume handle at stage, the pod at publish. The pod half is the whole
// point of the exercise — without it nobody can answer "which workload is
// causing this".
func TestVolumeIOMetricsCarryTheWorkloadLabels(t *testing.T) {
	src := &stubSource{samples: map[string]VolumeIOSample{
		"nas1/nfs/tank/k8s/pvc-abc": {
			ReadOps: 10, WriteOps: 4,
			ReadBytes: 4096, WriteBytes: 8192,
			ReadSeconds: 1.5, WriteSeconds: 2.5,
			HasOps: true, HasLatency: true,
		},
	}}
	c := NewVolumeIOCollector(src.sample)
	c.Track(VolumeIOLabels{
		VolumeID:         "nas1/nfs/tank/k8s/pvc-abc",
		PersistentVolume: "pvc-abc",
		Protocol:         "nfs",
	})
	c.Attach("nas1/nfs/tank/k8s/pvc-abc", "prod", "postgres-0", "")

	out := gather(t, c)
	for _, want := range []string{
		`truenas_csi_volume_read_bytes_total{namespace="prod",` +
			`persistentvolume="pvc-abc",pod="postgres-0",protocol="nfs",pvc="",` +
			`volume_id_hash="` + HashVolumeID("nas1/nfs/tank/k8s/pvc-abc") + `"} 4096`,
		"truenas_csi_volume_write_bytes_total",
		"truenas_csi_volume_read_ops_total",
		"truenas_csi_volume_read_seconds_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
	// No block device, so no busy time — and it must be absent rather than a
	// flat zero, which would read as "never busy" instead of "not measured".
	if strings.Contains(out, "truenas_csi_volume_io_busy_seconds_total") {
		t.Fatalf("an NFS volume must not report device busy time:\n%s", out)
	}
}

// TestUnstageRetiresTheSeries is the lifecycle guarantee. A volume that has
// been unstaged is gone from this node; leaving its series behind would mean
// every node that ever hosted a volume reports it for ever, and a dashboard
// summing across nodes would count the same volume many times.
func TestUnstageRetiresTheSeries(t *testing.T) {
	src := &stubSource{samples: map[string]VolumeIOSample{
		"nas1/iscsi/tank/k8s/pvc-1": {ReadBytes: 1},
		"nas1/iscsi/tank/k8s/pvc-2": {ReadBytes: 2},
	}}
	c := NewVolumeIOCollector(src.sample)
	c.Track(VolumeIOLabels{VolumeID: "nas1/iscsi/tank/k8s/pvc-1", PersistentVolume: "pvc-1"})
	c.Track(VolumeIOLabels{VolumeID: "nas1/iscsi/tank/k8s/pvc-2", PersistentVolume: "pvc-2"})
	if got := strings.Count(gather(t, c), "truenas_csi_volume_read_bytes_total{"); got != 2 {
		t.Fatalf("two staged volumes must produce two series, got %d", got)
	}

	c.Forget("nas1/iscsi/tank/k8s/pvc-1")
	out := gather(t, c)
	if strings.Contains(out, `persistentvolume="pvc-1"`) {
		t.Fatalf("an unstaged volume must stop being exported:\n%s", out)
	}
	if !strings.Contains(out, `persistentvolume="pvc-2"`) {
		t.Fatalf("the volume still staged must keep reporting:\n%s", out)
	}
	if c.Tracked() != 1 {
		t.Fatalf("Tracked() = %d, want 1", c.Tracked())
	}
}

// TestStageAfterPublishKeepsTheWorkloadLabels covers the kubelet replaying
// NodeStageVolume for a volume that is already published — after a plugin
// restart, say. Stage knows nothing about the pod, and must not blank what
// publish established.
func TestStageAfterPublishKeepsTheWorkloadLabels(t *testing.T) {
	src := &stubSource{samples: map[string]VolumeIOSample{"v": {ReadBytes: 1}}}
	c := NewVolumeIOCollector(src.sample)
	c.Track(VolumeIOLabels{VolumeID: "v", PersistentVolume: "pvc-x", Protocol: "iscsi"})
	c.Attach("v", "prod", "web-0", "web-data")
	c.Track(VolumeIOLabels{VolumeID: "v", PersistentVolume: "pvc-x", Protocol: "iscsi"})

	out := gather(t, c)
	for _, want := range []string{`namespace="prod"`, `pod="web-0"`, `pvc="web-data"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("a replayed stage must not lose %s:\n%s", want, out)
		}
	}
}

// TestUnreadableVolumeExportsNothing pins that an unreadable volume produces no
// series at all. Exporting zeros would say "this volume is idle", which is a
// different — and load-bearing — claim from "this volume could not be read".
func TestUnreadableVolumeExportsNothing(t *testing.T) {
	src := &stubSource{samples: map[string]VolumeIOSample{}}
	c := NewVolumeIOCollector(src.sample)
	c.Track(VolumeIOLabels{VolumeID: "v", PersistentVolume: "pvc-x", Protocol: "nfs"})
	if out := gather(t, c); strings.Contains(out, "truenas_csi_volume_read_bytes_total{") {
		t.Fatalf("an unreadable volume must export nothing:\n%s", out)
	}
	if src.calls == 0 {
		t.Fatal("the collector must actually have asked the source")
	}
}

// TestCounterValuesArePassedThroughUnmodified pins that the collector never
// smooths or clamps: a pod rescheduling onto this node restarts the kernel's
// counters, and Prometheus must see that reset rather than a fabricated
// monotonic line.
func TestCounterValuesArePassedThroughUnmodified(t *testing.T) {
	src := &stubSource{samples: map[string]VolumeIOSample{"v": {ReadBytes: 5000}}}
	c := NewVolumeIOCollector(src.sample)
	c.Track(VolumeIOLabels{VolumeID: "v", PersistentVolume: "pvc-x"})
	if out := gather(t, c); !strings.Contains(out, "} 5000") {
		t.Fatalf("want the raw value:\n%s", out)
	}
	src.samples["v"] = VolumeIOSample{ReadBytes: 7} // remounted, counters reset
	if out := gather(t, c); !strings.Contains(out, "} 7") {
		t.Fatalf("a reset must be reported as it stands:\n%s", out)
	}
}
