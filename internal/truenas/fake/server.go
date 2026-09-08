// Package fake provides an in-process stand-in for the TrueNAS middleware,
// speaking JSON-RPC 2.0 over a TLS websocket exactly as the appliance does.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

// Options controls how the fake misbehaves, so tests can drive the failure paths.
type Options struct {
	// RejectAuth makes auth.login_ex answer AUTH_ERR.
	RejectAuth bool
	// AuthResponseType overrides the response_type when set (e.g. "EXPIRED").
	AuthResponseType string
	// DropAfter closes the connection after this many method calls (0 = never).
	DropAfter int
	// ConcurrencyLimit rejects calls beyond this many in flight with -32000,
	// mirroring the appliance's real ceiling of 20.
	ConcurrencyLimit int
}

// Handler answers one method. Returning an *RPCError produces a JSON-RPC error.
type Handler func(params []json.RawMessage) (any, error)

// RPCError is a JSON-RPC error the fake should return.
type RPCError struct {
	Code    int
	ErrName string
	Reason  string
}

func (e *RPCError) Error() string { return e.Reason }

// Server is a fake middleware endpoint.
type Server struct {
	opts Options
	ts   *httptest.Server

	mu       sync.Mutex
	handlers map[string]Handler
	calls    []string
	conns    []func(any)

	inFlight  atomic.Int64
	peak      atomic.Int64
	callCount atomic.Int64
	authCount atomic.Int64
}

// Start launches a fake and returns it; it is shut down when the test ends.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()
	s := &Server{opts: opts, handlers: map[string]Handler{}}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.ts.Close)
	return s
}

// URL is the wss:// endpoint to dial.
func (s *Server) URL() string {
	return "wss" + strings.TrimPrefix(s.ts.URL, "https")
}

// Handle registers a handler for a method, replacing any previous one.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// HandleValue registers a handler that always returns v.
func (s *Server) HandleValue(method string, v any) {
	s.Handle(method, func([]json.RawMessage) (any, error) { return v, nil })
}

// ISCSISession is one entry of an iscsi.global.sessions answer, named with the
// appliance's own field semantics so a test reads like the box's output.
type ISCSISession struct {
	Initiator     string
	InitiatorAddr string
	Target        string
	TargetAlias   string
}

// SeedISCSISessions makes iscsi.global.sessions answer with these sessions.
//
// The payload is built here, once, rather than in each test: the wire field
// names (initiator_addr, target_alias) are the part a test cannot get wrong
// without silently proving nothing.
func (s *Server) SeedISCSISessions(sessions ...ISCSISession) {
	out := make([]any, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, map[string]any{
			"initiator":       sess.Initiator,
			"initiator_addr":  sess.InitiatorAddr,
			"initiator_alias": nil,
			"target":          sess.Target,
			"target_alias":    sess.TargetAlias,
			"immediate_data":  true,
			"iser":            false,
			"offload":         false,
		})
	}
	s.HandleValue("iscsi.global.sessions", out)
}

// NFSv4Client is one entry of an nfs.get_nfs4_clients answer.
type NFSv4Client struct {
	// Address is "ip:port", exactly as /proc/fs/nfsd/clients writes it.
	Address string
	// Status is the NFSv4 client state — "confirmed", "courtesy" or
	// "expirable". Empty means confirmed.
	Status string
	// RenewAgeSeconds is the "seconds from last renew" lease age.
	RenewAgeSeconds int
}

// SeedNFSClients makes both NFS client listings answer. v3 takes bare addresses,
// which is all rmtab records.
//
// The v4 payload reproduces the appliance's own keys, SPACES INCLUDED —
// "seconds from last renew" is the shape a decoder has to get right, so a test
// that invented a tidier key would prove nothing.
func (s *Server) SeedNFSClients(v3 []string, v4 []NFSv4Client) {
	out3 := make([]any, 0, len(v3))
	for _, ip := range v3 {
		out3 = append(out3, map[string]any{"ip": ip, "export": "/mnt/Pool0/k8s/vol1"})
	}
	s.HandleValue("nfs.get_nfs3_clients", out3)

	out4 := make([]any, 0, len(v4))
	for i, client := range v4 {
		status := client.Status
		if status == "" {
			status = "confirmed"
		}
		out4 = append(out4, map[string]any{
			"id": fmt.Sprint(i + 1),
			"info": map[string]any{
				"clientid":                5618032175184784444,
				"address":                 client.Address,
				"status":                  status,
				"seconds from last renew": client.RenewAgeSeconds,
				"name":                    "Linux NFSv4.2 node",
				"minor version":           2,
				"callback state":          "UP",
				"admin-revoked states":    0,
			},
			"states": []any{},
		})
	}
	s.HandleValue("nfs.get_nfs4_clients", out4)
}

// SeedClientCounts makes the two bare-integer health calls answer.
func (s *Server) SeedClientCounts(iscsi, nfs int) {
	s.HandleValue("iscsi.global.client_count", iscsi)
	s.HandleValue("nfs.client_count", nfs)
}

// ReportingSeries is one element of a reporting.get_data answer, whose verified
// shape is {"name", "identifier", "data"}. Data is [][]any rather than
// [][]float64 so a test can inject the JSON nulls that represent gaps, which is
// the shape a consumer must survive.
type ReportingSeries struct {
	Name       string
	Identifier any // string, or nil for an appliance-wide graph
	Legend     []string
	Data       [][]any
}

// SeedReportingData makes reporting.get_data answer with these series.
func (s *Server) SeedReportingData(series ...ReportingSeries) {
	out := make([]any, 0, len(series))
	for _, ser := range series {
		rows := make([]any, 0, len(ser.Data))
		for _, row := range ser.Data {
			rows = append(rows, row)
		}
		out = append(out, map[string]any{
			"name":       ser.Name,
			"identifier": ser.Identifier,
			"legend":     ser.Legend,
			"data":       rows,
		})
	}
	s.HandleValue("reporting.get_data", out)
}

// Calls returns the methods called so far, in order.
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// CallsTo counts how many times a method was called.
func (s *Server) CallsTo(method string) int {
	n := 0
	for _, c := range s.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

// AuthCount is how many auth.login_ex calls arrived, across all connections.
func (s *Server) AuthCount() int { return int(s.authCount.Load()) }

// PeakConcurrency is the highest number of simultaneously in-flight calls seen.
func (s *Server) PeakConcurrency() int { return int(s.peak.Load()) }

var upgrader = websocket.Upgrader{}

type request struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      *json.RawMessage  `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()

	var writeMu sync.Mutex
	send := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = c.WriteJSON(v)
	}

	s.mu.Lock()
	s.conns = append(s.conns, send)
	s.mu.Unlock()

	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		var req request
		if err := json.Unmarshal(data, &req); err != nil {
			continue
		}
		if s.opts.DropAfter > 0 && int(s.callCount.Load()) >= s.opts.DropAfter {
			s.callCount.Store(0)
			return // simulate the appliance dropping the connection
		}
		go s.dispatch(req, send)
	}
}

func (s *Server) dispatch(req request, send func(any)) {
	cur := s.inFlight.Add(1)
	for {
		old := s.peak.Load()
		if cur <= old || s.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	// The in-flight window must close BEFORE the response is written. If it
	// closed after, the client could receive the reply, release its own
	// semaphore slot and issue a new call that this server counts while the
	// finished one is still counted — inflating the observed peak by one and
	// making a correct client look like it breached its cap.
	var done sync.Once
	finish := func(v any) {
		done.Do(func() { s.inFlight.Add(-1) })
		send(v)
	}
	defer done.Do(func() { s.inFlight.Add(-1) })

	if s.opts.ConcurrencyLimit > 0 && int(cur) > s.opts.ConcurrencyLimit {
		finish(errResponse(req.ID, -32000, "", "too many concurrent calls"))
		return
	}

	s.mu.Lock()
	s.calls = append(s.calls, req.Method)
	h := s.handlers[req.Method]
	s.mu.Unlock()

	if req.Method != "auth.login_ex" {
		s.callCount.Add(1)
	}

	if req.Method == "auth.login_ex" {
		s.authCount.Add(1)
		rt := "SUCCESS"
		if s.opts.AuthResponseType != "" {
			rt = s.opts.AuthResponseType
		} else if s.opts.RejectAuth {
			rt = "AUTH_ERR"
		}
		finish(okResponse(req.ID, map[string]any{"response_type": rt}))
		return
	}

	if h == nil {
		finish(errResponse(req.ID, -32601, "", "method not found"))
		return
	}
	v, err := h(req.Params)
	if err != nil {
		var re *RPCError
		if e, ok := err.(*RPCError); ok {
			re = e
		} else {
			re = &RPCError{Code: -32001, ErrName: "EFAULT", Reason: err.Error()}
		}
		finish(errResponse(req.ID, re.Code, re.ErrName, re.Reason))
		return
	}
	finish(okResponse(req.ID, v))
}

// Notify pushes an id-less JSON-RPC notification to every open connection,
// as the appliance does for collection_update job progress messages.
func (s *Server) Notify(method string, params any) {
	s.mu.Lock()
	conns := append([]func(any){}, s.conns...)
	s.mu.Unlock()
	for _, send := range conns {
		send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	}
}

func okResponse(id *json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func errResponse(id *json.RawMessage, code int, errname, reason string) map[string]any {
	e := map[string]any{"code": code, "message": "Method call error"}
	if errname != "" || reason != "" {
		e["data"] = map[string]any{"errname": errname, "reason": reason, "error": 0}
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": e}
}
