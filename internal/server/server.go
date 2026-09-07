// Package server serves the CSI gRPC services over a UNIX socket.
package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	csipb "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pwatteel/truenas-csi/internal/obs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DrainTimeout bounds how long a graceful shutdown waits for in-flight calls.
const DrainTimeout = 30 * time.Second

// Server serves whichever CSI services it was given.
type Server struct {
	endpoint string
	grpc     *grpc.Server
	lis      net.Listener
}

// New builds a server. Any of the three services may be nil.
func New(endpoint string, id csipb.IdentityServer, ctrl csipb.ControllerServer, nd csipb.NodeServer) (*Server, error) {
	path := strings.TrimPrefix(endpoint, "unix://")
	if path == "" {
		return nil, fmt.Errorf("endpoint must be a unix socket path, got %q", endpoint)
	}
	// A crashed process leaves its socket behind; without this the next start
	// fails with "address already in use" and the plugin never recovers.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	g := grpc.NewServer(grpc.UnaryInterceptor(interceptor))
	if id != nil {
		csipb.RegisterIdentityServer(g, id)
	}
	if ctrl != nil {
		csipb.RegisterControllerServer(g, ctrl)
	}
	if nd != nil {
		csipb.RegisterNodeServer(g, nd)
	}
	return &Server{endpoint: path, grpc: g, lis: lis}, nil
}

// interceptor times every call, records it, and makes sure no credential can
// escape inside an error message.
func interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	start := time.Now()
	method := info.FullMethod
	if i := strings.LastIndex(method, "/"); i >= 0 {
		method = method[i+1:]
	}
	resp, err := h(ctx, req)
	obs.ObserveCSI(method, err, time.Since(start))
	if err != nil {
		if s, ok := status.FromError(err); ok {
			return resp, status.Error(s.Code(), obs.Redact(s.Message()))
		}
		return resp, status.Error(codes.Internal, obs.Redact(err.Error()))
	}
	return resp, nil
}

// Serve blocks until the context is cancelled, then drains.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.grpc.Serve(s.lis) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return s.Stop(context.Background())
	}
}

// Stop drains in-flight calls, bounded by DrainTimeout.
//
// A hard stop would cancel a CreateVolume mid-flight and leave a half-built
// volume on the appliance, so the drain is the default and the hard stop only
// the fallback.
func (s *Server) Stop(context.Context) error {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(DrainTimeout):
		s.grpc.Stop()
	}
	_ = os.Remove(s.endpoint)
	return nil
}

// Addr is the socket path being served.
func (s *Server) Addr() string { return s.endpoint }
