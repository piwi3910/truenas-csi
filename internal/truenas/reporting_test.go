package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/piwi3910/truenas-csi/internal/truenas/fake"
)

// fmtRow renders a decoded row so a mismatch names the gap rather than a
// pointer address.
func fmtRow(row []*float64) string {
	out := "["
	for i, v := range row {
		if i > 0 {
			out += " "
		}
		if v == nil {
			out += "gap"
		} else {
			out += formatFloat(*v)
		}
	}
	return out + "]"
}

func formatFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestReportingGetDataDecodesRows pins the shape a consumer depends on: rows of
// [timestamp, value...] where a missing sample stays MISSING. Turning a gap into
// 0 would be indistinguishable from "this volume did no I/O", which is exactly
// the reading a fencing decision must not get wrong.
func TestReportingGetDataDecodesRows(t *testing.T) {
	tests := []struct {
		name string
		data [][]any
		want []string
	}{
		{
			name: "plain numbers",
			data: [][]any{{1700000000, 1.5, 2.5}, {1700000010, 3.0, 4.0}},
			want: []string{"[1700000000 1.5 2.5]", "[1700000010 3 4]"},
		},
		{
			name: "a gap in the middle of a healthy series",
			data: [][]any{{1700000000, 1.5}, {1700000010, nil}, {1700000020, 2.5}},
			want: []string{"[1700000000 1.5]", "[1700000010 gap]", "[1700000020 2.5]"},
		},
		{
			name: "numbers arriving as strings",
			data: [][]any{{"1700000000", "1.5"}},
			want: []string{"[1700000000 1.5]"},
		},
		{
			name: "an unparseable cell reads as a gap, not as zero",
			data: [][]any{{1700000000, "NaN"}},
			want: []string{"[1700000000 gap]"},
		},
		{
			name: "an empty series",
			data: [][]any{},
			want: []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			s.SeedReportingData(fake.ReportingSeries{
				Name: GraphDisk, Identifier: "sda",
				Legend: []string{"time", "read", "write"},
				Data:   tc.data,
			})
			c := dialFake(t, s)

			got, err := c.ReportingGetData(context.Background(),
				[]ReportingQuery{{Name: GraphDisk, Identifier: "sda"}},
				time.Unix(1700000000, 0), time.Unix(1700000020, 0))
			if err != nil {
				t.Fatalf("ReportingGetData: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 series, got %d", len(got))
			}
			if got[0].Name != GraphDisk || got[0].Identifier != "sda" {
				t.Fatalf("wrong series identity: %+v", got[0])
			}
			if len(got[0].Data) != len(tc.want) {
				t.Fatalf("want %d rows, got %d (%v)", len(tc.want), len(got[0].Data), got[0].Data)
			}
			for i, want := range tc.want {
				if fmtRow(got[0].Data[i]) != want {
					t.Errorf("row %d: want %s, got %s", i, want, fmtRow(got[0].Data[i]))
				}
			}
		})
	}
}

// TestReportingGetDataNullIdentifierIsEmpty covers an appliance-wide graph,
// whose identifier comes back as JSON null rather than absent.
func TestReportingGetDataNullIdentifierIsEmpty(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.SeedReportingData(fake.ReportingSeries{
		Name: GraphCPU, Identifier: nil, Legend: []string{"time", "user"},
		Data: [][]any{{1700000000, 12.0}},
	})
	c := dialFake(t, s)

	got, err := c.ReportingGetData(context.Background(),
		[]ReportingQuery{{Name: GraphCPU}}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ReportingGetData: %v", err)
	}
	if got[0].Identifier != "" {
		t.Fatalf("want empty identifier for an appliance-wide graph, got %q", got[0].Identifier)
	}
}

// TestReportingGetDataSendsTheDocumentedParameters checks the wire form, which
// is the half a fake cannot catch by accident: the appliance rejects an empty
// identifier string and rejects a time range combined with unit/page.
func TestReportingGetDataSendsTheDocumentedParameters(t *testing.T) {
	tests := []struct {
		name       string
		queries    []ReportingQuery
		start, end time.Time
		wantGraphs string
		wantQuery  string
	}{
		{
			name:       "an identified graph over an explicit window",
			queries:    []ReportingQuery{{Name: GraphDisk, Identifier: "sda"}},
			start:      time.Unix(1700000000, 0),
			end:        time.Unix(1700003600, 0),
			wantGraphs: `[{"identifier":"sda","name":"disk"}]`,
			wantQuery:  `{"end":1700003600,"start":1700000000}`,
		},
		{
			// The verified request for a single-instance graph is exactly
			// {"name": "cpu"} — no identifier key at all.
			name:       "a single-instance graph omits the identifier",
			queries:    []ReportingQuery{{Name: GraphCPU}},
			wantGraphs: `[{"name":"cpu"}]`,
			wantQuery:  `{}`,
		},
		{
			name: "several graphs in one call",
			queries: []ReportingQuery{
				{Name: GraphDisk, Identifier: "sda"},
				{Name: GraphInterface, Identifier: "eth0"},
			},
			wantGraphs: `[{"identifier":"sda","name":"disk"},{"identifier":"eth0","name":"interface"}]`,
			wantQuery:  `{}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := fake.Start(t, fake.Options{})
			var graphs, query string
			s.Handle("reporting.get_data", func(p []json.RawMessage) (any, error) {
				if len(p) > 0 {
					graphs = string(p[0])
				}
				if len(p) > 1 {
					query = string(p[1])
				}
				return []any{}, nil
			})
			c := dialFake(t, s)

			if _, err := c.ReportingGetData(context.Background(), tc.queries, tc.start, tc.end); err != nil {
				t.Fatalf("ReportingGetData: %v", err)
			}
			if graphs != tc.wantGraphs {
				t.Errorf("graphs: want %s, got %s", tc.wantGraphs, graphs)
			}
			if query != tc.wantQuery {
				t.Errorf("query: want %s, got %s", tc.wantQuery, query)
			}
		})
	}
}

// TestReportingGetDataRejectsAnEmptyRequest keeps a call the appliance would
// refuse off the wire; the schema requires at least one graph.
func TestReportingGetDataRejectsAnEmptyRequest(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.SeedReportingData()
	c := dialFake(t, s)

	if _, err := c.ReportingGetData(context.Background(), nil, time.Time{}, time.Time{}); !errors.Is(err, ErrNoReportingQueries) {
		t.Fatalf("want ErrNoReportingQueries, got %v", err)
	}
	if n := s.CallsTo("reporting.get_data"); n != 0 {
		t.Fatalf("the call must not reach the appliance, saw %d", n)
	}
}

// TestReportingGetDataRefusesAnUnreadableShape is the anti-silence test. A
// "data" field this driver cannot read must be an error: answering with an empty
// series would tell a caller the volume was idle.
func TestReportingGetDataRefusesAnUnreadableShape(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("reporting.get_data", []any{map[string]any{
		"name": GraphDisk, "identifier": "sda",
		"legend": []string{"time"},
		"data":   map[string]any{"unexpected": true},
	}})
	c := dialFake(t, s)

	if _, err := c.ReportingGetData(context.Background(),
		[]ReportingQuery{{Name: GraphDisk, Identifier: "sda"}},
		time.Time{}, time.Time{}); err == nil {
		t.Fatal("want an error for an unreadable data shape, got nil")
	}
}

// TestReportingGetDataNullDataIsEmpty separates "no samples" from "unreadable":
// an explicit null is the appliance saying it has nothing, which is not a fault.
func TestReportingGetDataNullDataIsEmpty(t *testing.T) {
	s := fake.Start(t, fake.Options{})
	s.HandleValue("reporting.get_data", []any{map[string]any{
		"name": GraphDisk, "identifier": "sda", "legend": []string{"time"}, "data": nil,
	}})
	c := dialFake(t, s)

	got, err := c.ReportingGetData(context.Background(),
		[]ReportingQuery{{Name: GraphDisk, Identifier: "sda"}}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("ReportingGetData: %v", err)
	}
	if len(got) != 1 || len(got[0].Data) != 0 {
		t.Fatalf("want one empty series, got %+v", got)
	}
}
