// Package fake provides an in-process stand-in for the TrueNAS CORE REST v2
// API, so the CORE client can be exercised without a CORE appliance.
//
// UNVERIFIED. Every shape here is modelled on the DOCUMENTED CORE REST v2
// surface, not recorded from real hardware — no CORE appliance was available.
// It pins the client's own behaviour (routing, auth handling, error mapping,
// job polling) and it is honest about paths and payloads, but it cannot prove
// that a real CORE box answers this way. Treat a green test here as "the client
// is internally consistent", not as "CORE works".
package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// DefaultAPIKey is the bearer token the fake accepts.
const DefaultAPIKey = "1-corekey"

// Options controls how the fake misbehaves, so tests can drive failure paths.
type Options struct {
	// APIKey overrides DefaultAPIKey.
	APIKey string
	// RejectAuth answers every request with 401, as a revoked or wrong key does.
	RejectAuth bool
	// QueryNotFound answers collection GETs with 404 instead of an empty list.
	// CORE is documented to do either depending on the endpoint, and both must
	// mean the same thing to a caller.
	QueryNotFound bool
	// JobPollsBeforeDone keeps a job RUNNING for this many polls (0 = instant).
	JobPollsBeforeDone int
	// JobFails makes the job reach FAILED instead of SUCCESS.
	JobFails bool
}

// Server is a fake TrueNAS CORE REST endpoint.
type Server struct {
	opts Options
	ts   *httptest.Server

	mu       sync.Mutex
	store    map[string][]map[string]any // collection path -> rows
	nextID   int
	jobs     map[int]*job
	requests int
	paths    []string
}

type job struct {
	id        int
	remaining int
	fails     bool
}

// collections are the REST paths the fake serves as CRUD collections. Ordered
// longest-first so "pool/dataset" wins over "pool".
var collections = []string{
	"pool/dataset", "pool/snapshot", "sharing/nfs",
	"iscsi/extent", "iscsi/targetextent", "iscsi/target", "iscsi/portal",
	"iscsi/auth", "iscsi/initiator", "pool",
}

// Start launches a fake and returns it; it is shut down when the test ends.
func Start(t *testing.T, opts Options) *Server {
	t.Helper()
	if opts.APIKey == "" {
		opts.APIKey = DefaultAPIKey
	}
	s := &Server{
		opts:  opts,
		store: map[string][]map[string]any{},
		jobs:  map[int]*job{},
	}
	s.ts = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.ts.Close)
	return s
}

// URL is the https:// base URL to configure as the endpoint.
func (s *Server) URL() string { return s.ts.URL }

// Requests counts every HTTP request that reached the fake.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// PathCount counts requests to one API path, e.g. "core/get_jobs".
func (s *Server) PathCount(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, p := range s.paths {
		if p == path {
			n++
		}
	}
	return n
}

// Add inserts a row into a collection, e.g. Add("pool/dataset", row).
func (s *Server) Add(collection string, row map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store[collection] = append(s.store[collection], row)
}

// SeedParity fills the fake with the objects the parity test expects on both
// flavours: one dataset, one snapshot of it, one NFS export, one pool.
func (s *Server) SeedParity() {
	s.Add("pool/dataset", map[string]any{
		"id": "Pool0/k8s/vol1", "name": "Pool0/k8s/vol1",
		"type": "FILESYSTEM", "mountpoint": "/mnt/Pool0/k8s/vol1",
	})
	s.Add("pool/snapshot", map[string]any{
		"id": "Pool0/k8s/vol1@snap1", "name": "snap1", "dataset": "Pool0/k8s/vol1",
	})
	s.Add("sharing/nfs", map[string]any{
		"id": 1, "path": "/mnt/Pool0/k8s/vol1", "enabled": true,
	})
	s.Add("pool", map[string]any{
		"name": "Pool0", "status": "ONLINE", "healthy": true,
		"size": 72000000000000, "free": 44900000000000,
	})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// EscapedPath, not Path: dataset ids contain "/" and are percent-encoded by
	// the client. Reading the decoded path would split "Pool0/k8s/vol1" into
	// three path segments and address the wrong object.
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/api/v2.0/")
	path = strings.Trim(path, "/")

	s.mu.Lock()
	s.requests++
	s.paths = append(s.paths, path)
	s.mu.Unlock()

	if s.opts.RejectAuth || r.Header.Get("Authorization") != "Bearer "+s.opts.APIKey {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Not authenticated"})
		return
	}

	var body any
	if r.Body != nil {
		defer r.Body.Close()
		if raw, err := io.ReadAll(r.Body); err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case path == "core/get_jobs":
		s.getJobs(w)
		return
	case path == "filesystem/setperm":
		s.setperm(w)
		return
	case path == "pool/dataset/recommended_zvol_blocksize":
		writeJSON(w, http.StatusOK, "128K")
		return
	case path == "pool/snapshot/clone":
		writeJSON(w, http.StatusOK, true)
		return
	case path == "iscsi/global":
		writeJSON(w, http.StatusOK, map[string]any{
			"basename": "iqn.2005-10.org.freenas.ctl", "listen_port": 3260, "alua": false,
		})
		return
	}

	collection, rest, ok := splitCollection(path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "endpoint " + path + " does not exist"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if rest == "" {
			if s.opts.QueryNotFound {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"message": collection + " does not exist"})
				return
			}
			rows := s.store[collection]
			if rows == nil {
				rows = []map[string]any{}
			}
			writeJSON(w, http.StatusOK, rows)
			return
		}
		s.byID(w, collection, rest, func(row map[string]any) { writeJSON(w, http.StatusOK, row) })
	case http.MethodPost:
		s.create(w, collection, body)
	case http.MethodPut:
		s.update(w, collection, rest, body)
	case http.MethodDelete:
		s.delete(w, collection, rest)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"message": "bad method"})
	}
}

// splitCollection matches the longest known collection prefix and returns what
// follows it, which for an item endpoint is "id/<id>".
func splitCollection(path string) (collection, rest string, ok bool) {
	for _, c := range collections {
		if path == c {
			return c, "", true
		}
		if strings.HasPrefix(path, c+"/") {
			return c, strings.TrimPrefix(path, c+"/"), true
		}
	}
	return "", "", false
}

func idFrom(rest string) (string, bool) {
	if !strings.HasPrefix(rest, "id/") {
		return "", false
	}
	id, err := url.PathUnescape(strings.TrimPrefix(rest, "id/"))
	if err != nil {
		return "", false
	}
	return id, true
}

func (s *Server) find(collection, id string) int {
	for i, row := range s.store[collection] {
		if fmt.Sprint(row["id"]) == id || fmt.Sprint(row["name"]) == id {
			return i
		}
	}
	return -1
}

func (s *Server) byID(w http.ResponseWriter, collection, rest string, fn func(map[string]any)) {
	id, ok := idFrom(rest)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": rest + " does not exist"})
		return
	}
	i := s.find(collection, id)
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"message": collection + " " + id + " does not exist"})
		return
	}
	fn(s.store[collection][i])
}

func (s *Server) create(w http.ResponseWriter, collection string, body any) {
	payload, _ := body.(map[string]any)
	if payload == nil {
		payload = map[string]any{}
	}
	row := map[string]any{}
	for k, v := range payload {
		row[k] = v
	}
	switch collection {
	case "pool/dataset":
		name := fmt.Sprint(payload["name"])
		row["id"] = name
		row["mountpoint"] = "/mnt/" + name
		// A dataset is CREATED with user_properties as a list of {key,value}
		// but READ BACK as a map of key -> {value, source}. Modelling only the
		// list would hide the fact that the ownership guard reads the map form,
		// and Owned() would never be exercised.
		stampProperties(row, row["user_properties"])
		delete(row, "user_properties_update")
	case "pool/snapshot":
		ds := fmt.Sprint(payload["dataset"])
		row["id"] = ds + "@" + fmt.Sprint(payload["name"])
	default:
		s.nextID++
		row["id"] = s.nextID
	}
	s.store[collection] = append(s.store[collection], row)
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) update(w http.ResponseWriter, collection, rest string, body any) {
	s.byID(w, collection, rest, func(row map[string]any) {
		if patch, ok := body.(map[string]any); ok {
			for k, v := range patch {
				if k == "user_properties_update" {
					stampProperties(row, v)
					continue
				}
				row[k] = v
			}
		}
		writeJSON(w, http.StatusOK, row)
	})
}

// stampProperties folds a [{key,value}, ...] list into the
// key -> {value, source} map the appliance reports on read.
//
// source is LOCAL because a property set on this dataset is local; the
// driver's delete guard refuses to destroy anything whose marker is merely
// INHERITED from a parent.
func stampProperties(row map[string]any, list any) {
	items, ok := list.([]any)
	if !ok {
		return
	}
	props, ok := row["user_properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
	}
	for _, item := range items {
		kv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		props[fmt.Sprint(kv["key"])] = map[string]any{
			"value": fmt.Sprint(kv["value"]), "source": "LOCAL",
		}
	}
	row["user_properties"] = props
}

func (s *Server) delete(w http.ResponseWriter, collection, rest string) {
	id, ok := idFrom(rest)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": rest + " does not exist"})
		return
	}
	i := s.find(collection, id)
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"message": collection + " " + id + " does not exist"})
		return
	}
	rows := s.store[collection]
	s.store[collection] = append(rows[:i], rows[i+1:]...)
	writeJSON(w, http.StatusOK, true)
}

func (s *Server) setperm(w http.ResponseWriter) {
	s.nextID++
	id := s.nextID
	s.jobs[id] = &job{id: id, remaining: s.opts.JobPollsBeforeDone, fails: s.opts.JobFails}
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) getJobs(w http.ResponseWriter) {
	out := []map[string]any{}
	for _, j := range s.jobs {
		state := "SUCCESS"
		errMsg := ""
		switch {
		case j.remaining > 0:
			j.remaining--
			state = "RUNNING"
		case j.fails:
			state = "FAILED"
			errMsg = "setperm: operation not permitted"
		}
		out = append(out, map[string]any{"id": j.id, "state": state, "error": errMsg})
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
