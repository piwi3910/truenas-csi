package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Graph names accepted by reporting.get_data.
//
// The set is a CLOSED enum on the appliance — anything outside it is rejected by
// middleware validation rather than answered with an empty series — so it is
// spelled out here instead of being passed through as free text. The names come
// from the published schema at
// https://192.168.10.253/api/docs/current/api_methods_reporting.get_data.html;
// the live instance list comes from reporting.netdata_graphs (reporting.graphs
// returns the same payload).
//
// WHAT IS NOT HERE MATTERS MORE THAN WHAT IS. A live 25.10.6 appliance publishes
// exactly 40 graphs: cpu, cputemp, memory, disk (identified by PHYSICAL device —
// sdc, nvme0n1 — 17 of them), interface (per NIC), load, uptime, the ARC and
// L2ARC size/hit/miss counters, disktemp and six ups* graphs. There is NO
// zfs-dataset, zvol or pool I/O graph of any kind.
//
// So reporting.get_data answers appliance-level health — disk throughput, ARC
// efficiency, interface load — and it CANNOT answer per-volume IOPS, bandwidth
// or latency. Per-volume performance comes from node-side kernel counters
// instead; do not come back here looking for it.
const (
	GraphCPU           = "cpu"
	GraphCPUTemp       = "cputemp"
	GraphDisk          = "disk"
	GraphInterface     = "interface"
	GraphLoad          = "load"
	GraphProcesses     = "processes"
	GraphMemory        = "memory"
	GraphUptime        = "uptime"
	GraphARCActualRate = "arcactualrate"
	GraphARCRate       = "arcrate"
	GraphARCSize       = "arcsize"
	GraphARCResult     = "arcresult"
	GraphDiskTemp      = "disktemp"
)

// ReportingQuery is one series requested from reporting.get_data.
type ReportingQuery struct {
	// Name is the graph name, one of the Graph* constants above.
	Name string
	// Identifier selects one instance of the metric — a physical disk device
	// name, a network interface name — and is "" for a single-instance graph.
	Identifier string
}

// ReportingSeries is one graph's answer.
//
// Data is the appliance's rows of [unix_timestamp, value...]. A nil value is a
// GAP: the collector has no sample for that instant. Gaps are normal in the
// middle of a healthy series — a restarted collector, a disk that briefly
// disappeared — so a consumer must skip them rather than read them as zero.
//
// Legend labels the columns of Data. The live appliance omitted it from the
// answer, so it is frequently empty: graph metadata (legend, vertical_label,
// identifiers) properly comes from reporting.netdata_graphs, not from here.
type ReportingSeries struct {
	Name       string
	Identifier string
	Legend     []string
	Data       [][]*float64
}

// ErrNoReportingQueries is returned when ReportingGetData is called with no
// graphs. The appliance requires at least one and rejects the call, so this is
// caught locally rather than spent as a round trip.
var ErrNoReportingQueries = errors.New("reporting.get_data: at least one graph is required")

// ReportingGetData fetches time-series data for the named graphs over a window.
//
// A zero start or end means "let the appliance choose", which it documents as
// its default start and "now" respectively. Times are sent as whole unix
// seconds, the only form the method accepts.
//
// The request form is hardware-verified on 25.10.6:
//
//	[[{"name": "cpu"}], {"start": <unix>, "end": <unix>}]
//
// — a list of graphs and ONE options object, not two positional time arguments.
// Nothing else is sent: unit and page are mutually exclusive with a time range,
// and aggregate is left at its default rather than guessed at.
func (c *Ops) ReportingGetData(ctx context.Context, q []ReportingQuery, start, end time.Time) ([]ReportingSeries, error) {
	if len(q) == 0 {
		return nil, ErrNoReportingQueries
	}
	graphs := make([]map[string]any, 0, len(q))
	for _, g := range q {
		if g.Name == "" {
			return nil, fmt.Errorf("reporting.get_data: graph name is empty")
		}
		item := map[string]any{"name": g.Name}
		// identifier is omitted rather than sent empty: the schema rejects ""
		// as "must be at least 1 characters long", and the verified request for
		// a single-instance graph carries no identifier key at all.
		if g.Identifier != "" {
			item["identifier"] = g.Identifier
		}
		graphs = append(graphs, item)
	}

	// start and end must be strictly positive.
	query := map[string]any{}
	if ts := start.Unix(); !start.IsZero() && ts > 0 {
		query["start"] = ts
	}
	if ts := end.Unix(); !end.IsZero() && ts > 0 {
		query["end"] = ts
	}

	var out []reportingSeries
	if err := c.CallJSON(ctx, &out, "reporting.get_data", graphs, query); err != nil {
		return nil, err
	}
	series := make([]ReportingSeries, 0, len(out))
	for _, s := range out {
		identifier := ""
		if s.Identifier != nil {
			identifier = *s.Identifier
		}
		series = append(series, ReportingSeries{
			Name:       s.Name,
			Identifier: identifier,
			Legend:     s.Legend,
			Data:       s.Data,
		})
	}
	return series, nil
}

// reportingSeries is one element of the reporting.get_data answer, in the
// appliance's own field names. Verified shape:
//
//	{"name": "cpu", "identifier": "cpu", "data": [[1788836682, 23, 22, ...]]}
type reportingSeries struct {
	Name       string        `json:"name"`
	Identifier *string       `json:"identifier"` // null for a single-instance graph
	Legend     []string      `json:"legend"`     // often absent; see ReportingSeries
	Data       reportingRows `json:"data"`
}

// reportingRows decodes the "data" array without assuming a numeric shape.
//
// The same defensiveness sizeField needs applies here: the observed rows carry
// integers, but a rate graph will carry floats and the middleware is not
// consistent about whether a number arrives as a number or as a string. A row
// that is not an array at all is dropped rather than guessed at, because
// inventing a timestamp would corrupt every consumer downstream.
type reportingRows [][]*float64

func (r *reportingRows) UnmarshalJSON(b []byte) error {
	if isJSONNull(b) {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(b, &rows); err != nil {
		// Not a list. Silence here would report "this graph had no samples" for
		// what is really a shape the driver failed to read, so it is an error.
		return fmt.Errorf("reporting data is not a list of rows: %w", err)
	}
	out := make([][]*float64, 0, len(rows))
	for _, raw := range rows {
		var cells []json.RawMessage
		if err := json.Unmarshal(raw, &cells); err != nil {
			continue
		}
		row := make([]*float64, 0, len(cells))
		for _, cell := range cells {
			row = append(row, decodeFloat(cell))
		}
		out = append(out, row)
	}
	*r = out
	return nil
}

// decodeFloat reads one cell, returning nil for a gap.
//
// nil covers JSON null, a non-finite value (a collector may emit "NaN" for an
// unsampled instant) and anything unparseable — all of which mean the same
// thing to a caller: no measurement here. The explicit null test is load
// bearing: encoding/json unmarshals null into a float64 as a NO-OP, leaving the
// zero value behind, which would turn every gap into a confident "0 bytes".
func decodeFloat(raw json.RawMessage) *float64 {
	if isJSONNull(raw) {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return finite(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return finite(v)
		}
	}
	return nil
}

func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func isJSONNull(b []byte) bool { return strings.TrimSpace(string(b)) == "null" }
