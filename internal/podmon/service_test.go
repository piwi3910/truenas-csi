package podmon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stalledDriver stands in for the CSI driver having wedged: every call blocks
// until the test ends. Podmon exists precisely for this state, so nothing it
// does may depend on such a call returning.
type stalledDriver struct {
	release chan struct{}
	calls   chan struct{}
}

func newStalledDriver() *stalledDriver {
	return &stalledDriver{release: make(chan struct{}), calls: make(chan struct{}, 16)}
}

func (d *stalledDriver) probe(context.Context) error {
	select {
	case d.calls <- struct{}{}:
	default:
	}
	<-d.release
	return nil
}

func (d *stalledDriver) Close() { close(d.release) }

func healthyService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	s := New("worker-1", "nas1", "192.0.2.10:2049")
	s.Timeout = 100 * time.Millisecond
	s.Dial = func(context.Context, string) error { return nil }
	s.Statfs = func(string) error { return nil }
	s.Volumes = func(id string) (VolumeRef, bool) {
		return VolumeRef{VolumeID: id, Protocol: "nfs", Path: dir}, true
	}
	s.LastIO = func(string) (time.Time, bool) { return time.Now(), true }
	return s
}

// TestPodmonAnswersWhileDriverStalled is the reason this service exists at all.
// An independent health checker that shares the stalled driver's fate is not a
// health checker.
func TestPodmonAnswersWhileDriverStalled(t *testing.T) {
	driver := newStalledDriver()
	defer driver.Close()

	s := healthyService(t)
	s.DriverProbe = driver.probe

	type result struct {
		resp *Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := s.ValidateVolumeHostConnectivity(context.Background(), &Request{
			NodeID:         "worker-1",
			VolumeIDs:      []string{"nas1/nfs/tank/k8s/pvc-a"},
			IOSampleWindow: time.Minute,
		})
		done <- result{resp, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("ValidateVolumeHostConnectivity: %v", r.err)
		}
		if !r.resp.Connected {
			t.Errorf("the node's own checks all passed, so it is connected: %+v", r.resp)
		}
		if !r.resp.IOsInProgress {
			t.Error("recent I/O was observed, so IOsInProgress must be true")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("podmon blocked on the stalled driver: its checks and timers must " +
			"be its own, or it dies with the process it is meant to watch")
	}
}

// TestPodmonReportsUnreachableBackend: the appliance dial is the node-level
// signal, and a failure must be reported as such, named, rather than swallowed.
func TestPodmonReportsUnreachableBackend(t *testing.T) {
	s := healthyService(t)
	s.Dial = func(context.Context, string) error { return errors.New("connection refused") }

	resp, err := s.ValidateVolumeHostConnectivity(context.Background(), &Request{
		NodeID:    "worker-1",
		VolumeIDs: []string{"nas1/nfs/tank/k8s/pvc-a"},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeHostConnectivity: %v", err)
	}
	if resp.Connected {
		t.Error("a node that cannot reach the appliance is not connected")
	}
	joined := strings.Join(resp.Messages, "; ")
	if !strings.Contains(joined, "nas1") {
		t.Errorf("the message must name the backend, got %q", joined)
	}
	if !strings.Contains(joined, "connection refused") {
		t.Errorf("the message must carry the cause, got %q", joined)
	}
}

// TestPodmonVolumeCheckIsBounded: a hung mount must not hold the answer either.
func TestPodmonVolumeCheckIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	s := healthyService(t)
	s.Statfs = func(string) error { <-release; return nil }

	done := make(chan *Response, 1)
	go func() {
		resp, _ := s.ValidateVolumeHostConnectivity(context.Background(), &Request{
			NodeID: "worker-1", VolumeIDs: []string{"nas1/nfs/tank/k8s/pvc-a"}})
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp.Connected {
			t.Error("a volume whose data path hangs is not connected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the per-volume check must run under its own deadline")
	}
}

// TestPodmonZeroIOsReported: no I/O in the sample window is a distinct answer
// from "unreachable", and the caller acts differently on each.
func TestPodmonZeroIOsReported(t *testing.T) {
	s := healthyService(t)
	s.LastIO = func(string) (time.Time, bool) {
		return time.Now().Add(-time.Hour), true
	}
	resp, err := s.ValidateVolumeHostConnectivity(context.Background(), &Request{
		NodeID: "worker-1", VolumeIDs: []string{"nas1/nfs/tank/k8s/pvc-a"},
		IOSampleWindow: time.Minute})
	if err != nil {
		t.Fatalf("ValidateVolumeHostConnectivity: %v", err)
	}
	if !resp.Connected {
		t.Error("an idle volume is still connected")
	}
	if resp.IOsInProgress {
		t.Error("no I/O in the window must report IOsInProgress false")
	}
}

// TestPodmonServesOnItsOwnListener proves the extension really is reachable
// from another container, on a listener of its own rather than on the CSI
// socket the stalled driver owns.
func TestPodmonServesOnItsOwnListener(t *testing.T) {
	s := healthyService(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = ServeListener(ctx, lis, s) }()

	body, _ := json.Marshal(Request{NodeID: "worker-1",
		VolumeIDs: []string{"nas1/nfs/tank/k8s/pvc-a"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+lis.Addr().String()+ValidatePath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("call the podmon extension: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !out.Connected || out.NodeID != "worker-1" {
		t.Errorf("unexpected response %+v", out)
	}
}
