package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// request is one middleware method expressed as a REST v2 call.
type request struct {
	verb string
	path string
	body any
	// filters are the JSON-RPC query filters, applied to the response rather
	// than sent to the appliance. See applyFilters.
	filters [][]any
}

// route maps a JSON-RPC method name onto CORE's REST v2 surface.
//
// CORE mirrors the method namespace as a path: pool.dataset -> pool/dataset,
// sharing.nfs -> sharing/nfs, iscsi.targetextent -> iscsi/targetextent. The last
// segment names the operation and decides the verb; an item is addressed as
// <collection>/id/<id>.
//
// UNVERIFIED: this mapping follows the documented convention, but no CORE
// appliance was available to confirm it call by call.
func route(method string, params []any) (request, error) {
	segments := strings.Split(method, ".")
	if len(segments) < 2 {
		return request{}, fmt.Errorf("core: cannot route method %q", method)
	}
	collection := strings.Join(segments[:len(segments)-1], "/")
	op := segments[len(segments)-1]

	// core.get_jobs is a read, and the driver's only job-polling call. It takes
	// filters like a query even though its name does not end in ".query".
	if method == "core.get_jobs" {
		return request{verb: http.MethodGet, path: "core/get_jobs", filters: filtersOf(params)}, nil
	}

	switch op {
	case "query":
		return request{verb: http.MethodGet, path: collection, filters: filtersOf(params)}, nil

	case "config":
		// iscsi.global.config -> GET iscsi/global: a singleton, not a collection.
		return request{verb: http.MethodGet, path: collection}, nil

	case "create":
		return request{verb: http.MethodPost, path: collection, body: first(params)}, nil

	case "update":
		id, rest, err := idAndRest(method, params)
		if err != nil {
			return request{}, err
		}
		return request{verb: http.MethodPut, path: itemPath(collection, id), body: first(rest)}, nil

	case "delete":
		id, rest, err := idAndRest(method, params)
		if err != nil {
			return request{}, err
		}
		// The trailing booleans SCALE passes positionally (recursive, force) go
		// in the body, which is where CORE documents delete options.
		return request{verb: http.MethodDelete, path: itemPath(collection, id), body: first(rest)}, nil

	case "get_instance":
		id, _, err := idAndRest(method, params)
		if err != nil {
			return request{}, err
		}
		return request{verb: http.MethodGet, path: itemPath(collection, id)}, nil
	}

	// Everything else is a plain action mirrored at its full method path:
	// filesystem.setperm, pool.snapshot.clone,
	// pool.dataset.recommended_zvol_blocksize.
	return request{
		verb: http.MethodPost,
		path: strings.Join(segments, "/"),
		body: first(params),
	}, nil
}

// itemPath addresses one object. The id is escaped because dataset ids contain
// "/" and snapshot ids contain "@" — leaving them raw would silently address a
// different, possibly existing, object.
func itemPath(collection, id string) string {
	return collection + "/id/" + url.PathEscape(id)
}

func first(params []any) any {
	if len(params) == 0 {
		return nil
	}
	return params[0]
}

func idAndRest(method string, params []any) (string, []any, error) {
	if len(params) == 0 {
		return "", nil, fmt.Errorf("core: %s needs an id", method)
	}
	return fmt.Sprint(params[0]), params[1:], nil
}

// filtersOf reads the [[field, op, value], ...] filter list SCALE takes as the
// first query parameter.
func filtersOf(params []any) [][]any {
	if len(params) == 0 {
		return nil
	}
	raw, ok := params[0].([]any)
	if !ok {
		return nil
	}
	out := make([][]any, 0, len(raw))
	for _, f := range raw {
		if triple, ok := f.([]any); ok && len(triple) == 3 {
			out = append(out, triple)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyFilters narrows a result set CLIENT-SIDE.
//
// CORE's query-parameter filtering is not uniform across endpoints, and a filter
// silently ignored by the appliance would hand the caller the wrong object —
// DatasetQuery would answer with somebody else's dataset. Filtering here makes
// the answer correct regardless of what the appliance honoured. The cost is
// transferring the whole collection, which is acceptable for the collection
// sizes a CSI driver touches and is the safe side of the trade.
func applyFilters(raw json.RawMessage, filters [][]any) json.RawMessage {
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		return raw // not a list: nothing to filter
	}
	kept := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if matchesAll(item, filters) {
			kept = append(kept, item)
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return raw
	}
	return out
}

func matchesAll(item map[string]any, filters [][]any) bool {
	for _, f := range filters {
		field, op, want := fmt.Sprint(f[0]), fmt.Sprint(f[1]), fmt.Sprint(f[2])
		got := fmt.Sprint(item[field])
		switch op {
		case "=", "==":
			if got != want {
				return false
			}
		case "!=":
			if got == want {
				return false
			}
		case "^":
			if !strings.HasPrefix(got, want) {
				return false
			}
		default:
			// An operator this client does not implement must not silently widen
			// the result set into a wrong answer.
			return false
		}
	}
	return true
}
