package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/csi"
	"github.com/pwatteel/truenas-csi/internal/driver"
	"github.com/pwatteel/truenas-csi/internal/obs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// sockPath returns a short socket path. macOS limits sun_path to ~104 bytes and
// t.TempDir() alone exceeds it; the real endpoint
// (/var/lib/kubelet/plugins/.../csi.sock) is comfortably inside the limit.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tncsi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "csi.sock")
}

func dial(t *testing.T, path string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("unix://"+path, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { cc.Close() })
	return cc
}

func TestServerServesIdentityOverUnixSocket(t *testing.T) {
	sock := sockPath(t)
	s, err := New("unix://"+sock, csi.NewIdentity(nil), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	t.Cleanup(func() { cancel() })

	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	resp, err := csipb.NewIdentityClient(dial(t, sock)).
		GetPluginInfo(context.Background(), &csipb.GetPluginInfoRequest{})
	if err != nil {
		t.Fatalf("GetPluginInfo: %v", err)
	}
	if resp.GetName() != driver.DriverName {
		t.Fatalf("plugin name %q, want %q", resp.GetName(), driver.DriverName)
	}
}

// TestServerRemovesStaleSocket: a crashed plugin leaves its socket behind and
// the replacement must still start.
func TestServerRemovesStaleSocket(t *testing.T) {
	sock := sockPath(t)
	if err := os.WriteFile(sock, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New("unix://"+sock, csi.NewIdentity(nil), nil, nil)
	if err != nil {
		t.Fatalf("a leftover socket file must not stop the plugin starting: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	defer cancel()
	if _, err := net.Dial("unix", sock); err != nil {
		t.Fatalf("socket not usable after replacing a stale file: %v", err)
	}
}

// blockingController holds CreateVolume open so shutdown has something to drain.
type blockingController struct {
	csipb.UnimplementedControllerServer
	entered chan struct{}
	release chan struct{}
}

func (b *blockingController) CreateVolume(context.Context, *csipb.CreateVolumeRequest) (*csipb.CreateVolumeResponse, error) {
	close(b.entered)
	<-b.release
	return &csipb.CreateVolumeResponse{Volume: &csipb.Volume{VolumeId: "drained"}}, nil
}

// TestGracefulShutdownDrainsInFlight proves SIGTERM does not cancel work in
// flight. A hard stop mid-CreateVolume leaves a half-built volume behind.
func TestGracefulShutdownDrainsInFlight(t *testing.T) {
	sock := sockPath(t)
	bc := &blockingController{entered: make(chan struct{}), release: make(chan struct{})}
	s, err := New("unix://"+sock, csi.NewIdentity(nil), bc, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()

	client := csipb.NewControllerClient(dial(t, sock))
	callDone := make(chan *csipb.CreateVolumeResponse, 1)
	callErr := make(chan error, 1)
	go func() {
		resp, err := client.CreateVolume(context.Background(), &csipb.CreateVolumeRequest{Name: "v"})
		if err != nil {
			callErr <- err
			return
		}
		callDone <- resp
	}()

	<-bc.entered
	cancel() // shutdown while the call is in flight
	time.Sleep(150 * time.Millisecond)
	close(bc.release)

	select {
	case resp := <-callDone:
		if resp.GetVolume().GetVolumeId() != "drained" {
			t.Fatalf("unexpected response %v", resp)
		}
	case err := <-callErr:
		t.Fatalf("in-flight call was cancelled by shutdown instead of draining: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight call never completed")
	}
	select {
	case <-served:
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

type erroringController struct {
	csipb.UnimplementedControllerServer
	secret string
}

func (e *erroringController) CreateVolume(context.Context, *csipb.CreateVolumeRequest) (*csipb.CreateVolumeResponse, error) {
	return nil, status.Errorf(codes.Internal, "middleware rejected key %s", e.secret)
}

// TestUnaryInterceptorRecordsMetricsAndRedacts: an error must never carry a
// credential back to the caller, where it lands in kubelet logs and events.
func TestUnaryInterceptorRecordsMetricsAndRedacts(t *testing.T) {
	const secret = "8-averysecretapikeyvalue0000000000"
	obs.Register(secret)

	sock := sockPath(t)
	s, err := New("unix://"+sock, csi.NewIdentity(nil), &erroringController{secret: secret}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.Serve(ctx)
	defer cancel()

	_, err = csipb.NewControllerClient(dial(t, sock)).
		CreateVolume(context.Background(), &csipb.CreateVolumeRequest{Name: "v"})
	if err == nil {
		t.Fatal("want an error")
	}
	if contains(err.Error(), secret) {
		t.Fatalf("the api key leaked through a gRPC error: %v", err)
	}
	if !contains(err.Error(), "[redacted]") {
		t.Fatalf("error should show the redaction marker: %v", err)
	}
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) || status.Code(err) != codes.Internal {
		t.Fatalf("status code should survive redaction, got %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
