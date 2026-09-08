// Package podmon serves this driver's ValidateVolumeHostConnectivity
// extension: "can this node still reach the appliance, and has any I/O been
// seen on these volumes lately?"
//
// # This is a driver extension, not CSI
//
// The CSI specification has no ValidateVolumeHostConnectivity call. Dell's CSM
// for Resiliency defines one in its own proto and its podmon sidecar calls it;
// this package answers the same question with the same shape of answer, but as
// this driver's own service. It is deliberately proto-free: a JSON request and
// a JSON response over HTTP on a listener of the driver's own, so that a
// sidecar, an operator or a curl(1) can ask without generated stubs. Nothing
// here is registered on the CSI socket, and no CO will ever call it.
//
// # Why it does not share anything with the driver
//
// The entire value of an independent connectivity checker is that it survives
// the driver stalling. A checker that reaches its answer through the driver's
// middleware client, its goroutine pool or its CSI socket is worth nothing at
// the one moment it is needed: when those are wedged, it wedges with them.
//
// So this service:
//
//   - listens on its own socket or port, served by its own http.Server, and
//     never on the CSI socket;
//   - runs its own bounded probes (a TCP dial to the appliance's data address, a
//     statfs on each volume's path) with its own deadlines, never a call into
//     the driver's client;
//   - treats any optional probe of the driver itself as advisory only: it is run
//     under its own timeout, concurrently, and a stalled driver contributes a
//     message to the answer instead of withholding the answer.
//
// TestPodmonAnswersWhileDriverStalled pins that property with a driver fake that
// never responds.
package podmon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"
)

// ValidatePath is the HTTP path of the extension's single RPC.
const ValidatePath = "/podmon/v1/validate-volume-host-connectivity"

// DefaultTimeout bounds one probe. It is short on purpose: a caller asking
// whether a node is still connected wants an answer now, and "did not answer
// within a couple of seconds" is itself the answer.
const DefaultTimeout = 2 * time.Second

// DefaultIOSampleWindow is how far back "I/O has been observed" looks when the
// request names no window.
const DefaultIOSampleWindow = 60 * time.Second

// VolumeRef is what the service needs to know about one volume on this node.
type VolumeRef struct {
	VolumeID string
	Protocol string
	// Path is the staging or published path to stat. Empty for a raw block
	// volume, which is judged by the appliance probe alone.
	Path string
}

// Request is the extension's request message.
type Request struct {
	// NodeID is the node the caller believes it is asking about. A mismatch is
	// reported in Messages rather than rejected: the caller's view of node
	// naming is not this driver's to police.
	NodeID string `json:"nodeId"`
	// VolumeIDs is the optional list of volumes to check. Empty means "just
	// tell me about the node".
	VolumeIDs []string `json:"volumeIds,omitempty"`
	// IOSampleWindow is how recent I/O must be to count as in progress.
	IOSampleWindow time.Duration `json:"ioSampleWindow,omitempty"`
}

// Response is the extension's reply message.
type Response struct {
	NodeID string `json:"nodeId"`
	// Connected is true when the node reached the appliance and every named
	// volume's data path answered.
	Connected bool `json:"connected"`
	// IOsInProgress is true when I/O was observed on at least one named volume
	// within the sample window.
	IOsInProgress bool `json:"iosInProgress"`
	// Messages explains anything that made Connected false, and anything the
	// service could not determine.
	Messages []string `json:"messages,omitempty"`
}

// Service answers the extension's RPC.
type Service struct {
	// NodeID is this node's name.
	NodeID string
	// Backend and DataAddr name the appliance and its data address (host:port).
	Backend  string
	DataAddr string
	// Timeout bounds each individual probe.
	Timeout time.Duration

	// Dial probes the appliance data address.
	Dial func(ctx context.Context, addr string) error
	// Statfs probes a mounted volume path.
	Statfs func(path string) error
	// Volumes resolves a volume id to its path on this node. It must not block:
	// it reads the node plugin's own in-memory target set.
	Volumes func(volumeID string) (VolumeRef, bool)
	// LastIO reports when I/O was last observed on a path.
	LastIO func(path string) (time.Time, bool)
	// DriverProbe optionally checks whether the CSI driver itself is
	// responsive. It is advisory: its result colours Messages, never the
	// answer's availability. See the package comment.
	DriverProbe func(ctx context.Context) error
	// Now is the clock, overridable in tests.
	Now func() time.Time
}

// New builds a service with the production probes. Volumes and LastIO are left
// nil; the caller wires them to the node plugin's target set.
func New(nodeID, backend, dataAddr string) *Service {
	return &Service{
		NodeID:   nodeID,
		Backend:  backend,
		DataAddr: dataAddr,
		Timeout:  DefaultTimeout,
		Dial:     dialProbe,
		Statfs:   statfsProbe,
		LastIO:   lastIOProbe,
		Now:      time.Now,
	}
}

func dialProbe(ctx context.Context, addr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

func statfsProbe(path string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return err
	}
	if st.Blocks == 0 {
		return errors.New("filesystem reports no blocks")
	}
	return nil
}

// lastIOProbe uses the mount point's own modification time as the cheapest
// available evidence of recent activity. It is deliberately coarse: a pod
// rewriting one file in place does not move it, so "no I/O observed" means
// exactly what it says — no evidence, not proof of idleness — and the caller
// weighs it alongside Connected rather than acting on it alone.
// defaultIOCounters backs the production LastIO probe.
var defaultIOCounters = newIOCounters("/host")

// lastIOProbe reports when the volume at path last moved bytes.
//
// It reads kernel I/O counters rather than the mount point's mtime. The mtime
// looks like a reasonable proxy and is not one: verified against a real NFS
// mount, writing 4 MiB to an existing file left the directory's mtime
// unchanged, because a directory's mtime tracks entries appearing and
// disappearing, not writes to files already in it. Every steady writer — a
// database being the obvious one — would have reported no I/O at all.
func lastIOProbe(path string) (time.Time, bool) {
	return defaultIOCounters.Active(path)
}

// ValidateVolumeHostConnectivity answers the extension's RPC. Every probe it
// runs is bounded, and it never waits on the driver.
func (s *Service) ValidateVolumeHostConnectivity(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, errors.New("nil request")
	}
	resp := &Response{NodeID: s.NodeID, Connected: true}
	if req.NodeID != "" && s.NodeID != "" && req.NodeID != s.NodeID {
		resp.Messages = append(resp.Messages,
			fmt.Sprintf("this node is %s, not %s", s.NodeID, req.NodeID))
	}

	// The advisory driver probe runs concurrently with our own checks and is
	// collected with whatever time is left. A driver that never answers costs
	// one line in Messages, not the whole response.
	driverDone := make(chan error, 1)
	if s.DriverProbe != nil {
		probeCtx, cancel := context.WithTimeout(ctx, s.timeout())
		defer cancel()
		go func() { driverDone <- s.DriverProbe(probeCtx) }()
	}

	if s.DataAddr != "" {
		if err := s.bounded(ctx, func(c context.Context) error {
			return s.dial(c, s.DataAddr)
		}); err != nil {
			resp.Connected = false
			resp.Messages = append(resp.Messages,
				fmt.Sprintf("backend %s at %s is unreachable from this node: %v",
					s.Backend, s.DataAddr, err))
		}
	}

	window := req.IOSampleWindow
	if window <= 0 {
		window = DefaultIOSampleWindow
	}

	for _, id := range req.VolumeIDs {
		ref, ok := VolumeRef{VolumeID: id}, false
		if s.Volumes != nil {
			ref, ok = s.Volumes(id)
		}
		if !ok {
			// Not staged here. That is not a connectivity failure — it is the
			// caller asking the wrong node — so it is reported, not counted.
			resp.Messages = append(resp.Messages,
				fmt.Sprintf("volume %s is not staged on this node", id))
			continue
		}
		if ref.Path == "" {
			continue
		}
		if err := s.bounded(ctx, func(context.Context) error {
			return s.statfs(ref.Path)
		}); err != nil {
			resp.Connected = false
			resp.Messages = append(resp.Messages,
				fmt.Sprintf("volume %s data path at %s did not answer: %v", id, ref.Path, err))
			continue
		}
		if s.LastIO == nil {
			continue
		}
		if last, ok := s.LastIO(ref.Path); ok && s.now().Sub(last) <= window {
			resp.IOsInProgress = true
		}
	}

	if s.DriverProbe != nil {
		select {
		case err := <-driverDone:
			if err != nil {
				resp.Messages = append(resp.Messages,
					fmt.Sprintf("the csi driver did not answer its own probe: %v", err))
			}
		case <-time.After(s.timeout()):
			resp.Messages = append(resp.Messages,
				"the csi driver did not answer within "+s.timeout().String()+
					"; this report was produced without it")
		}
	}
	return resp, nil
}

// bounded runs one probe under its own deadline, in its own goroutine, and
// returns whichever arrives first. The buffered channel lets a hung probe's
// goroutine finish and be collected whenever the kernel finally releases it.
func (s *Service) bounded(ctx context.Context, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timed out after %s", s.timeout())
	}
}

func (s *Service) dial(ctx context.Context, addr string) error {
	if s.Dial == nil {
		return dialProbe(ctx, addr)
	}
	return s.Dial(ctx, addr)
}

func (s *Service) statfs(path string) error {
	if s.Statfs == nil {
		return statfsProbe(path)
	}
	return s.Statfs(path)
}

func (s *Service) timeout() time.Duration {
	if s.Timeout <= 0 {
		return DefaultTimeout
	}
	return s.Timeout
}

func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Handler serves the extension over HTTP. It is a plain mux with one route, so
// nothing else on the process' listeners can be reached through it.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(ValidatePath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST a ValidateVolumeHostConnectivity request", http.StatusMethodNotAllowed)
			return
		}
		var req Request
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, "malformed request", http.StatusBadRequest)
			return
		}
		resp, err := s.ValidateVolumeHostConnectivity(r.Context(), &req)
		if err != nil {
			http.Error(w, "connectivity check failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

// Serve listens on addr and serves the extension until ctx is cancelled.
//
// addr is either "unix:///path/to.sock" or a TCP address. A UNIX socket is the
// safer default — it is reachable only by a container sharing the volume — and
// a TCP address should be bound to localhost unless the operator has a reason
// to expose it, which is why the chart binds 127.0.0.1 by default.
func Serve(ctx context.Context, addr string, s *Service) error {
	network, address := "tcp", addr
	if path, ok := strings.CutPrefix(addr, "unix://"); ok {
		network, address = "unix", path
		// A crashed process leaves its socket behind and the next start would
		// fail with "address already in use" forever.
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale podmon socket %s: %w", address, err)
		}
	}
	lis, err := net.Listen(network, address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ServeListener(ctx, lis, s)
}

// ServeListener serves the extension on an existing listener.
func ServeListener(ctx context.Context, lis net.Listener, s *Service) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		// Close, not Shutdown: this endpoint's whole point is not to wait on
		// anything, including its own in-flight requests, at shutdown.
		_ = srv.Close()
	}()
	if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
